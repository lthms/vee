package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PromptViewerCmd is the internal subcommand that displays the system prompt
// for the session in the current tmux window, rendered inside a tmux display-popup.
type PromptViewerCmd struct {
	Port     int    `short:"p" default:"2700" name:"port"`
	WindowID string `required:"" name:"window-id"`
}

// promptResult holds the fetched prompt data or an error message.
type promptResult struct {
	profile      string
	indicator    string
	systemPrompt string
	errorMsg     string
}

// promptViewerModel is the Bubble Tea model for the prompt viewer.
type promptViewerModel struct {
	viewport   viewport.Model
	profile    string
	indicator  string
	rawContent string
	lines      []promptLine // pre-processed lines with metadata
	ready      bool
	errorMsg   string
	width      int
	height     int

	// Search state
	searching bool
	filter    string
	matches   []int // line indices that match
	matchIdx  int   // current match index
}

// Styles
var (
	pvTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#89b4fa"))

	pvHelpStyle = lipgloss.NewStyle().
			Faint(true)

	pvErrorStyle = lipgloss.NewStyle().
			Faint(true)

	pvSearchStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#bb9af7"))

	pvMatchStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#414868")).
			Foreground(lipgloss.Color("#c0caf5"))
)

func initialPromptViewerModel(result promptResult) promptViewerModel {
	return promptViewerModel{
		profile:    result.profile,
		indicator:  result.indicator,
		rawContent: result.systemPrompt,
		errorMsg:   result.errorMsg,
		ready:      false,
	}
}

func (m promptViewerModel) Init() tea.Cmd {
	return nil
}

func (m promptViewerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		if m.errorMsg != "" {
			return m, nil
		}

		headerHeight := 2 // title + divider
		footerHeight := 1 // help line
		viewportHeight := m.height - headerHeight - footerHeight
		if viewportHeight < 1 {
			viewportHeight = 1
		}

		if !m.ready {
			m.viewport = viewport.New(m.width, viewportHeight)
			m.viewport.YPosition = headerHeight
			m.ready = true
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = viewportHeight
		}

		// Pre-process lines on first ready
		if m.ready && len(m.lines) == 0 && m.rawContent != "" {
			m.lines = preparePromptLines(m.rawContent, m.width-2)
			m.refreshContent()
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

func (m promptViewerModel) handleSearchInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searching = false
		m.filter = ""
		m.matches = nil
		m.matchIdx = 0
		m.refreshContent()
	case tea.KeyEnter:
		m.searching = false
		if m.filter != "" {
			m.updateMatches()
			m.refreshContent()
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

func (m promptViewerModel) handleNormalInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		if m.filter != "" {
			m.filter = ""
			m.matches = nil
			m.matchIdx = 0
			m.refreshContent()
			return m, nil
		}
		return m, tea.Quit
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyCtrlD, tea.KeyCtrlU:
		// Swallow vi half-page bindings
		return m, nil
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			switch msg.Runes[0] {
			case 'q':
				return m, tea.Quit
			case 'g':
				if m.ready && m.errorMsg == "" {
					m.viewport.GotoTop()
					return m, nil
				}
			case 'G':
				if m.ready && m.errorMsg == "" {
					m.viewport.GotoBottom()
					return m, nil
				}
			case 'j', 'k', 'd', 'u':
				// Swallow vi scroll bindings
				return m, nil
			case '/':
				m.searching = true
				m.filter = ""
				return m, nil
			case 'n':
				if len(m.matches) > 0 {
					m.matchIdx = (m.matchIdx + 1) % len(m.matches)
					m.jumpToMatch(m.matchIdx)
				}
				return m, nil
			case 'N':
				if len(m.matches) > 0 {
					m.matchIdx--
					if m.matchIdx < 0 {
						m.matchIdx = len(m.matches) - 1
					}
					m.jumpToMatch(m.matchIdx)
				}
				return m, nil
			}
		}
	}

	// Pass remaining keys to viewport (arrows, pgup/pgdown)
	if m.ready && m.errorMsg == "" {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *promptViewerModel) updateMatches() {
	m.matches = nil
	if m.filter == "" {
		return
	}
	lower := strings.ToLower(m.filter)
	for i, pl := range m.lines {
		if strings.Contains(strings.ToLower(pl.text), lower) {
			m.matches = append(m.matches, i)
		}
	}
}

func (m *promptViewerModel) jumpToMatch(idx int) {
	if idx < 0 || idx >= len(m.matches) {
		return
	}
	lineIdx := m.matches[idx]
	// Calculate target position (center the match in viewport)
	targetLine := lineIdx - m.viewport.Height/2
	if targetLine < 0 {
		targetLine = 0
	}
	m.viewport.SetYOffset(targetLine)
	m.refreshContent()
}

func (m *promptViewerModel) refreshContent() {
	if len(m.lines) == 0 {
		return
	}

	var content strings.Builder
	for i, pl := range m.lines {
		if i > 0 {
			content.WriteString("\n")
		}

		// Check if this is the current match
		isCurrentMatch := false
		if m.filter != "" && len(m.matches) > 0 && m.matchIdx < len(m.matches) {
			if m.matches[m.matchIdx] == i {
				isCurrentMatch = true
			}
		}

		if isCurrentMatch {
			// Highlight entire line for current match
			styled := renderPromptLine(pl, m.filter)
			content.WriteString(pvMatchStyle.Render(stripAnsiForMatch(styled)))
		} else {
			// Normal rendering with optional filter highlighting
			content.WriteString(renderPromptLine(pl, m.filter))
		}
	}
	m.viewport.SetContent(content.String())
}

// stripAnsiForMatch removes ANSI codes for clean match highlighting.
func stripAnsiForMatch(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			i++
			for i < len(s) {
				c := s[i]
				i++
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
			}
			continue
		}
		result.WriteByte(s[i])
		i++
	}
	return result.String()
}

func (m promptViewerModel) View() string {
	if m.errorMsg != "" {
		return m.viewError()
	}

	if !m.ready {
		return ""
	}

	var b strings.Builder

	// Header
	title := fmt.Sprintf(" %s %s — System Prompt", m.indicator, m.profile)
	if m.filter != "" && len(m.matches) > 0 {
		title += pvSearchStyle.Render(fmt.Sprintf(" [%s] ", m.filter)) +
			pvHelpStyle.Render(fmt.Sprintf("%d/%d", m.matchIdx+1, len(m.matches)))
	} else if m.filter != "" {
		title += pvSearchStyle.Render(fmt.Sprintf(" [%s] ", m.filter)) +
			pvHelpStyle.Render("no matches")
	}
	b.WriteString(pvTitleStyle.Render(title))
	b.WriteString("\n")
	b.WriteString(pvHelpStyle.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	// Content
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	// Footer
	b.WriteString(m.renderFooter())

	return b.String()
}

func (m promptViewerModel) renderFooter() string {
	if m.searching {
		return " " + pvSearchStyle.Render("/") + m.filter + pvHelpStyle.Render("▏")
	}

	pct := m.viewport.ScrollPercent() * 100
	position := ""
	if m.viewport.TotalLineCount() > m.viewport.Height {
		position = fmt.Sprintf(" %.0f%%", pct)
	}

	help := "↑/↓ scroll  g/G top/bottom  / search  q quit"
	if m.filter != "" {
		help = "n/N match  Esc clear  " + help
	}
	return " " + pvHelpStyle.Render(help) + pvHelpStyle.Render(position)
}

func (m promptViewerModel) viewError() string {
	row := m.height / 2
	col := (m.width - len(m.errorMsg)) / 2
	if col < 0 {
		col = 0
	}

	var b strings.Builder
	for i := 0; i < row; i++ {
		b.WriteString("\n")
	}
	b.WriteString(strings.Repeat(" ", col))
	b.WriteString(pvErrorStyle.Render(m.errorMsg))

	return b.String()
}

func (cmd *PromptViewerCmd) Run() error {
	result := cmd.fetchPrompt()
	m := initialPromptViewerModel(result)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (cmd *PromptViewerCmd) fetchPrompt() promptResult {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/session/prompt?window=%s",
		cmd.Port, url.QueryEscape(cmd.WindowID)))
	if err != nil {
		return promptResult{errorMsg: "Could not reach the daemon."}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return promptResult{errorMsg: "No session in this window."}
	}
	if resp.StatusCode != http.StatusOK {
		return promptResult{errorMsg: fmt.Sprintf("Daemon returned %d.", resp.StatusCode)}
	}

	var result struct {
		Profile      string `json:"profile"`
		Indicator    string `json:"indicator"`
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return promptResult{errorMsg: "Failed to decode response."}
	}

	if result.SystemPrompt == "" {
		return promptResult{errorMsg: "No system prompt stored for this session."}
	}

	return promptResult{
		profile:      result.Profile,
		indicator:    result.Indicator,
		systemPrompt: result.SystemPrompt,
	}
}
