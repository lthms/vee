package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SessionPickerCmd is the internal subcommand that shows an interactive
// profile picker with a prompt input, rendered inside tmux display-popup.
type SessionPickerCmd struct {
	VeePath    string `required:"" type:"path" name:"vee-path"`
	Port       int    `short:"p" default:"2700" name:"port"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

type pickerProfile struct {
	name              string
	indicator         string
	desc              string
	defaultPrompt     string
	promptPlaceholder string
}

// pickerModel is the Bubble Tea model for the profile picker.
type pickerModel struct {
	profiles     []pickerProfile
	cursor       int
	prompt       textarea.Model
	ephemeral    bool
	canEphemeral bool
	width        int
	height       int

	// Scrolling state
	scrollOffset int

	// Integration
	cmd  *SessionPickerCmd
	args claudeArgs

	// Result
	confirmed bool
	cancelled bool
}

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#89b4fa"))

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#89b4fa"))

	profileNameStyle = lipgloss.NewStyle().
				Bold(true)

	dimStyle = lipgloss.NewStyle().
			Faint(true)

	ephemeralActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#f9e2af"))

	helpStyle = lipgloss.NewStyle().
			Faint(true)
)

func initialPickerModel(cmd *SessionPickerCmd, profiles []pickerProfile, args claudeArgs) pickerModel {
	ta := textarea.New()
	ta.Focus()
	ta.CharLimit = 2000
	ta.SetWidth(60)
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.Prompt = ""

	// Remove all default styling (borders, backgrounds, etc.)
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle()
	ta.FocusedStyle.Text = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.Prompt = lipgloss.NewStyle()
	ta.BlurredStyle.Text = lipgloss.NewStyle()

	return pickerModel{
		profiles:     profiles,
		cursor:       0,
		prompt:       ta,
		ephemeral:    false,
		canEphemeral: ephemeralAvailable(),
		width:        80,
		height:       20,
		scrollOffset: 0,
		cmd:          cmd,
		args:         args,
	}
}

func (m pickerModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.prompt.SetWidth(min(60, m.width-12))
		return m, nil

	case tea.KeyMsg:
		cur := m.profiles[m.cursor]
		isStatic := cur.defaultPrompt != "" && !strings.Contains(cur.defaultPrompt, "{}")

		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			m.cancelled = true
			return m, tea.Quit

		case tea.KeyEnter:
			// Enter confirms (don't pass to textarea)
			m.confirmed = true
			return m, tea.Quit

		case tea.KeyCtrlP:
			if m.cursor > 0 {
				m.cursor--
				m.updateScroll()
				m.updatePlaceholder()
			}
			return m, nil

		case tea.KeyCtrlN:
			if m.cursor < len(m.profiles)-1 {
				m.cursor++
				m.updateScroll()
				m.updatePlaceholder()
			}
			return m, nil

		case tea.KeyCtrlE:
			if m.canEphemeral {
				m.ephemeral = !m.ephemeral
			}
			return m, nil
		}

		// Pass key to textarea if prompt is editable
		if !isStatic {
			var cmd tea.Cmd
			m.prompt, cmd = m.prompt.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m *pickerModel) updatePlaceholder() {
	cur := m.profiles[m.cursor]
	m.prompt.Placeholder = cur.promptPlaceholder
}

func (m pickerModel) promptHeight() int {
	// Calculate height based on content, min 2, max 5
	lines := strings.Count(m.prompt.Value(), "\n") + 1
	if lines < 2 {
		lines = 2
	}
	if lines > 5 {
		lines = 5
	}
	return lines
}

func (m *pickerModel) updateScroll() {
	visibleRows := m.visibleProfileRows()
	if visibleRows <= 0 {
		visibleRows = len(m.profiles)
	}

	// Keep cursor visible with some padding
	if m.cursor < m.scrollOffset {
		m.scrollOffset = m.cursor
	} else if m.cursor >= m.scrollOffset+visibleRows {
		m.scrollOffset = m.cursor - visibleRows + 1
	}
}

func (m pickerModel) visibleProfileRows() int {
	// Reserve space for: title (2 lines) + ephemeral (2) + prompt area (4) + help (2) + margins (2)
	overhead := 12
	if !m.canEphemeral {
		overhead -= 2
	}
	rows := m.height - overhead
	if rows < 1 {
		rows = 1
	}
	return rows
}

func (m pickerModel) View() string {
	var b strings.Builder

	// Title
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(titleStyle.Render("New Session"))
	b.WriteString("\n\n")

	// Profile list with scrolling
	maxNameLen := 0
	for _, p := range m.profiles {
		if len(p.name) > maxNameLen {
			maxNameLen = len(p.name)
		}
	}

	visibleRows := m.visibleProfileRows()
	endIdx := m.scrollOffset + visibleRows
	if endIdx > len(m.profiles) {
		endIdx = len(m.profiles)
	}

	for i := m.scrollOffset; i < endIdx; i++ {
		p := m.profiles[i]
		if i == m.cursor {
			b.WriteString("  ")
			b.WriteString(selectedStyle.Render(">"))
			b.WriteString(" ")
		} else {
			b.WriteString("    ")
		}

		b.WriteString(p.indicator)
		b.WriteString(" ")
		b.WriteString(profileNameStyle.Render(fmt.Sprintf("%-*s", maxNameLen, p.name)))
		b.WriteString("  ")
		b.WriteString(dimStyle.Render(p.desc))
		b.WriteString("\n")
	}

	// Scroll indicators
	if len(m.profiles) > visibleRows {
		scrollInfo := fmt.Sprintf("  %d-%d of %d", m.scrollOffset+1, endIdx, len(m.profiles))
		b.WriteString(dimStyle.Render(scrollInfo))
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Ephemeral toggle
	if m.canEphemeral {
		b.WriteString("  ")
		if m.ephemeral {
			b.WriteString(ephemeralActiveStyle.Render("\u23e3 Ephemeral"))
		} else {
			b.WriteString(dimStyle.Render("\u23e3 Local"))
		}
		b.WriteString("\n")
	}

	// Prompt input
	cur := m.profiles[m.cursor]
	isStatic := cur.defaultPrompt != "" && !strings.Contains(cur.defaultPrompt, "{}")

	b.WriteString("\n")
	if isStatic {
		b.WriteString("  ")
		b.WriteString(dimStyle.Render("Prompt: " + cur.defaultPrompt))
	} else {
		// Indent each line of the textarea output
		taView := m.prompt.View()
		lines := strings.Split(taView, "\n")
		for i, line := range lines {
			if i == 0 {
				b.WriteString(dimStyle.Render("  Prompt:"))
				b.WriteString(" ")
				b.WriteString(line)
			} else {
				b.WriteString("\n          ") // Align with text after "Prompt: "
				b.WriteString(line)
			}
		}
	}
	b.WriteString("\n")

	// Help text
	b.WriteString("\n  ")
	if m.canEphemeral {
		b.WriteString(helpStyle.Render("C-n/C-p select  C-e ephemeral  Enter confirm  Esc cancel"))
	} else {
		b.WriteString(helpStyle.Render("C-n/C-p select  Enter confirm  Esc cancel"))
	}
	b.WriteString("\n")

	return b.String()
}

func (cmd *SessionPickerCmd) Run(args claudeArgs) error {
	tmuxSocketName = cmd.TmuxSocket
	if err := initProfileRegistry(cmd.VeePath); err != nil {
		return err
	}

	var profiles []pickerProfile
	for _, name := range profileOrder {
		profile, ok := profileRegistry[name]
		if !ok {
			continue
		}
		profiles = append(profiles, pickerProfile{
			name:              name,
			indicator:         profile.Indicator,
			desc:              profile.Description,
			defaultPrompt:     profile.DefaultPrompt,
			promptPlaceholder: profile.PromptPlaceholder,
		})
	}

	m := initialPickerModel(cmd, profiles, args)
	// Set initial placeholder
	if len(profiles) > 0 {
		m.prompt.Placeholder = profiles[0].promptPlaceholder
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return err
	}

	result := finalModel.(pickerModel)
	if result.cancelled {
		return nil
	}

	if result.confirmed {
		// Normalize newlines to spaces for the prompt
		promptText := strings.ReplaceAll(result.prompt.Value(), "\n", " ")
		return cmd.createSession(result.profiles[result.cursor], promptText, result.ephemeral, result.args)
	}

	return nil
}

func (cmd *SessionPickerCmd) createSession(profile pickerProfile, prompt string, ephemeral bool, args claudeArgs) error {
	// Template expansion: resolve the final prompt from default_prompt + user input.
	if profile.defaultPrompt != "" {
		if strings.Contains(profile.defaultPrompt, "{}") {
			prompt = strings.Replace(profile.defaultPrompt, "{}", prompt, 1)
		} else {
			prompt = profile.defaultPrompt
		}
	}

	veeBinary, err := os.Executable()
	if err != nil {
		return err
	}

	var cmdParts []string
	cmdParts = append(cmdParts, veeBinary)
	cmdParts = append(cmdParts, "_new-pane")
	cmdParts = append(cmdParts, "--vee-path", cmd.VeePath)
	cmdParts = append(cmdParts, "--port", fmt.Sprintf("%d", cmd.Port))
	cmdParts = append(cmdParts, "--tmux-socket", tmuxSocketName)
	cmdParts = append(cmdParts, "--profile", profile.name)
	if prompt != "" {
		cmdParts = append(cmdParts, "--prompt", prompt)
	}
	if ephemeral {
		cmdParts = append(cmdParts, "--ephemeral")
	}

	if len(args) > 0 {
		cmdParts = append(cmdParts, "--")
		cmdParts = append(cmdParts, []string(args)...)
	}

	execCmd := exec.Command(cmdParts[0], cmdParts[1:]...)
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr
	return execCmd.Run()
}
