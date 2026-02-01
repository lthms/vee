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

// tmuxSetWindowOption sets a per-window user option (@-prefixed) on a tmux window.
func tmuxSetWindowOption(windowID, key, value string) error {
	_, err := tmuxRun("set-option", "-t", windowID, "-p", "@"+key, value)
	return err
}

// tmuxUnsetWindowOption removes a per-window user option from a tmux window.
func tmuxUnsetWindowOption(windowID, key string) error {
	_, err := tmuxRun("set-option", "-t", windowID, "-p", "-u", "@"+key)
	return err
}

// syncWindowOptions pushes the session's dynamic state to tmux per-window options.
func syncWindowOptions(sess *Session) error {
	if sess.WindowTarget == "" {
		return nil
	}
	wid := sess.WindowTarget

	// @vee-working
	if sess.Working {
		tmuxSetWindowOption(wid, "vee-working", "1")
	} else {
		tmuxUnsetWindowOption(wid, "vee-working")
	}

	// @vee-notif
	if sess.HasNotification {
		tmuxSetWindowOption(wid, "vee-notif", "1")
	} else {
		tmuxUnsetWindowOption(wid, "vee-notif")
	}

	// @vee-perm
	if sess.PermissionMode != "" && sess.PermissionMode != "default" {
		tmuxSetWindowOption(wid, "vee-perm", sess.PermissionMode)
	} else {
		tmuxUnsetWindowOption(wid, "vee-perm")
	}

	return nil
}

// tmuxConfigure applies all tmux configuration for the Vee session.
func tmuxConfigure(veeBinary string, port int, veePath string, passthrough []string) error {
	// Window status format strings with dynamic indicators.
	// Indicators use per-window @vee-* user options:
	//   @vee-ephemeral:  ⏣ (inherits tab fg)
	//   @vee-kb-ingest:  ⊙ (teal #73daca)
	//   @vee-working:    ✱ (orange #ff9e64) — mutually exclusive with notif (working wins)
	//   @vee-notif:      ♪ (blue #7aa2f7)
	//   @vee-perm:       ⏸ for "plan" (yellow #e0af68), ⏵⏵ for "acceptEdits" (violet #bb9af7)
	windowStatusFmt := ` #W#{?#{@vee-ephemeral}, ⏣,}#{?#{@vee-kb-ingest},#[fg=#73daca] ⊙#[fg=default],}#{?#{@vee-working},#[fg=#ff9e64] ✱#[fg=default],#{?#{@vee-notif},#[fg=#7aa2f7] ♪#[fg=default],}}#{?#{==:#{@vee-perm},plan},#[fg=#e0af68] ⏸#[fg=default],}#{?#{==:#{@vee-perm},acceptEdits},#[fg=#bb9af7] ⏵⏵#[fg=default],} `
	windowStatusCurrentFmt := `#[bg=#414868,fg=#a9b1d6] #W#{?#{@vee-ephemeral}, ⏣,}#{?#{@vee-kb-ingest},#[fg=#73daca] ⊙#[fg=#a9b1d6],}#{?#{@vee-working},#[fg=#ff9e64] ✱#[fg=#a9b1d6],#{?#{@vee-notif},#[fg=#7aa2f7] ♪#[fg=#a9b1d6],}}#{?#{==:#{@vee-perm},plan},#[fg=#e0af68] ⏸#[fg=#a9b1d6],}#{?#{==:#{@vee-perm},acceptEdits},#[fg=#bb9af7] ⏵⏵#[fg=#a9b1d6],} #[default]`

	// Each entry is a slice of tmux set-option/bind-key args.
	commands := [][]string{
		// True color support
		{"set-option", "-t", "vee", "-g", "default-terminal", "tmux-256color"},
		{"set-option", "-t", "vee", "-ga", "terminal-overrides", ",*256col*:Tc,alacritty:Tc,xterm-kitty:Tc"},

		// Status bar — Tokyo Night theme
		{"set-option", "-t", "vee", "-g", "status-style", "bg=#1a1b26,fg=#a9b1d6"},
		{"set-option", "-t", "vee", "-g", "status-left", ""},
		{"set-option", "-t", "vee", "-g", "status-right", " #S "},
		{"set-option", "-t", "vee", "-g", "status-interval", "1"},
		{"set-option", "-t", "vee", "-g", "window-status-format", windowStatusFmt},
		{"set-option", "-t", "vee", "-g", "window-status-current-format", windowStatusCurrentFmt},

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

	// Rebind Ctrl-b c to the session picker popup
	pickerCmd := buildPickerPopupCmd(veeBinary, port, veePath, passthrough)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "c", "display-popup", "-E", "-w", "80", "-h", "20", pickerCmd); err != nil {
		return fmt.Errorf("tmux bind-key picker: %w", err)
	}

	// Ctrl-b q: graceful shutdown (suspend all sessions, clean up, kill tmux)
	shutdownCmd := fmt.Sprintf("%s _shutdown --port %d", shelljoin(veeBinary), port)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "q", "run-shell", shutdownCmd); err != nil {
		return fmt.Errorf("tmux bind-key q: %w", err)
	}

	// Ctrl-b x: suspend the session in the current window
	suspendCmd := fmt.Sprintf("%s _suspend-window --port %d --window-id #{window_id}", shelljoin(veeBinary), port)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "x", "run-shell", suspendCmd); err != nil {
		return fmt.Errorf("tmux bind-key x: %w", err)
	}

	// Ctrl-b k: complete (kill) the session in the current window
	completeCmd := fmt.Sprintf("%s _complete-window --port %d --window-id #{window_id}", shelljoin(veeBinary), port)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "k", "run-shell", completeCmd); err != nil {
		return fmt.Errorf("tmux bind-key k: %w", err)
	}

	// Ctrl-b r: resume a suspended session
	resumeCmd := fmt.Sprintf("%s _resume-menu --port %d", shelljoin(veeBinary), port)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "r", "run-shell", resumeCmd); err != nil {
		return fmt.Errorf("tmux bind-key r: %w", err)
	}

	// Ctrl-b l: show logs in a popup (Esc or q to dismiss)
	logPopupCmd := fmt.Sprintf("%s _log-viewer", shelljoin(veeBinary))
	if _, err := tmuxRun("bind-key", "-T", "prefix", "l", "display-popup", "-E", "-w", "90%", "-h", "80%", logPopupCmd); err != nil {
		return fmt.Errorf("tmux bind-key l: %w", err)
	}

	return nil
}

// buildPickerPopupCmd constructs the shell command for the session picker popup.
func buildPickerPopupCmd(veeBinary string, port int, veePath string, passthrough []string) string {
	var cmdParts []string
	cmdParts = append(cmdParts, shelljoin(veeBinary))
	cmdParts = append(cmdParts, "_session-picker")
	cmdParts = append(cmdParts, "--vee-path", shelljoin(veePath))
	cmdParts = append(cmdParts, "--port", fmt.Sprintf("%d", port))

	if len(passthrough) > 0 {
		cmdParts = append(cmdParts, "--")
		for _, p := range passthrough {
			cmdParts = append(cmdParts, shelljoin(p))
		}
	}

	return strings.Join(cmdParts, " ")
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
