package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// DashboardCmd is the internal subcommand that renders a session dashboard in the terminal.
type DashboardCmd struct {
	Port int `short:"p" default:"2700" name:"port"`
}

// ANSI escape helpers — foreground only, no background overrides.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiItalic = "\033[3m"
	ansiAccent = "\033[38;2;137;180;250m" // #89b4fa
	ansiGreen  = "\033[38;2;166;227;161m" // #a6e3a1
	ansiYellow = "\033[38;2;249;226;175m" // #f9e2af
	ansiMuted  = "\033[38;2;147;153;178m" // #9399b2
	ansiOrange = "\033[38;2;255;158;100m" // #ff9e64
)

// dashboardState mirrors the /api/state JSON response.
type dashboardState struct {
	Active     []*Session     `json:"active_sessions"`
	Suspended  []*Session     `json:"suspended_sessions"`
	Completed  []*Session     `json:"completed_sessions"`
	Indexing   []IndexingTask `json:"indexing_tasks"`
	IssueCount int            `json:"issue_count"`
}

// Run starts the dashboard TUI loop.
func (cmd *DashboardCmd) Run() error {
	// Raw mode so keystrokes don't echo on screen
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), oldState)
		// Drain stdin in background so keypresses are silently consumed
		go func() {
			buf := make([]byte, 64)
			for {
				os.Stdin.Read(buf)
			}
		}()
	}

	// Hide cursor once at startup; defer guarantees it's restored on any exit path
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Initial render
	cmd.render(cmd.fetchState())

	for {
		select {
		case <-sigCh:
			return nil
		case <-winchCh:
			cmd.render(cmd.fetchState())
		case <-ticker.C:
			cmd.render(cmd.fetchState())
		}
	}
}

func (cmd *DashboardCmd) fetchState() *dashboardState {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", cmd.Port))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var state dashboardState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil
	}
	return &state
}

func (cmd *DashboardCmd) render(state *dashboardState) {
	termWidth, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || termWidth < 40 {
		termWidth = 80
	}

	var sb strings.Builder

	// Clear screen and move cursor to top-left
	sb.WriteString("\033[2J\033[H")

	// Header
	sb.WriteString("\r\n")
	sb.WriteString("  ")
	sb.WriteString(ansiAccent)
	sb.WriteString(ansiBold)
	sb.WriteString("Vee")
	sb.WriteString(ansiReset)
	sb.WriteString(ansiDim)
	sb.WriteString(" — Session Orchestrator")
	sb.WriteString(ansiReset)

	if state != nil && state.IssueCount > 0 {
		sb.WriteString("  ")
		sb.WriteString(ansiYellow)
		sb.WriteString(fmt.Sprintf("⚠ %d issue", state.IssueCount))
		if state.IssueCount > 1 {
			sb.WriteString("s")
		}
		sb.WriteString(ansiReset)
	}

	sb.WriteString("\r\n\r\n")

	if state == nil {
		sb.WriteString("  ")
		sb.WriteString(ansiMuted)
		sb.WriteString("Connecting to daemon...")
		sb.WriteString(ansiReset)
		sb.WriteString("\r\n")
	} else {
		// Active sessions
		cmd.renderSection(&sb, "ACTIVE", state.Active, ansiGreen, termWidth)
		// Indexing tasks (only shown when non-empty)
		if len(state.Indexing) > 0 {
			cmd.renderIndexingSection(&sb, state.Indexing, termWidth)
		}
		// Suspended sessions
		cmd.renderSection(&sb, "SUSPENDED", state.Suspended, ansiYellow, termWidth)
		// Completed sessions
		cmd.renderSection(&sb, "COMPLETED", state.Completed, ansiMuted, termWidth)
	}

	// Keybindings footer
	sb.WriteString("\r\n")
	sb.WriteString("  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Ctrl-b c")
	sb.WriteString(ansiReset)
	sb.WriteString(" new  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Ctrl-b /")
	sb.WriteString(ansiReset)
	sb.WriteString(" KB  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Ctrl-b i")
	sb.WriteString(ansiReset)
	sb.WriteString(" issues  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Ctrl-b n/p")
	sb.WriteString(ansiReset)
	sb.WriteString(" next/prev  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Ctrl-b d")
	sb.WriteString(ansiReset)
	sb.WriteString(" detach")
	sb.WriteString("\r\n")

	fmt.Print(sb.String())
}

func (cmd *DashboardCmd) renderSection(sb *strings.Builder, title string, sessions []*Session, color string, termWidth int) {
	sb.WriteString("  ")
	sb.WriteString(ansiMuted)
	sb.WriteString(title)
	sb.WriteString(ansiReset)
	sb.WriteString("\r\n")

	if len(sessions) == 0 {
		sb.WriteString("    ")
		sb.WriteString(ansiMuted)
		sb.WriteString(ansiItalic)
		sb.WriteString("none")
		sb.WriteString(ansiReset)
		sb.WriteString("\r\n\r\n")
		return
	}

	for _, sess := range sessions {
		age := formatAge(time.Since(sess.StartedAt))

		// Layout: indent(4) + ⏣(1) + space(1) + indicator(2) + space(1) + profile + gap + preview + gap + age
		const indent = 4
		const badgeWidth = 1    // ephemeral(1)
		const indicatorWidth = 2 // emoji
		leftFixed := indent + badgeWidth + 1 + indicatorWidth + 1 + len(sess.Profile)
		rightFixed := len(age) + 2 // +2 for right margin

		sb.WriteString("    ")

		// Ephemeral badge (always shown, colored when active, dim when not)
		if sess.Ephemeral {
			sb.WriteString(ansiYellow)
		} else {
			sb.WriteString(ansiDim)
		}
		sb.WriteString("⏣")
		sb.WriteString(ansiReset)

		// Indicator + profile name
		sb.WriteString(" ")
		sb.WriteString(color)
		sb.WriteString(sess.Indicator)
		sb.WriteString(" ")
		sb.WriteString(ansiBold)
		sb.WriteString(sess.Profile)
		sb.WriteString(ansiReset)

		usedWidth := leftFixed

		// Preview (between profile and age)
		if sess.Preview != "" {
			maxPreview := termWidth - leftFixed - rightFixed - 4
			if maxPreview > 3 {
				preview := sess.Preview
				if len(preview) > maxPreview {
					preview = preview[:maxPreview-3] + "..."
				}
				sb.WriteString("  ")
				sb.WriteString(ansiDim)
				sb.WriteString(ansiItalic)
				sb.WriteString(preview)
				sb.WriteString(ansiReset)
				usedWidth += 2 + len(preview)
			}
		}

		// Right-align age
		padding := termWidth - usedWidth - rightFixed
		if padding < 2 {
			padding = 2
		}
		sb.WriteString(strings.Repeat(" ", padding))
		sb.WriteString(ansiMuted)
		sb.WriteString(age)
		sb.WriteString(ansiReset)

		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
}

const ansiCyan = "\033[38;2;137;220;235m" // #89dceb
const ansiTeal = "\033[38;2;115;218;202m" // #73daca

func (cmd *DashboardCmd) renderIndexingSection(sb *strings.Builder, tasks []IndexingTask, termWidth int) {
	sb.WriteString("  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("INDEXING")
	sb.WriteString(ansiReset)
	sb.WriteString("\r\n")

	for _, task := range tasks {
		age := formatAge(time.Since(task.StartedAt))

		title := task.Title
		const indent = 4
		const iconWidth = 2
		leftFixed := indent + iconWidth + 1 + len(title)
		rightFixed := len(age) + 2

		sb.WriteString("    ")
		sb.WriteString(ansiCyan)
		sb.WriteString("⟳")
		sb.WriteString(" ")
		sb.WriteString(ansiBold)
		maxTitle := termWidth - indent - iconWidth - 1 - rightFixed - 4
		if maxTitle > 0 && len(title) > maxTitle {
			title = title[:maxTitle-3] + "..."
			leftFixed = indent + iconWidth + 1 + len(title)
		}
		sb.WriteString(title)
		sb.WriteString(ansiReset)

		padding := termWidth - leftFixed - rightFixed
		if padding < 2 {
			padding = 2
		}
		sb.WriteString(strings.Repeat(" ", padding))
		sb.WriteString(ansiMuted)
		sb.WriteString(age)
		sb.WriteString(ansiReset)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
}

func formatAge(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds ago", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm ago", s/60)
	}
	h := s / 3600
	m := (s % 3600) / 60
	return fmt.Sprintf("%dh%dm ago", h, m)
}
