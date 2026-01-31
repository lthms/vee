package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// tmuxCmd builds an exec.Cmd for tmux using the "vee" named socket.
func tmuxCmd(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-L", "vee"}, args...)...)
}

// tmuxRun executes a tmux command and returns its combined output.
func tmuxRun(args ...string) (string, error) {
	cmd := tmuxCmd(args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// tmuxSessionExists checks whether the "vee" tmux session exists.
func tmuxSessionExists() bool {
	cmd := tmuxCmd("has-session", "-t", "vee")
	return cmd.Run() == nil
}

// tmuxCreateSession creates a new detached tmux session named "vee" with a dashboard window.
func tmuxCreateSession(dashboardCmd string) error {
	_, err := tmuxRun("new-session", "-d", "-s", "vee", "-n", "dashboard", dashboardCmd)
	return err
}

// tmuxAttach attaches to the "vee" session, taking over the current terminal.
// This call blocks until the user detaches or the session ends.
func tmuxAttach() error {
	cmd := tmuxCmd("attach-session", "-t", "vee")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// tmuxNewWindow creates a new window in the "vee" session running shellCmd.
// Returns the tmux window ID (e.g. "@3") which is stable across renumbering.
func tmuxNewWindow(name, shellCmd string) (string, error) {
	return tmuxRun("new-window", "-t", "vee", "-n", name, "-P", "-F", "#{window_id}", shellCmd)
}

// tmuxKillWindow kills a specific window in the "vee" session.
func tmuxKillWindow(target string) error {
	_, err := tmuxRun("kill-window", "-t", "vee:"+target)
	return err
}

// tmuxKillWindowByID kills a tmux window by its stable window ID (e.g. "@3").
func tmuxKillWindowByID(windowID string) error {
	_, err := tmuxRun("kill-window", "-t", windowID)
	return err
}

// tmuxGracefulClose gracefully terminates a Claude process in a tmux window
// by sending Ctrl-C (to interrupt any ongoing work) then /exit (to trigger
// a clean shutdown that persists the session index). Falls back to killing
// the window after a timeout. Blocks for several seconds — call from a goroutine.
func tmuxGracefulClose(windowID string) {
	// Move window to a hidden background session so it disappears
	// from the status bar and Ctrl-b n/p navigation immediately
	if cmd := tmuxCmd("has-session", "-t", "vee-bg"); cmd.Run() != nil {
		tmuxRun("new-session", "-d", "-s", "vee-bg")
	}
	tmuxRun("move-window", "-s", windowID, "-t", "vee-bg:")
	tmuxRun("select-window", "-t", "vee:0")

	// Send /exit to the (now hidden) window — window IDs are global
	tmuxRun("send-keys", "-t", windowID, "-l", "/exit")
	time.Sleep(100 * time.Millisecond)
	tmuxRun("send-keys", "-t", windowID, "Enter")
	// Give Claude time to persist session data
	time.Sleep(10 * time.Second)
	// Fallback if window is still alive
	tmuxKillWindowByID(windowID)
}

// tmuxConfigure applies all tmux configuration for the Vee session.
func tmuxConfigure(veeBinary string, port int, veePath string, zettelkasten bool, passthrough []string) error {
	// Each entry is a slice of tmux set-option/bind-key args.
	commands := [][]string{
		// True color support
		{"set-option", "-t", "vee", "-g", "default-terminal", "tmux-256color"},
		{"set-option", "-t", "vee", "-ga", "terminal-overrides", ",*256col*:Tc,alacritty:Tc,xterm-kitty:Tc"},

		// Status bar — Tokyo Night theme
		{"set-option", "-t", "vee", "-g", "status-style", "bg=#1a1b26,fg=#a9b1d6"},
		{"set-option", "-t", "vee", "-g", "status-left", ""},
		{"set-option", "-t", "vee", "-g", "status-right", " #S "},
		{"set-option", "-t", "vee", "-g", "window-status-format", " #W "},
		{"set-option", "-t", "vee", "-g", "window-status-current-format", "#[bg=#7aa2f7,fg=#1a1b26] #W #[default]"},

		// Window behavior
		{"set-option", "-t", "vee", "-g", "allow-rename", "off"},
		{"set-option", "-t", "vee", "-g", "mouse", "on"},
		{"set-option", "-t", "vee", "-g", "history-limit", "50000"},

		// Renumber windows on close so indices stay compact
		{"set-option", "-t", "vee", "-g", "renumber-windows", "on"},
	}

	for _, args := range commands {
		if _, err := tmuxRun(args...); err != nil {
			return fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
		}
	}

	// Rebind Ctrl-b c to the mode picker menu
	menuArgs := buildModeMenuArgs(veeBinary, port, veePath, zettelkasten, passthrough)
	if _, err := tmuxRun(menuArgs...); err != nil {
		return fmt.Errorf("tmux bind-key menu: %w", err)
	}

	// Ctrl-b q: kill the vee session
	if _, err := tmuxRun("bind-key", "-T", "prefix", "q", "kill-session", "-t", "vee"); err != nil {
		return fmt.Errorf("tmux bind-key q: %w", err)
	}

	// Ctrl-b x: suspend the session in the current window
	suspendCmd := fmt.Sprintf("%s _suspend-window --port %d --window-id #{window_id}", shelljoin(veeBinary), port)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "x", "run-shell", suspendCmd); err != nil {
		return fmt.Errorf("tmux bind-key x: %w", err)
	}

	// Ctrl-b r: resume a suspended session
	resumeCmd := fmt.Sprintf("%s _resume-menu --port %d", shelljoin(veeBinary), port)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "r", "run-shell", resumeCmd); err != nil {
		return fmt.Errorf("tmux bind-key r: %w", err)
	}

	return nil
}

// buildModeMenuArgs constructs the tmux bind-key command that opens a
// display-menu with one entry per Vee mode. Each entry runs:
//
//	vee _new-pane --vee-path=... --port=... --mode=<mode> [-- claude-args]
func buildModeMenuArgs(veeBinary string, port int, veePath string, zettelkasten bool, passthrough []string) []string {
	args := []string{"bind-key", "-T", "prefix", "c", "display-menu", "-T", "New Session"}

	for _, name := range modeOrder {
		mode, ok := modeRegistry[name]
		if !ok {
			continue
		}

		label := fmt.Sprintf("%s %s", mode.Indicator, mode.Name)

		// Build the vee _new-pane command
		var cmdParts []string
		cmdParts = append(cmdParts, shelljoin(veeBinary))
		cmdParts = append(cmdParts, "_new-pane")
		cmdParts = append(cmdParts, "--vee-path", shelljoin(veePath))
		cmdParts = append(cmdParts, "--port", fmt.Sprintf("%d", port))
		if zettelkasten {
			cmdParts = append(cmdParts, "-z")
		}
		cmdParts = append(cmdParts, "--mode", name)

		if len(passthrough) > 0 {
			cmdParts = append(cmdParts, "--")
			for _, p := range passthrough {
				cmdParts = append(cmdParts, shelljoin(p))
			}
		}

		shellCmd := strings.Join(cmdParts, " ")

		// display-menu format: label, key-shortcut, action
		// Use empty string for key shortcut (no accelerator)
		args = append(args, label, "", "run-shell "+shelljoin(shellCmd))
	}

	return args
}

// shelljoin quotes a string for safe use in a shell command if it contains
// special characters.
func shelljoin(s string) string {
	if s == "" {
		return "''"
	}
	// If it contains no shell-special characters, return as-is.
	safe := true
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' ||
			c == '.' || c == '/' || c == ':' || c == '=' || c == '+') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	// Use single quotes; escape existing single quotes.
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
