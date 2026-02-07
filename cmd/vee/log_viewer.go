package main

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// LogViewerCmd is the internal subcommand that shows an interactive
// log viewer with syntax highlighting, rendered inside a tmux display-popup.
type LogViewerCmd struct {
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Styles for slog syntax highlighting (Tokyo Night palette)
var (
	logTimeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#565f89"))
	logDebugStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#7aa2f7"))
	logInfoStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#a6e3a1"))
	logWarnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#f9e2af"))
	logErrorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e"))
	logMsgStyle     = lipgloss.NewStyle().Bold(true)
	logKeyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#565f89"))
	logMatchStyle   = lipgloss.NewStyle().Background(lipgloss.Color("#414868")).Foreground(lipgloss.Color("#c0caf5"))
	logStatusStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7aa2f7"))
	logPausedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f9e2af"))
	logHelpStyle    = lipgloss.NewStyle().Faint(true)
	logSearchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#bb9af7"))
)

// Regex patterns for slog text format parsing
var (
	slogTimeRe  = regexp.MustCompile(`^(time=\S+)`)
	slogLevelRe = regexp.MustCompile(`\b(level=)(DEBUG|INFO|WARN|ERROR)\b`)
	slogMsgRe   = regexp.MustCompile(`\b(msg=)("(?:[^"\\]|\\.)*"|[^\s]+)`)
	slogKVRe    = regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)=`)
)

const maxLogLines = 5000

type logModel struct {
	lines     []string
	scroll    int
	hscroll   int // horizontal scroll offset
	following bool
	filter    string
	searching bool
	matches   []int
	matchIdx  int
	width     int
	height    int

	file       *os.File
	lastSize   int64
	socketName string
}

type tickMsg time.Time

type fileOpenedMsg struct {
	file     *os.File
	lines    []string
	lastSize int64
	err      error
}

type newLinesMsg struct {
	lines    []string
	lastSize int64
}

func initialLogModel(socketName string) logModel {
	return logModel{
		lines:      make([]string, 0, maxLogLines),
		following:  true,
		socketName: socketName,
		width:      80,
		height:     24,
	}
}

func (m logModel) Init() tea.Cmd {
	return openLogFile(m.socketName)
}

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func openLogFile(socketName string) tea.Cmd {
	return func() tea.Msg {
		tmuxSocketName = socketName
		path := logFilePath()

		f, err := os.Open(path)
		if err != nil {
			return fileOpenedMsg{err: err}
		}

		// Read last portion of file
		info, err := f.Stat()
		if err == nil && info.Size() > 64*1024 {
			f.Seek(-64*1024, 2)
			// Skip partial line
			scanner := bufio.NewScanner(f)
			if scanner.Scan() {
				// Discard first partial line
			}
		}

		var lines []string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
			if len(lines) > maxLogLines {
				lines = lines[1:]
			}
		}

		lastSize, _ := f.Seek(0, 1) // Current position
		return fileOpenedMsg{file: f, lines: lines, lastSize: lastSize}
	}
}

func readNewLines(f *os.File, lastSize int64) tea.Cmd {
	return func() tea.Msg {
		if f == nil {
			return nil
		}

		info, err := f.Stat()
		if err != nil {
			return nil
		}

		if info.Size() <= lastSize {
			return nil
		}

		var lines []string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		newSize, _ := f.Seek(0, 1)

		if len(lines) == 0 {
			return nil
		}
		return newLinesMsg{lines: lines, lastSize: newSize}
	}
}

func (m logModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.following {
			m.scrollToBottom()
		}
		return m, nil

	case fileOpenedMsg:
		if msg.err != nil {
			m.lines = append(m.lines, "[error opening log file: "+msg.err.Error()+"]")
			return m, nil
		}
		m.file = msg.file
		m.lastSize = msg.lastSize
		m.lines = append(m.lines, msg.lines...)
		if len(m.lines) > maxLogLines {
			m.lines = m.lines[len(m.lines)-maxLogLines:]
		}
		if m.following {
			m.scrollToBottom()
		}
		return m, tickCmd()

	case tickMsg:
		return m, tea.Batch(readNewLines(m.file, m.lastSize), tickCmd())

	case newLinesMsg:
		if len(msg.lines) > 0 {
			m.lines = append(m.lines, msg.lines...)
			m.lastSize = msg.lastSize
			if len(m.lines) > maxLogLines {
				m.lines = m.lines[len(m.lines)-maxLogLines:]
			}
			if m.following {
				m.scrollToBottom()
			}
			if m.filter != "" {
				m.updateMatches()
			}
		}
		return m, nil

	case tea.KeyMsg:
		if m.searching {
			return m.handleSearchInput(msg)
		}
		return m.handleNormalInput(msg)
	}

	return m, nil
}

func (m *logModel) handleSearchInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searching = false
		m.filter = ""
		m.matches = nil
		m.matchIdx = 0
	case tea.KeyEnter:
		m.searching = false
		if m.filter != "" {
			m.updateMatches()
			if len(m.matches) > 0 {
				m.jumpToMatch(0)
			}
		}
	case tea.KeyBackspace:
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
		}
	case tea.KeyCtrlU:
		m.filter = ""
	case tea.KeyCtrlW:
		// Delete last word
		i := len(m.filter)
		for i > 0 && m.filter[i-1] == ' ' {
			i--
		}
		for i > 0 && m.filter[i-1] != ' ' {
			i--
		}
		m.filter = m.filter[:i]
	default:
		if msg.Type == tea.KeyRunes {
			m.filter += string(msg.Runes)
		}
	}
	return m, nil
}

func (m *logModel) handleNormalInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	viewHeight := m.viewableHeight()

	switch msg.Type {
	case tea.KeyEsc:
		if m.filter != "" {
			m.filter = ""
			m.matches = nil
			m.matchIdx = 0
			return m, nil
		}
		m.closeFile()
		return m, tea.Quit

	case tea.KeyCtrlC:
		m.closeFile()
		return m, tea.Quit

	case tea.KeyUp:
		m.scroll--
		m.following = false
		m.clampScroll()

	case tea.KeyDown:
		m.scroll++
		m.clampScroll()
		if m.scroll >= m.maxScroll() {
			m.following = true
		}

	case tea.KeyPgUp:
		m.scroll -= viewHeight
		m.following = false
		m.clampScroll()

	case tea.KeyPgDown:
		m.scroll += viewHeight
		m.clampScroll()
		if m.scroll >= m.maxScroll() {
			m.following = true
		}

	case tea.KeyLeft:
		m.hscroll -= 10
		if m.hscroll < 0 {
			m.hscroll = 0
		}

	case tea.KeyRight:
		m.hscroll += 10

	case tea.KeyHome:
		m.hscroll = 0

	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "q":
			m.closeFile()
			return m, tea.Quit
		case "g":
			m.scroll = 0
			m.following = false
		case "G":
			m.scrollToBottom()
			m.following = true
		case "f":
			m.following = !m.following
			if m.following {
				m.scrollToBottom()
			}
		case "/":
			m.searching = true
			m.filter = ""
		case "n":
			if len(m.matches) > 0 {
				m.matchIdx = (m.matchIdx + 1) % len(m.matches)
				m.jumpToMatch(m.matchIdx)
			}
		case "N":
			if len(m.matches) > 0 {
				m.matchIdx--
				if m.matchIdx < 0 {
					m.matchIdx = len(m.matches) - 1
				}
				m.jumpToMatch(m.matchIdx)
			}
		case "0":
			m.hscroll = 0
		}
	}

	return m, nil
}

func (m *logModel) closeFile() {
	if m.file != nil {
		m.file.Close()
		m.file = nil
	}
}

func (m logModel) viewableHeight() int {
	// Reserve: header (1) + footer (1)
	h := m.height - 2
	if h < 1 {
		h = 1
	}
	return h
}

func (m logModel) maxScroll() int {
	max := len(m.lines) - m.viewableHeight()
	if max < 0 {
		return 0
	}
	return max
}

func (m *logModel) clampScroll() {
	if m.scroll < 0 {
		m.scroll = 0
	}
	max := m.maxScroll()
	if m.scroll > max {
		m.scroll = max
	}
}

func (m *logModel) scrollToBottom() {
	m.scroll = m.maxScroll()
}

func (m *logModel) updateMatches() {
	m.matches = nil
	if m.filter == "" {
		return
	}
	lower := strings.ToLower(m.filter)
	for i, line := range m.lines {
		if strings.Contains(strings.ToLower(line), lower) {
			m.matches = append(m.matches, i)
		}
	}
}

func (m *logModel) jumpToMatch(idx int) {
	if idx < 0 || idx >= len(m.matches) {
		return
	}
	lineIdx := m.matches[idx]
	m.scroll = lineIdx - m.viewableHeight()/2
	m.following = false
	m.clampScroll()
}

func (m logModel) View() string {
	var b strings.Builder

	viewHeight := m.viewableHeight()

	// Header: status line
	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	// Log lines
	end := m.scroll + viewHeight
	if end > len(m.lines) {
		end = len(m.lines)
	}
	start := m.scroll
	if start < 0 {
		start = 0
	}

	for i := start; i < end; i++ {
		line := m.renderLogLine(m.lines[i], i)
		// Apply horizontal scroll and truncate to width (ANSI-aware)
		if m.hscroll > 0 {
			line = sliceAnsi(line, m.hscroll, m.width)
		} else {
			line = truncateAnsi(line, m.width)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	// Pad remaining lines
	for i := end - start; i < viewHeight; i++ {
		b.WriteString("\n")
	}

	// Footer
	b.WriteString(m.renderFooter())

	return b.String()
}

func (m logModel) renderHeader() string {
	var status string
	if m.following {
		status = logStatusStyle.Render("◉ following")
	} else {
		status = logPausedStyle.Render("○ paused")
	}

	lineInfo := logHelpStyle.Render(
		" " + formatLineRange(m.scroll+1, min(m.scroll+m.viewableHeight(), len(m.lines)), len(m.lines)),
	)

	// Horizontal scroll indicator
	var hscrollInfo string
	if m.hscroll > 0 {
		hscrollInfo = logHelpStyle.Render(" +" + strconv.Itoa(m.hscroll) + "→")
	}

	// Search info
	var searchInfo string
	if m.filter != "" && len(m.matches) > 0 {
		searchInfo = logSearchStyle.Render(" [" + m.filter + "] ") +
			logHelpStyle.Render(formatMatchInfo(m.matchIdx+1, len(m.matches)))
	} else if m.filter != "" {
		searchInfo = logSearchStyle.Render(" [" + m.filter + "] ") +
			logHelpStyle.Render("no matches")
	}

	return " " + status + searchInfo + lineInfo + hscrollInfo
}

func (m logModel) renderFooter() string {
	if m.searching {
		return " " + logSearchStyle.Render("/") + m.filter + logHelpStyle.Render("▏")
	}

	help := "↑↓ scroll  ←→ pan  g/G top/end  f follow  / search  q quit"
	if m.filter != "" {
		help = "n/N match  Esc clear  " + help
	}
	return " " + logHelpStyle.Render(help)
}

func (m logModel) renderLogLine(line string, lineIdx int) string {
	// Check if this line matches current search
	isMatch := false
	if m.filter != "" {
		for _, idx := range m.matches {
			if idx == lineIdx {
				isMatch = true
				break
			}
		}
	}

	// Highlight current match
	if isMatch && len(m.matches) > 0 && m.matchIdx < len(m.matches) && m.matches[m.matchIdx] == lineIdx {
		return logMatchStyle.Render(highlightSlog(line, m.filter))
	}

	return highlightSlog(line, m.filter)
}

// highlightSlog applies syntax highlighting to a slog-formatted log line.
func highlightSlog(line, filter string) string {
	// If empty or doesn't look like slog, return as-is
	if !strings.HasPrefix(line, "time=") {
		if filter != "" {
			return highlightFilter(line, filter)
		}
		return line
	}

	var result strings.Builder
	remaining := line

	// Parse time=...
	if loc := slogTimeRe.FindStringIndex(remaining); loc != nil {
		result.WriteString(logTimeStyle.Render(remaining[loc[0]:loc[1]]))
		remaining = remaining[loc[1]:]
	}

	// Process the rest token by token
	for len(remaining) > 0 {
		// Skip leading spaces
		if remaining[0] == ' ' {
			result.WriteByte(' ')
			remaining = remaining[1:]
			continue
		}

		// Try level=...
		if loc := slogLevelRe.FindStringSubmatchIndex(remaining); loc != nil && loc[0] == 0 {
			key := remaining[loc[2]:loc[3]]   // "level="
			level := remaining[loc[4]:loc[5]] // DEBUG|INFO|WARN|ERROR

			result.WriteString(logKeyStyle.Render(key))
			switch level {
			case "DEBUG":
				result.WriteString(logDebugStyle.Render(level))
			case "INFO":
				result.WriteString(logInfoStyle.Render(level))
			case "WARN":
				result.WriteString(logWarnStyle.Render(level))
			case "ERROR":
				result.WriteString(logErrorStyle.Render(level))
			}
			remaining = remaining[loc[1]:]
			continue
		}

		// Try msg=...
		if loc := slogMsgRe.FindStringSubmatchIndex(remaining); loc != nil && loc[0] == 0 {
			key := remaining[loc[2]:loc[3]] // "msg="
			val := remaining[loc[4]:loc[5]] // the message value

			result.WriteString(logKeyStyle.Render(key))
			result.WriteString(logMsgStyle.Render(val))
			remaining = remaining[loc[1]:]
			continue
		}

		// Try generic key=value
		if loc := slogKVRe.FindStringIndex(remaining); loc != nil && loc[0] == 0 {
			key := remaining[loc[0]:loc[1]]
			result.WriteString(logKeyStyle.Render(key))
			remaining = remaining[loc[1]:]

			// Extract value (until space or end)
			valEnd := strings.IndexByte(remaining, ' ')
			if valEnd == -1 {
				valEnd = len(remaining)
			}
			// Handle quoted values
			if len(remaining) > 0 && remaining[0] == '"' {
				// Find closing quote
				for i := 1; i < len(remaining); i++ {
					if remaining[i] == '"' && (i == 0 || remaining[i-1] != '\\') {
						valEnd = i + 1
						break
					}
				}
			}
			result.WriteString(remaining[:valEnd])
			remaining = remaining[valEnd:]
			continue
		}

		// No pattern matched, copy one character
		result.WriteByte(remaining[0])
		remaining = remaining[1:]
	}

	highlighted := result.String()
	if filter != "" {
		return highlightFilter(highlighted, filter)
	}
	return highlighted
}

// highlightFilter underlines filter matches in the line.
func highlightFilter(line, filter string) string {
	if filter == "" {
		return line
	}

	lower := strings.ToLower(line)
	filterLower := strings.ToLower(filter)

	var result strings.Builder
	lastEnd := 0

	for {
		idx := strings.Index(lower[lastEnd:], filterLower)
		if idx == -1 {
			result.WriteString(line[lastEnd:])
			break
		}

		start := lastEnd + idx
		end := start + len(filter)

		result.WriteString(line[lastEnd:start])
		result.WriteString(lipgloss.NewStyle().Underline(true).Render(line[start:end]))
		lastEnd = end
	}

	return result.String()
}

func formatLineRange(start, end, total int) string {
	if total == 0 {
		return "0 lines"
	}
	if start == end {
		return formatInt(start) + "/" + formatInt(total)
	}
	return formatInt(start) + "-" + formatInt(end) + "/" + formatInt(total)
}

func formatMatchInfo(current, total int) string {
	return formatInt(current) + "/" + formatInt(total) + " matches"
}

func formatInt(n int) string {
	return strconv.Itoa(n)
}

// sliceAnsi extracts a visible substring from a line containing ANSI codes.
// It skips `start` visible characters, then returns up to `width` visible characters.
// Crucially, it tracks active ANSI codes while skipping and prepends them to maintain formatting.
func sliceAnsi(line string, start, width int) string {
	runes := []rune(line)
	var result strings.Builder
	var activeStyles strings.Builder // Track ANSI codes encountered while skipping
	visible := 0
	i := 0

	// Skip `start` visible characters, but track ANSI codes
	for i < len(runes) && visible < start {
		if runes[i] == '\033' {
			// Capture escape sequence
			seqStart := i
			i++
			for i < len(runes) {
				c := runes[i]
				i++
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
			}
			seq := string(runes[seqStart:i])
			// Check if it's a reset - if so, clear active styles
			if seq == "\033[0m" || seq == "\033[m" {
				activeStyles.Reset()
			} else {
				activeStyles.WriteString(seq)
			}
			continue
		}
		visible++
		i++
	}

	// Prepend active styles to maintain formatting
	if activeStyles.Len() > 0 {
		result.WriteString(activeStyles.String())
	}

	// Now collect up to `width` visible characters
	collected := 0
	for i < len(runes) && collected < width {
		if runes[i] == '\033' {
			// Include escape sequence in output
			seqStart := i
			i++
			for i < len(runes) {
				c := runes[i]
				i++
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
			}
			result.WriteString(string(runes[seqStart:i]))
			continue
		}
		result.WriteRune(runes[i])
		collected++
		i++
	}

	// Reset formatting at the end
	if result.Len() > 0 {
		result.WriteString("\033[0m")
	}
	return result.String()
}

// truncateAnsi truncates a line to fit within maxWidth visible characters,
// skipping ANSI escape sequences when counting width.
func truncateAnsi(line string, maxWidth int) string {
	visible := 0
	i := 0
	runes := []rune(line)
	for i < len(runes) {
		if runes[i] == '\033' {
			// Skip entire escape sequence: ESC [ ... letter
			i++
			for i < len(runes) {
				c := runes[i]
				i++
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
			}
			continue
		}
		if visible >= maxWidth {
			return string(runes[:i]) + "\033[0m"
		}
		visible++
		i++
	}
	return line
}

func (cmd *LogViewerCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket

	m := initialLogModel(cmd.TmuxSocket)
	p := tea.NewProgram(m, tea.WithAltScreen())

	_, err := p.Run()
	return err
}
