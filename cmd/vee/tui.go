package main

import (
	"fmt"
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Custom bubbletea messages

type paneOutputMsg struct{}

type paneExitMsg struct {
	paneID string
	err    error
}

type paneCreatedMsg struct {
	pane *Pane
}

type paneCreateFailedMsg struct {
	err error
}

// model is the bubbletea model for the Vee TUI multiplexer.
type model struct {
	app           *App
	projectConfig string
	startCmd      *StartCmd
	passthrough   []string
	program       *tea.Program

	panes       []*Pane
	activeIdx   int
	commandMode bool
	showingLogs bool
	logBuffer   *ringBuffer

	// Overlay state
	showOverlay   bool // session list overlay (Ctrl-b w)
	showHelp      bool // help overlay (Ctrl-b ?)
	showModePick  bool // mode picker overlay (Ctrl-b c)
	modePickIdx   int  // cursor in mode picker
	overlayScroll int  // scroll for log viewer

	width  int
	height int

	quitting bool
}

func newModel(app *App, projectConfig string, cmd *StartCmd, passthrough []string, logBuf *ringBuffer) *model {
	return &model{
		app:           app,
		projectConfig: projectConfig,
		startCmd:      cmd,
		passthrough:   passthrough,
		logBuffer:     logBuf,
	}
}

func (m *model) Init() tea.Cmd {
	return nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		for _, p := range m.panes {
			p.resize(m.width, m.height)
		}
		return m, nil

	case paneOutputMsg:
		return m, nil

	case paneExitMsg:
		for i, p := range m.panes {
			if p.SessionID == msg.paneID {
				slog.Debug("pane exited", "pane", p.ID, "mode", p.Mode.Name, "err", msg.err)
				// Mark session status
				sess := m.app.Sessions.get(p.SessionID)
				if sess != nil && sess.Status == "active" {
					m.app.Sessions.setStatus(p.SessionID, "completed")
				}
				// Remove dead pane from the list
				m.panes = append(m.panes[:i], m.panes[i+1:]...)
				m.app.Control.clearSessionFor(p.SessionID)
				if m.activeIdx >= len(m.panes) && m.activeIdx > 0 {
					m.activeIdx = len(m.panes) - 1
				}
				break
			}
		}
		return m, nil

	case paneCreatedMsg:
		m.panes = append(m.panes, msg.pane)
		m.activeIdx = len(m.panes) - 1
		m.showingLogs = false
		m.watchPane(msg.pane)
		return m, nil

	case paneCreateFailedMsg:
		slog.Error("failed to create pane", "error", msg.err)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showModePick {
		return m.handleModePickKey(msg)
	}
	if m.showOverlay {
		return m.handleOverlayKey(msg)
	}
	if m.showHelp {
		return m.handleHelpKey(msg)
	}
	if m.showingLogs {
		return m.handleLogViewerKey(msg)
	}

	if m.commandMode {
		m.commandMode = false
		return m.handleCommandKey(msg)
	}

	// Relay mode: check for prefix key
	if msg.String() == "ctrl+b" {
		m.commandMode = true
		return m, nil
	}

	// Forward raw key to active pane
	if len(m.panes) > 0 && m.activeIdx < len(m.panes) {
		p := m.panes[m.activeIdx]
		if p.isAlive() {
			raw := keyToBytes(msg)
			if len(raw) > 0 {
				p.writeInput(raw)
			}
		}
	}

	return m, nil
}

func (m *model) handleCommandKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	keyStr := msg.String()

	switch keyStr {
	case "ctrl+b":
		if len(m.panes) > 0 && m.activeIdx < len(m.panes) {
			m.panes[m.activeIdx].writeInput([]byte{0x02})
		}
		return m, nil

	case "c":
		m.showModePick = true
		m.modePickIdx = 0
		return m, nil

	case "n":
		if len(m.panes) > 0 {
			m.showingLogs = false
			m.activeIdx = (m.activeIdx + 1) % len(m.panes)
		}
		return m, nil

	case "p":
		if len(m.panes) > 0 {
			m.showingLogs = false
			m.activeIdx = (m.activeIdx - 1 + len(m.panes)) % len(m.panes)
		}
		return m, nil

	case "w":
		m.showOverlay = true
		return m, nil

	case "l":
		m.showingLogs = true
		m.overlayScroll = 0
		return m, nil

	case "x":
		if len(m.panes) > 0 && m.activeIdx < len(m.panes) {
			p := m.panes[m.activeIdx]
			go func() {
				p.close()
			}()
			m.app.Sessions.drop(p.SessionID)
			m.app.Control.clearSessionFor(p.SessionID)
			m.panes = append(m.panes[:m.activeIdx], m.panes[m.activeIdx+1:]...)
			if m.activeIdx >= len(m.panes) && m.activeIdx > 0 {
				m.activeIdx = len(m.panes) - 1
			}
		}
		return m, nil

	case "d":
		if len(m.panes) > 0 && m.activeIdx < len(m.panes) {
			p := m.panes[m.activeIdx]
			if p.isAlive() {
				m.app.Control.requestSuspendFor(p.SessionID)
			}
		}
		return m, nil

	case "?":
		m.showHelp = true
		return m, nil

	case "q":
		m.quitting = true
		for _, p := range m.panes {
			go func(p *Pane) { p.close() }(p)
		}
		return m, tea.Quit

	default:
		if len(keyStr) == 1 && keyStr[0] >= '0' && keyStr[0] <= '9' {
			idx := int(keyStr[0] - '0')
			if idx < len(m.panes) {
				m.showingLogs = false
				m.activeIdx = idx
			}
		}
		return m, nil
	}
}

func (m *model) handleModePickKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.modePickIdx > 0 {
			m.modePickIdx--
		}
	case "down", "j":
		if m.modePickIdx < len(modeOrder)-1 {
			m.modePickIdx++
		}
	case "enter":
		name := modeOrder[m.modePickIdx]
		mode, ok := modeRegistry[name]
		if !ok {
			m.showModePick = false
			return m, nil
		}
		if mode.NeedsMCP && !m.startCmd.Zettelkasten {
			return m, nil
		}
		m.showModePick = false
		return m, m.createPaneCmd(mode, "")
	case "esc", "q":
		m.showModePick = false
	}
	return m, nil
}

func (m *model) handleOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "w":
		m.showOverlay = false
	default:
		keyStr := msg.String()
		if len(keyStr) == 1 && keyStr[0] >= '0' && keyStr[0] <= '9' {
			idx := int(keyStr[0] - '0')
			if idx < len(m.panes) {
				m.showingLogs = false
				m.activeIdx = idx
				m.showOverlay = false
			}
		}
	}
	return m, nil
}

func (m *model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "?":
		m.showHelp = false
	}
	return m, nil
}

func (m *model) handleLogViewerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	keyStr := msg.String()

	if keyStr == "ctrl+b" {
		m.commandMode = true
		return m, nil
	}

	switch keyStr {
	case "up", "k":
		if m.overlayScroll > 0 {
			m.overlayScroll--
		}
	case "down", "j":
		m.overlayScroll++
	case "G":
		m.overlayScroll = len(m.logBuffer.Lines())
	case "g":
		m.overlayScroll = 0
	case "esc", "q":
		m.showingLogs = false
	}
	return m, nil
}

func (m *model) View() string {
	if m.quitting {
		return ""
	}

	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	var content string

	if m.showModePick {
		content = m.renderModePicker()
	} else if m.showOverlay {
		content = m.renderSessionList()
	} else if m.showHelp {
		content = m.renderHelp()
	} else if m.showingLogs {
		content = m.renderLogViewer()
	} else if len(m.panes) > 0 && m.activeIdx < len(m.panes) {
		content = m.panes[m.activeIdx].render()
	} else {
		content = m.renderIdle()
	}

	contentHeight := m.height - 1
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > contentHeight {
		lines = lines[:contentHeight]
	}
	for len(lines) < contentHeight {
		lines = append(lines, "")
	}
	for i, line := range lines {
		if lipgloss.Width(line) > m.width {
			lines[i] = ansi.Truncate(line, m.width, "")
		}
	}
	content = strings.Join(lines, "\n")

	statusBar := renderStatusBar(m.panes, m.activeIdx, m.showingLogs, m.commandMode, m.width)

	return content + "\n" + statusBar
}

// --- Overlay renderers ---

func (m *model) renderModePicker() string {
	var sb strings.Builder
	sb.WriteString("\n  Select a mode:\n\n")
	for i, name := range modeOrder {
		mode, ok := modeRegistry[name]
		if !ok {
			continue
		}
		cursor := "  "
		if i == m.modePickIdx {
			cursor = "> "
		}
		disabled := ""
		if mode.NeedsMCP && !m.startCmd.Zettelkasten {
			disabled = " (requires -z)"
		}
		sb.WriteString(fmt.Sprintf("  %s%s %s  %s%s\n", cursor, mode.Indicator, mode.Name, mode.Description, disabled))
	}
	sb.WriteString("\n  Enter to select, Esc to cancel\n")
	return sb.String()
}

func (m *model) renderSessionList() string {
	var sb strings.Builder
	sb.WriteString("\n  Session List:\n\n")
	for i, p := range m.panes {
		marker := "  "
		if !m.showingLogs && i == m.activeIdx {
			marker = "* "
		}
		status := "running"
		if !p.isAlive() {
			status = "dead"
		}
		sb.WriteString(fmt.Sprintf("  %s%d: %s %s [%s]\n", marker, i, p.Mode.Indicator, p.Mode.Name, status))
	}
	sb.WriteString("\n  Press 0-9 to switch, Esc to close\n")
	return sb.String()
}

func (m *model) renderHelp() string {
	return `
  Vee Multiplexer — Key Bindings

  Ctrl-b is the prefix key. Press Ctrl-b then:

    c       Create a new session (mode picker)
    n       Next session
    p       Previous session
    0-9     Switch to session by index
    w       Show session list
    l       Log viewer
    d       Suspend current session
    x       Drop (kill) current session
    q       Quit vee
    ?       This help
    Ctrl-b  Send literal Ctrl-b to session

  Press Esc to close this help.
`
}

func (m *model) renderLogViewer() string {
	lines := m.logBuffer.Lines()
	viewHeight := m.height - 1

	if len(lines) == 0 {
		return "\n  (no log entries)"
	}

	maxScroll := len(lines) - viewHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.overlayScroll > maxScroll {
		m.overlayScroll = maxScroll
	}

	start := m.overlayScroll
	end := start + viewHeight
	if end > len(lines) {
		end = len(lines)
	}

	visible := lines[start:end]
	return strings.Join(visible, "\n")
}

func (m *model) renderIdle() string {
	return `
  Vee — Modal Code Assistant

  No active sessions. Press Ctrl-b c to create one.
  Press Ctrl-b ? for help.
`
}

// --- Pane management ---

// createPaneCmd returns a bubbletea Cmd that spawns a new pane asynchronously.
func (m *model) createPaneCmd(mode Mode, initialMsg string) tea.Cmd {
	// Capture dimensions at call time (we're in the Update goroutine here)
	width, height := m.width, m.height
	if width < 1 {
		width = 80
	}
	if height < 2 {
		height = 24
	}

	return func() tea.Msg {
		id := newUUID()

		preview := initialMsg
		if len(preview) > 80 {
			preview = preview[:80] + "…"
		}

		m.app.Sessions.create(id, mode.Name, mode.Indicator, preview)
		m.app.Control.newSessionFor(id)

		args := buildSessionArgs(id, false, mode, m.projectConfig, m.startCmd, m.passthrough)
		if initialMsg != "" {
			args = append([]string{initialMsg}, args...)
		}

		slog.Debug("creating pane", "mode", mode.Name, "session", id, "args", args)

		prog := m.program
		pane, err := newPane(id, mode, id, preview, args, width, height, func() {
			if prog != nil {
				prog.Send(paneOutputMsg{})
			}
		})
		if err != nil {
			m.app.Sessions.drop(id)
			m.app.Control.clearSessionFor(id)
			return paneCreateFailedMsg{err: err}
		}

		return paneCreatedMsg{pane: pane}
	}
}

// watchPane starts goroutines to watch for pane exit and suspend/self-drop signals.
func (m *model) watchPane(pane *Pane) {
	prog := m.program

	// Watch for process exit
	go func() {
		<-pane.done()
		if prog != nil {
			prog.Send(paneExitMsg{paneID: pane.SessionID, err: pane.exitErr})
		}
	}()

	// Watch for suspend/self-drop signals
	go func() {
		suspendCh, selfDropCh := m.app.Control.channelsFor(pane.SessionID)
		if suspendCh == nil {
			return
		}
		select {
		case <-suspendCh:
			slog.Debug("suspend signal received", "session", pane.SessionID)
			pane.close()
			m.app.Sessions.setStatus(pane.SessionID, "suspended")
			if prog != nil {
				prog.Send(paneExitMsg{paneID: pane.SessionID, err: nil})
			}
		case <-selfDropCh:
			slog.Debug("self-drop signal received", "session", pane.SessionID)
			pane.close()
			m.app.Sessions.setStatus(pane.SessionID, "completed")
			if prog != nil {
				prog.Send(paneExitMsg{paneID: pane.SessionID, err: nil})
			}
		case <-pane.done():
			// Process already exited naturally, nothing to do
		}
	}()
}

// keyToBytes converts a bubbletea key message to raw bytes for PTY forwarding.
func keyToBytes(msg tea.KeyMsg) []byte {
	// For control keys, the Type directly encodes the byte value
	if msg.Type >= 0 && msg.Type <= 31 {
		return []byte{byte(msg.Type)}
	}
	if msg.Type == tea.KeyBackspace {
		return []byte{0x7f}
	}

	switch msg.Type {
	case tea.KeyRunes:
		s := string(msg.Runes)
		if msg.Alt {
			return append([]byte{0x1b}, []byte(s)...)
		}
		return []byte(s)
	case tea.KeySpace:
		if msg.Alt {
			return []byte{0x1b, ' '}
		}
		return []byte{' '}
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyInsert:
		return []byte("\x1b[2~")
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeyShiftUp:
		return []byte("\x1b[1;2A")
	case tea.KeyShiftDown:
		return []byte("\x1b[1;2B")
	case tea.KeyShiftRight:
		return []byte("\x1b[1;2C")
	case tea.KeyShiftLeft:
		return []byte("\x1b[1;2D")
	case tea.KeyCtrlUp:
		return []byte("\x1b[1;5A")
	case tea.KeyCtrlDown:
		return []byte("\x1b[1;5B")
	case tea.KeyCtrlRight:
		return []byte("\x1b[1;5C")
	case tea.KeyCtrlLeft:
		return []byte("\x1b[1;5D")
	case tea.KeyCtrlHome:
		return []byte("\x1b[1;5H")
	case tea.KeyCtrlEnd:
		return []byte("\x1b[1;5F")
	case tea.KeyF1:
		return []byte("\x1bOP")
	case tea.KeyF2:
		return []byte("\x1bOQ")
	case tea.KeyF3:
		return []byte("\x1bOR")
	case tea.KeyF4:
		return []byte("\x1bOS")
	case tea.KeyF5:
		return []byte("\x1b[15~")
	case tea.KeyF6:
		return []byte("\x1b[17~")
	case tea.KeyF7:
		return []byte("\x1b[18~")
	case tea.KeyF8:
		return []byte("\x1b[19~")
	case tea.KeyF9:
		return []byte("\x1b[20~")
	case tea.KeyF10:
		return []byte("\x1b[21~")
	case tea.KeyF11:
		return []byte("\x1b[23~")
	case tea.KeyF12:
		return []byte("\x1b[24~")
	default:
		s := msg.String()
		if s != "" {
			return []byte(s)
		}
		return nil
	}
}
