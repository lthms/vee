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
)

// dashboardState mirrors the /api/state JSON response.
type dashboardState struct {
	Active    []*Session `json:"active_sessions"`
	Suspended []*Session `json:"suspended_sessions"`
	Completed []*Session `json:"completed_sessions"`
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Initial render
	cmd.render(cmd.fetchState())

	for {
		select {
		case <-sigCh:
			fmt.Print("\033[?25h") // show cursor
			return nil
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
	var sb strings.Builder

	// Clear screen and move cursor to top-left, hide cursor
	sb.WriteString("\033[2J\033[H\033[?25l")

	// Header
	sb.WriteString("\r\n")
	sb.WriteString("  ")
	sb.WriteString(ansiAccent)
	sb.WriteString(ansiBold)
	sb.WriteString("Vee")
	sb.WriteString(ansiReset)
	sb.WriteString(ansiDim)
	sb.WriteString(" — Modal Code Assistant")
	sb.WriteString(ansiReset)
	sb.WriteString("\r\n\r\n")

	if state == nil {
		sb.WriteString("  ")
		sb.WriteString(ansiMuted)
		sb.WriteString("Connecting to daemon...")
		sb.WriteString(ansiReset)
		sb.WriteString("\r\n")
	} else {
		// Active sessions
		cmd.renderSection(&sb, "ACTIVE", state.Active, ansiGreen)
		// Suspended sessions
		cmd.renderSection(&sb, "SUSPENDED", state.Suspended, ansiYellow)
		// Completed sessions
		cmd.renderSection(&sb, "COMPLETED", state.Completed, ansiMuted)
	}

	// Keybindings footer
	sb.WriteString("\r\n")
	sb.WriteString("  ")
	sb.WriteString(ansiMuted)
	sb.WriteString("Ctrl-b c")
	sb.WriteString(ansiReset)
	sb.WriteString(" new session  ")
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

func (cmd *DashboardCmd) renderSection(sb *strings.Builder, title string, sessions []*Session, color string) {
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
		sb.WriteString("    ")
		sb.WriteString(color)
		sb.WriteString(sess.Indicator)
		sb.WriteString(" ")
		sb.WriteString(ansiBold)
		sb.WriteString(sess.Mode)
		sb.WriteString(ansiReset)

		// Age
		age := formatAge(time.Since(sess.StartedAt))
		sb.WriteString("  ")
		sb.WriteString(ansiMuted)
		sb.WriteString(age)
		sb.WriteString(ansiReset)

		// Preview
		if sess.Preview != "" {
			sb.WriteString("  ")
			sb.WriteString(ansiDim)
			sb.WriteString(ansiItalic)
			preview := sess.Preview
			if len(preview) > 60 {
				preview = preview[:60] + "..."
			}
			sb.WriteString(preview)
			sb.WriteString(ansiReset)
		}

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
