package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// tmuxSocketName is the tmux socket name for this instance (set at startup).
var tmuxSocketName = "vee"

// tmuxSessionName is the tmux session name (always "vee" — one session per socket).
var tmuxSessionName = "vee"

// veeRuntimeDir returns the base runtime directory for this vee instance.
// Uses $XDG_RUNTIME_DIR/vee, falling back to /run/user/<uid>/vee.
func veeRuntimeDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, "vee")
}

// tmuxSocketPath returns the full path to the tmux socket file.
func tmuxSocketPath() string {
	return filepath.Join(veeRuntimeDir(), tmuxSocketName)
}

// ensureRuntimeDir creates the vee runtime directory if it doesn't exist.
func ensureRuntimeDir() error {
	return os.MkdirAll(veeRuntimeDir(), 0700)
}

// tmuxCmd builds an exec.Cmd for tmux using the instance socket path.
func tmuxCmd(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-S", tmuxSocketPath()}, args...)...)
}

// tmuxRun executes a tmux command and returns its combined output.
func tmuxRun(args ...string) (string, error) {
	cmd := tmuxCmd(args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// tmuxSessionExists checks whether the tmux session exists.
func tmuxSessionExists() bool {
	cmd := tmuxCmd("has-session", "-t", tmuxSessionName)
	return cmd.Run() == nil
}

// tmuxCreateSession creates a new detached tmux session with a given window 0 command.
func tmuxCreateSession(windowCmd string) error {
	_, err := tmuxRun("new-session", "-d", "-s", tmuxSessionName, "-n", "dashboard", windowCmd)
	return err
}

// tmuxAttach attaches to the tmux session, taking over the current terminal.
// This call blocks until the user detaches or the session ends.
func tmuxAttach() error {
	cmd := tmuxCmd("attach-session", "-t", tmuxSessionName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// tmuxNewWindow creates a new window in the tmux session running shellCmd.
// Returns the tmux window ID (e.g. "@3") which is stable across renumbering.
func tmuxNewWindow(name, shellCmd string) (string, error) {
	return tmuxRun("new-window", "-t", tmuxSessionName, "-n", name, "-P", "-F", "#{window_id}", shellCmd)
}

// tmuxKillWindow kills a specific window in the tmux session.
func tmuxKillWindow(target string) error {
	_, err := tmuxRun("kill-window", "-t", tmuxSessionName+":"+target)
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
	bgName := tmuxSessionName + "-bg"
	if cmd := tmuxCmd("has-session", "-t", bgName); cmd.Run() != nil {
		tmuxRun("new-session", "-d", "-s", bgName)
	}
	tmuxRun("move-window", "-s", windowID, "-t", bgName+":")
	tmuxRun("select-window", "-t", tmuxSessionName+":0")

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
// projectDir is the absolute path shown in the status bar.
func tmuxConfigure(veeBinary string, port int, veePath string, passthrough []string, projectDir string) error {
	// Window status format strings with dynamic indicators.
	// Layout: ⏣ ⊙ #W [working/notif] [perm]
	// Badges (⏣ ephemeral, ⊙ kb-ingest) are always shown: colored when active, dim (#565f89) when not.
	// Per-window @vee-* user options drive the conditionals:
	//   @vee-ephemeral:  ⏣ yellow #f9e2af / dim
	//   @vee-kb-ingest:  ⊙ teal #73daca / dim
	//   @vee-working:    ✱ (orange #ff9e64) — mutually exclusive with notif (working wins)
	//   @vee-notif:      ♪ (blue #7aa2f7)
	//   @vee-perm:       ⏸ for "plan" (yellow #e0af68), ⏵⏵ for "acceptEdits" (violet #bb9af7)
	windowStatusFmt := ` #{?#{@vee-ephemeral},#[fg=#f9e2af]⏣#[fg=default],#[fg=#565f89]⏣#[fg=default]} #{?#{@vee-kb-ingest},#[fg=#73daca]⊙#[fg=default],#[fg=#565f89]⊙#[fg=default]} #W#{?#{@vee-working},#[fg=#ff9e64] ✱#[fg=default],#{?#{@vee-notif},#[fg=#7aa2f7] ♪#[fg=default],}}#{?#{==:#{@vee-perm},plan},#[fg=#e0af68] ⏸#[fg=default],}#{?#{==:#{@vee-perm},acceptEdits},#[fg=#bb9af7] ⏵⏵#[fg=default],} `
	windowStatusCurrentFmt := `#[bg=#414868,fg=#a9b1d6] #{?#{@vee-ephemeral},#[fg=#f9e2af]⏣#[fg=#a9b1d6],#[fg=#565f89]⏣#[fg=#a9b1d6]} #{?#{@vee-kb-ingest},#[fg=#73daca]⊙#[fg=#a9b1d6],#[fg=#565f89]⊙#[fg=#a9b1d6]} #W#{?#{@vee-working},#[fg=#ff9e64] ✱#[fg=#a9b1d6],#{?#{@vee-notif},#[fg=#7aa2f7] ♪#[fg=#a9b1d6],}}#{?#{==:#{@vee-perm},plan},#[fg=#e0af68] ⏸#[fg=#a9b1d6],}#{?#{==:#{@vee-perm},acceptEdits},#[fg=#bb9af7] ⏵⏵#[fg=#a9b1d6],} #[default]`

	// Each entry is a slice of tmux set-option/bind-key args.
	commands := [][]string{
		// True color support
		{"set-option", "-t", tmuxSessionName, "-g", "default-terminal", "tmux-256color"},
		{"set-option", "-t", tmuxSessionName, "-ga", "terminal-overrides", ",*256col*:Tc,alacritty:Tc,xterm-kitty:Tc"},

		// Status bar — Tokyo Night theme
		{"set-option", "-t", tmuxSessionName, "-g", "status-style", "bg=#1a1b26,fg=#a9b1d6"},
		{"set-option", "-t", tmuxSessionName, "-g", "status-left", ""},
		{"set-option", "-t", tmuxSessionName, "-g", "status-right", " " + filepath.Base(projectDir) + " "},
		{"set-option", "-t", tmuxSessionName, "-g", "status-interval", "1"},
		{"set-option", "-t", tmuxSessionName, "-g", "window-status-format", windowStatusFmt},
		{"set-option", "-t", tmuxSessionName, "-g", "window-status-current-format", windowStatusCurrentFmt},

		// Window behavior
		{"set-option", "-t", tmuxSessionName, "-g", "allow-rename", "off"},
		{"set-option", "-t", tmuxSessionName, "-g", "mouse", "on"},
		{"set-option", "-t", tmuxSessionName, "-g", "history-limit", "50000"},

		// Renumber windows on close so indices stay compact
		{"set-option", "-t", tmuxSessionName, "-g", "renumber-windows", "on"},
	}

	for _, args := range commands {
		if _, err := tmuxRun(args...); err != nil {
			return fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
		}
	}

	// Dashboard window (0) gets a simpler status format without ⏣/⊙ badges
	dashboardTarget := tmuxSessionName + ":0"
	tmuxRun("set-option", "-w", "-t", dashboardTarget, "window-status-format", " #W ")
	tmuxRun("set-option", "-w", "-t", dashboardTarget, "window-status-current-format", "#[bg=#414868,fg=#a9b1d6] #W #[default]")

	// Rebind Ctrl-b c to the session picker popup
	pickerCmd := buildPickerPopupCmd(veeBinary, port, veePath, passthrough)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "c", "display-popup", "-E", "-w", "80", "-h", "20", pickerCmd); err != nil {
		return fmt.Errorf("tmux bind-key picker: %w", err)
	}

	// Ctrl-b q: suspend the session in the current window
	suspendCmd := fmt.Sprintf("%s _suspend-window --port %d --tmux-socket %s --window-id #{window_id}", shelljoin(veeBinary), port, tmuxSocketName)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "q", "run-shell", suspendCmd); err != nil {
		return fmt.Errorf("tmux bind-key q: %w", err)
	}

	// Ctrl-b k: complete (kill) the session in the current window
	completeCmd := fmt.Sprintf("%s _complete-window --port %d --tmux-socket %s --window-id #{window_id}", shelljoin(veeBinary), port, tmuxSocketName)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "k", "run-shell", completeCmd); err != nil {
		return fmt.Errorf("tmux bind-key k: %w", err)
	}

	// Ctrl-b x: exit (suspend all sessions, clean up, kill tmux)
	shutdownCmd := fmt.Sprintf("%s _shutdown --port %d --tmux-socket %s", shelljoin(veeBinary), port, tmuxSocketName)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "x", "run-shell", shutdownCmd); err != nil {
		return fmt.Errorf("tmux bind-key x: %w", err)
	}

	// Ctrl-b r: resume a suspended session
	resumeCmd := fmt.Sprintf("%s _resume-menu --port %d --tmux-socket %s", shelljoin(veeBinary), port, tmuxSocketName)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "r", "run-shell", resumeCmd); err != nil {
		return fmt.Errorf("tmux bind-key r: %w", err)
	}

	// Ctrl-b l: show logs in a popup (Esc or q to dismiss)
	logPopupCmd := fmt.Sprintf("%s _log-viewer --tmux-socket %s", shelljoin(veeBinary), tmuxSocketName)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "l", "display-popup", "-E", "-w", "90%", "-h", "80%", logPopupCmd); err != nil {
		return fmt.Errorf("tmux bind-key l: %w", err)
	}

	// Ctrl-b /: knowledge base explorer popup
	explorerCmd := fmt.Sprintf("%s _kb-explorer --port %d", shelljoin(veeBinary), port)
	if _, err := tmuxRun("bind-key", "-T", "prefix", "/", "display-popup", "-E", "-w", "90", "-h", "30", explorerCmd); err != nil {
		return fmt.Errorf("tmux bind-key /: %w", err)
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
	cmdParts = append(cmdParts, "--tmux-socket", tmuxSocketName)

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
