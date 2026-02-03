package main

import (
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"
)

//go:embed prompts/*.md
var promptFS embed.FS

// Mode describes a Vee operating mode.
type Mode struct {
	Name        string
	Indicator   string
	Description string
	Priority    int
	Prompt      string // composed system prompt content
}

// logFilePath returns the log path for this Vee instance.
func logFilePath() string {
	return filepath.Join(veeRuntimeDir(), tmuxSocketName+".log")
}

// instanceSocket computes a unique tmux socket name from the absolute CWD.
func instanceSocket() string {
	abs, err := filepath.Abs(".")
	if err != nil {
		abs = "."
	}
	h := sha256.Sum256([]byte(abs))
	return fmt.Sprintf("vee-%x", h[:8])
}

// discoverDaemonPort reads VEE_PORT from the tmux environment.
func discoverDaemonPort() (int, error) {
	out, err := tmuxRun("show-environment", "VEE_PORT")
	if err != nil {
		return 0, fmt.Errorf("show-environment: %w", err)
	}
	// Output format: "VEE_PORT=12345"
	parts := strings.SplitN(out, "=", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("unexpected output: %s", out)
	}
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil {
		return 0, fmt.Errorf("parse port: %w", err)
	}
	return port, nil
}

// daemonAlive checks whether the daemon is responding on the given port.
func daemonAlive(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitForDaemon polls until the daemon is reachable or the timeout expires.
func waitForDaemon(timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		port, err := discoverDaemonPort()
		if err == nil && daemonAlive(port) {
			return port, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("daemon did not start within %s", timeout)
}

// modeRegistry holds all known modes, keyed by name.
var modeRegistry map[string]Mode

// modeOrder defines the display order, populated by initModeRegistry.
var modeOrder []string

// claudeArgs holds the arguments after "--" that are forwarded to claude.
type claudeArgs []string

// CLI is the top-level command structure for vee.
type CLI struct {
	Debug         bool             `env:"VEE_DEBUG" help:"Enable debug logging."`
	Start         StartCmd         `cmd:"" help:"Start an interactive Vee session."`
	Daemon        DaemonCmd        `cmd:"" help:"Run the Vee daemon (MCP server + dashboard)."`
	NewPane       NewPaneCmd       `cmd:"" name:"_new-pane" hidden:"" help:"Internal: create a new tmux window."`
	Dashboard     DashboardCmd     `cmd:"" name:"_dashboard" hidden:"" help:"Internal: session dashboard TUI."`
	SessionPicker SessionPickerCmd `cmd:"" name:"_session-picker" hidden:"" help:"Internal: interactive mode picker."`
	SuspendWindow  SuspendWindowCmd  `cmd:"" name:"_suspend-window" hidden:"" help:"Internal: suspend session by window."`
	CompleteWindow CompleteWindowCmd `cmd:"" name:"_complete-window" hidden:"" help:"Internal: complete session by window."`
	ResumeMenu    ResumeMenuCmd    `cmd:"" name:"_resume-menu" hidden:"" help:"Internal: show resume picker."`
	ResumeSession ResumeSessionCmd `cmd:"" name:"_resume-session" hidden:"" help:"Internal: resume a suspended session."`
	SessionEnded  SessionEndedCmd  `cmd:"" name:"_session-ended" hidden:"" help:"Internal: clean up after Claude exits."`
	UpdatePreview UpdatePreviewCmd `cmd:"" name:"_update-preview" hidden:"" help:"Internal: update session preview from hook."`
	UpdateWindow  UpdateWindowCmd  `cmd:"" name:"_update-window" hidden:"" help:"Internal: update window state from hook."`
	LogViewer     LogViewerCmd     `cmd:"" name:"_log-viewer" hidden:"" help:"Internal: tail logs in a popup."`
	KBIngest      KBIngestCmd      `cmd:"" name:"_kb-ingest" hidden:"" help:"Internal: KB ingest hook handler."`
	KBExplorer    KBExplorerCmd    `cmd:"" name:"_kb-explorer" hidden:"" help:"Internal: KB explorer TUI."`
	Shutdown      ShutdownCmd      `cmd:"" name:"_shutdown" hidden:"" help:"Internal: graceful shutdown."`
	Serve         ServeCmd         `cmd:"" name:"_serve" hidden:"" help:"Internal: daemon + dashboard inside tmux."`
}

// StartCmd runs the in-process server and manages the tmux session.
type StartCmd struct {
	VeePath string `type:"path" help:"Path to the vee installation directory." name:"vee-path"`
}

// Run starts (or reattaches to) a Vee instance for the current directory.
func (cmd *StartCmd) Run(args claudeArgs) error {
	// Default VeePath to ~/.local/share/vee if not provided
	if cmd.VeePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		cmd.VeePath = filepath.Join(home, ".local", "share", "vee")
	}

	// Compute instance-specific socket name
	socketName := instanceSocket()
	tmuxSocketName = socketName

	// Ensure the tmux socket directory exists
	if err := ensureRuntimeDir(); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Resolve own binary path
	veeBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// If a tmux session already exists for this project, try to reclaim it
	if tmuxSessionExists() {
		port, err := discoverDaemonPort()
		if err == nil && daemonAlive(port) {
			// Daemon is alive — just reattach
			err = tmuxAttach()
			fmt.Print("\033[H\033[2J")
			return err
		}
		// Stale session — clean up
		tmuxRun("kill-session", "-t", tmuxSessionName)
	}

	// Clean up stale temp directories from previous runs
	cleanStaleTempFiles()

	// Build the _serve command for window 0
	absVeePath, _ := filepath.Abs(cmd.VeePath)
	serveShellCmd := fmt.Sprintf("%s _serve --vee-path %s --tmux-socket %s",
		shelljoin(veeBinary), shelljoin(absVeePath), socketName)
	if len(args) > 0 {
		serveShellCmd += " --"
		for _, a := range args {
			serveShellCmd += " " + shelljoin(a)
		}
	}

	// Create tmux session with _serve in window 0
	if err := tmuxCreateSession(serveShellCmd); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Wait for the daemon to come up
	if _, err := waitForDaemon(10 * time.Second); err != nil {
		return fmt.Errorf("daemon failed to start: %w", err)
	}

	// Attach to tmux — blocks until detach or session end
	err = tmuxAttach()
	fmt.Print("\033[H\033[2J")

	if err != nil && !tmuxSessionExists() {
		// Server was killed (e.g. Ctrl-b x shutdown). The _shutdown command
		// runs inside tmux via run-shell, so it gets killed before it can
		// clean up the socket file. Do it here instead.
		os.Remove(tmuxSocketPath())
		return nil
	}

	return err
}

// ServeCmd is the internal command that runs inside tmux window 0.
// It starts the daemon, configures tmux, and runs the dashboard.
type ServeCmd struct {
	VeePath    string `required:"" type:"path" name:"vee-path"`
	TmuxSocket string `required:"" name:"tmux-socket"`
}

// Run starts the daemon, publishes the port, configures tmux, and runs the dashboard.
func (cmd *ServeCmd) Run(args claudeArgs) error {
	tmuxSocketName = cmd.TmuxSocket

	if err := initModeRegistry(cmd.VeePath); err != nil {
		return fmt.Errorf("failed to init mode registry: %w", err)
	}

	projectConfig, err := readProjectConfig()
	if err != nil {
		return fmt.Errorf("failed to read project config: %w", err)
	}

	setupFileLogger(logFilePath())

	userCfg, err := loadUserConfig()
	if err != nil {
		slog.Warn("failed to load user config, using defaults", "error", err)
		userCfg = &UserConfig{
			Judgment:  JudgmentConfig{URL: "http://localhost:11434", Model: "claude:haiku"},
			Knowledge: KnowledgeConfig{EmbeddingModel: "nomic-embed-text"},
		}
	}

	// Resolve identity: merge user + project configs
	var projectIdentity *IdentityConfig
	if projTOML, err := readProjectTOML(); err == nil {
		projectIdentity = projTOML.Identity
	}
	resolvedIdentity := resolveIdentity(userCfg.Identity, projectIdentity)
	if err := validateIdentity(resolvedIdentity); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	idRule := identityRule(resolvedIdentity)

	kbase, judgmentModel, err := openKB(userCfg)
	if err != nil {
		return fmt.Errorf("failed to open knowledge base: %w", err)
	}
	defer kbase.Close()

	app := newApp()

	srv, port, err := startHTTPServerInBackground(app, kbase, judgmentModel)
	if err != nil {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}
	defer srv.Close()

	// Publish port so StartCmd can discover it on reattach
	tmuxRun("set-environment", "VEE_PORT", fmt.Sprintf("%d", port))

	veeBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	app.SetConfig(&AppConfig{
		VeePath:       cmd.VeePath,
		Port:          port,
		Passthrough:   []string(args),
		ProjectConfig: projectConfig,
		IdentityRule:  idRule,
	})

	// Resolve project directory for status bar
	projectDir, _ := filepath.Abs(".")

	// Apply tmux configuration
	if err := tmuxConfigure(veeBinary, port, cmd.VeePath, []string(args), projectDir); err != nil {
		return fmt.Errorf("failed to configure tmux: %w", err)
	}

	// Run dashboard inline — blocks until the session ends
	return (&DashboardCmd{Port: port}).Run()
}

// NewPaneCmd is the internal subcommand called by tmux display-menu entries.
type NewPaneCmd struct {
	VeePath    string `required:"" type:"path" name:"vee-path"`
	Port       int    `short:"p" default:"2700" name:"port"`
	Mode       string `required:"" name:"mode"`
	Prompt     string `name:"prompt" help:"Initial prompt for the session."`
	Ephemeral  bool   `name:"ephemeral" help:"Run session in an ephemeral Docker container."`
	KBIngest   bool   `name:"kb-ingest" help:"Enable KB ingest hook on Task completion."`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run creates a new tmux window with a Claude session for the given mode.
func (cmd *NewPaneCmd) Run(args claudeArgs) error {
	tmuxSocketName = cmd.TmuxSocket
	if err := initModeRegistry(cmd.VeePath); err != nil {
		return fmt.Errorf("failed to init mode registry: %w", err)
	}

	mode, ok := modeRegistry[cmd.Mode]
	if !ok {
		return fmt.Errorf("unknown mode: %s", cmd.Mode)
	}

	veeBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// Fetch config from the running daemon
	appCfg, err := fetchAppConfig(cmd.Port)
	if err != nil {
		slog.Warn("failed to fetch config from daemon, proceeding without", "error", err)
		appCfg = &AppConfig{}
	}

	// Generate session ID
	sessionID := newUUID()

	var shellCmd string
	if cmd.Ephemeral {
		cfg, err := readProjectTOML()
		if err != nil {
			return fmt.Errorf("failed to read .vee/config.toml: %w", err)
		}
		if cfg.Ephemeral == nil {
			return fmt.Errorf("no [ephemeral] section in .vee/config.toml")
		}
		shellCmd = buildEphemeralShellCmd(cfg.Ephemeral, sessionID, mode, appCfg.ProjectConfig, appCfg.IdentityRule, cmd.Prompt, cmd.Port, cmd.VeePath, veeBinary, []string(args), cmd.KBIngest)
	} else {
		sessionArgs := buildSessionArgs(sessionID, false, mode, appCfg.ProjectConfig, appCfg.IdentityRule, cmd.Port, cmd.VeePath, []string(args), veeBinary, cmd.KBIngest)
		shellCmd = buildWindowShellCmd(veeBinary, cmd.Port, sessionID, sessionArgs, cmd.Prompt)
	}

	windowName := fmt.Sprintf("%s %s", mode.Indicator, mode.Name)

	// Create the tmux window first so we have the window ID
	windowID, err := tmuxNewWindow(windowName, shellCmd)
	if err != nil {
		return fmt.Errorf("failed to create tmux window: %w", err)
	}

	// Set @vee-ephemeral on the window if this is an ephemeral session
	if cmd.Ephemeral {
		tmuxSetWindowOption(windowID, "vee-ephemeral", "1")
	}

	// Set @vee-kb-ingest on the window if KB ingest is enabled
	if cmd.KBIngest {
		tmuxSetWindowOption(windowID, "vee-kb-ingest", "1")
	}

	// Register session with daemon, including the window target
	if err := registerSession(cmd.Port, sessionID, mode, windowID, cmd.Ephemeral, cmd.KBIngest); err != nil {
		slog.Warn("failed to register session with daemon", "error", err)
	}

	return nil
}

// registerSession registers a new session with the running daemon.
func registerSession(port int, sessionID string, mode Mode, windowTarget string, ephemeral, kbIngest bool) error {
	body := fmt.Sprintf(`{"id":%q,"mode":%q,"indicator":%q,"preview":"","window_target":%q,"ephemeral":%t,"kb_ingest":%t}`,
		sessionID, mode.Name, mode.Indicator, windowTarget, ephemeral, kbIngest)

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/sessions", port),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("daemon returned %d", resp.StatusCode)
	}

	return nil
}

// SuspendWindowCmd suspends the session running in a given tmux window.
type SuspendWindowCmd struct {
	Port       int    `short:"p" default:"2700" name:"port"`
	WindowID   string `required:"" name:"window-id"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run suspends the session by its tmux window ID.
func (cmd *SuspendWindowCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	body := fmt.Sprintf(`{"window_target":%q}`, cmd.WindowID)

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/suspend", cmd.Port),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No session in this window (e.g. dashboard) — show a tmux message
		tmuxRun("display-message", "No session to suspend in this window")
		return nil
	}

	return nil
}

// CompleteWindowCmd marks the session running in a given tmux window as completed.
type CompleteWindowCmd struct {
	Port       int    `short:"p" default:"2700" name:"port"`
	WindowID   string `required:"" name:"window-id"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run marks the session as completed by its tmux window ID.
func (cmd *CompleteWindowCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	body := fmt.Sprintf(`{"window_target":%q}`, cmd.WindowID)

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/complete", cmd.Port),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		tmuxRun("display-message", "No session to complete in this window")
		return nil
	}

	return nil
}

// ResumeMenuCmd shows a tmux display-menu of suspended sessions.
type ResumeMenuCmd struct {
	Port       int    `short:"p" default:"2700" name:"port"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run fetches suspended sessions and shows a tmux picker.
func (cmd *ResumeMenuCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	// Fetch state from daemon
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", cmd.Port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var state struct {
		Suspended []*Session `json:"suspended_sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return err
	}

	if len(state.Suspended) == 0 {
		tmuxRun("display-message", "No suspended sessions")
		return nil
	}

	veeBinary, err := os.Executable()
	if err != nil {
		return err
	}

	// Build display-menu command
	args := []string{"display-menu", "-T", "Resume Session"}

	for _, sess := range state.Suspended {
		label := fmt.Sprintf("⏣ ⊙ %s %s", sess.Indicator, sess.Mode)
		if sess.Preview != "" {
			preview := sess.Preview
			if len(preview) > 40 {
				preview = preview[:40] + "..."
			}
			label += "  " + preview
		}

		resumeCmd := fmt.Sprintf("%s _resume-session --port %d --session-id %s --mode %s --tmux-socket %s",
			shelljoin(veeBinary), cmd.Port, sess.ID, sess.Mode, tmuxSocketName)

		args = append(args, label, "", "run-shell "+shelljoin(resumeCmd))
	}

	_, err = tmuxRun(args...)
	return err
}

// ResumeSessionCmd resumes a suspended session in a new tmux window.
type ResumeSessionCmd struct {
	Port       int    `short:"p" default:"2700" name:"port"`
	SessionID  string `required:"" name:"session-id"`
	Mode       string `required:"" name:"mode"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run resumes a suspended session.
func (cmd *ResumeSessionCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket

	veeBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// Fetch config from daemon (needed for VeePath before loading modes).
	cfg, err := fetchAppConfig(cmd.Port)
	if err != nil {
		return fmt.Errorf("failed to fetch config from daemon: %w", err)
	}

	if err := initModeRegistry(cfg.VeePath); err != nil {
		return fmt.Errorf("failed to init mode registry: %w", err)
	}

	mode, ok := modeRegistry[cmd.Mode]
	if !ok {
		// Mode file may have been removed since the session was created.
		// Construct a minimal fallback — --resume strips the system prompt
		// anyway, so the body isn't needed.
		mode = Mode{
			Name:      cmd.Mode,
			Indicator: "?",
		}
		// Try to recover the indicator from the stored session.
		if sess, err := fetchSession(cmd.Port, cmd.SessionID); err == nil {
			mode.Indicator = sess.Indicator
		}
	}

	// Fetch session state from daemon to get KBIngest flag
	sess, err := fetchSession(cmd.Port, cmd.SessionID)
	var kbIngest bool
	if err != nil {
		slog.Warn("failed to fetch session from daemon, assuming no kb-ingest", "error", err)
	} else {
		kbIngest = sess.KBIngest
	}

	// Build claude args with --resume
	sessionArgs := buildSessionArgs(cmd.SessionID, true, mode, cfg.ProjectConfig, cfg.IdentityRule, cfg.Port, cfg.VeePath, cfg.Passthrough, veeBinary, kbIngest)

	shellCmd := buildWindowShellCmd(veeBinary, cfg.Port, cmd.SessionID, sessionArgs, "")
	windowName := fmt.Sprintf("%s %s", mode.Indicator, mode.Name)

	windowID, err := tmuxNewWindow(windowName, shellCmd)
	if err != nil {
		return fmt.Errorf("failed to create tmux window: %w", err)
	}

	// Set @vee-kb-ingest on the window if KB ingest is enabled
	if kbIngest {
		tmuxSetWindowOption(windowID, "vee-kb-ingest", "1")
	}

	// Activate the session with the new window target
	activateBody := fmt.Sprintf(`{"session_id":%q,"window_target":%q}`, cmd.SessionID, windowID)
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/activate", cfg.Port),
		"application/json",
		strings.NewReader(activateBody),
	)
	if err != nil {
		slog.Warn("failed to activate session", "error", err)
	} else {
		resp.Body.Close()
	}

	return nil
}

// buildWindowShellCmd constructs the shell command for a tmux window:
//
//	claude <args> [prompt]; vee _session-ended --port <port> --session-id <id>
//
// The cleanup tail ensures the daemon is notified when Claude exits for any reason.
func buildWindowShellCmd(veeBinary string, port int, sessionID string, claudeArgs []string, prompt string) string {
	var cmdParts []string
	cmdParts = append(cmdParts, "claude")
	if prompt != "" {
		cmdParts = append(cmdParts, shelljoin(prompt))
	}
	for _, arg := range claudeArgs {
		cmdParts = append(cmdParts, shelljoin(arg))
	}

	claudeCmd := strings.Join(cmdParts, " ")
	cleanupCmd := fmt.Sprintf("%s _session-ended --port %d --tmux-socket %s --session-id %s",
		shelljoin(veeBinary), port, tmuxSocketName, sessionID)

	return "printf '\\033[?25h'; " + claudeCmd + "; " + cleanupCmd
}

// SessionEndedCmd is called when Claude exits to clean up stale sessions.
type SessionEndedCmd struct {
	Port        int    `short:"p" default:"2700" name:"port"`
	SessionID   string `required:"" name:"session-id"`
	WaitForUser bool   `name:"wait-for-user" help:"Pause for user input before closing (used for ephemeral sessions)."`
	TmuxSocket  string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run notifies the daemon that a Claude process has exited and cleans up temp files.
func (cmd *SessionEndedCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	setupFileLogger(logFilePath())

	if cmd.WaitForUser {
		fmt.Print("\n\033[1mPress Enter to close...\033[0m")
		buf := make([]byte, 1)
		os.Stdin.Read(buf)

		// Defensive cleanup: remove container if --rm didn't catch it
		dockerRm := exec.Command("docker", "rm", "-f", "vee-"+cmd.SessionID)
		dockerRm.Run() // ignore errors — container is likely already gone
	}

	// Clean up per-session temp directory
	dir := sessionTempDir(cmd.SessionID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("session-ended: failed to remove temp dir", "dir", dir, "error", err)
	} else {
		slog.Debug("session-ended: cleaned up temp dir", "dir", dir)
	}

	body := fmt.Sprintf(`{"session_id":%q}`, cmd.SessionID)

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/session-ended", cmd.Port),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		// Daemon might already be gone (e.g. Ctrl-b q killed everything)
		return nil
	}
	defer resp.Body.Close()
	return nil
}

// ShutdownCmd gracefully shuts down the Vee session: suspends all active
// sessions so they can be resumed later, cleans up temp files, then kills tmux.
type ShutdownCmd struct {
	Port       int    `short:"p" default:"2700" name:"port"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

func (cmd *ShutdownCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	setupFileLogger(logFilePath())
	slog.Debug("shutdown: starting graceful shutdown")

	// Fetch state from the daemon
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", cmd.Port))
	if err == nil {
		defer resp.Body.Close()

		var state struct {
			Active   []*Session     `json:"active_sessions"`
			Indexing []IndexingTask `json:"indexing_tasks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&state); err == nil {
			// Warn if indexing is in progress
			if len(state.Indexing) > 0 {
				slog.Debug("shutdown: indexing in progress", "count", len(state.Indexing))
				msg := fmt.Sprintf("%d note(s) are being indexed. Quit anyway?", len(state.Indexing))
				// Use tmux confirm-before to ask the user
				out, confirmErr := tmuxRun("confirm-before", "-p", msg+" (y/n)", "run-shell 'exit 0'")
				if confirmErr != nil {
					slog.Debug("shutdown: user cancelled due to indexing warning", "output", out)
					return nil
				}
			}

			slog.Debug("shutdown: handling active sessions", "count", len(state.Active))
			for _, sess := range state.Active {
				if sess.Ephemeral {
					// Ephemeral sessions cannot be suspended — complete and kill container
					slog.Debug("shutdown: completing ephemeral session", "id", sess.ID, "mode", sess.Mode)
					body := fmt.Sprintf(`{"window_target":%q}`, sess.WindowTarget)
					r, err := http.Post(
						fmt.Sprintf("http://127.0.0.1:%d/api/complete", cmd.Port),
						"application/json",
						strings.NewReader(body),
					)
					if err == nil {
						r.Body.Close()
					}
					dockerKill := exec.Command("docker", "kill", "vee-"+sess.ID)
					dockerKill.Run() // ignore errors
				} else {
					slog.Debug("shutdown: suspending session", "id", sess.ID, "mode", sess.Mode, "window", sess.WindowTarget)
					body := fmt.Sprintf(`{"window_target":%q}`, sess.WindowTarget)
					r, err := http.Post(
						fmt.Sprintf("http://127.0.0.1:%d/api/suspend", cmd.Port),
						"application/json",
						strings.NewReader(body),
					)
					if err == nil {
						r.Body.Close()
					}
				}
			}
		}
	} else {
		slog.Warn("shutdown: failed to fetch state from daemon", "error", err)
	}

	// Clean up all temp dirs
	slog.Debug("shutdown: cleaning stale temp files")
	cleanStaleTempFiles()

	// Kill the entire tmux server for this socket. Each vee instance has
	// its own socket, so this is safe and also cleans up the background
	// session ("vee-bg") used by tmuxGracefulClose.
	slog.Debug("shutdown: killing tmux server")
	tmuxRun("kill-server")
	return nil
}

// UpdatePreviewCmd is the hook handler that reads the user prompt from stdin
// and updates the session preview via the daemon API.
type UpdatePreviewCmd struct {
	Port       int    `short:"p" default:"2700" name:"port"`
	SessionID  string `required:"" name:"session-id"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run reads the hook JSON from stdin, extracts the prompt, and POSTs it to the daemon.
func (cmd *UpdatePreviewCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	setupFileLogger(logFilePath())
	var hookData struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&hookData); err != nil {
		slog.Debug("update-preview: failed to decode hook stdin", "error", err)
		return nil
	}

	if hookData.Prompt == "" {
		slog.Debug("update-preview: empty prompt, skipping")
		return nil
	}

	preview := hookData.Prompt
	if len(preview) > 200 {
		preview = preview[:200]
	}

	slog.Debug("update-preview: posting preview", "session", cmd.SessionID, "preview", preview)

	body, _ := json.Marshal(map[string]string{
		"session_id": cmd.SessionID,
		"preview":    preview,
	})

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/preview", cmd.Port),
		"application/json",
		strings.NewReader(string(body)),
	)
	if err != nil {
		slog.Debug("update-preview: failed to post preview", "error", err)
		return nil
	}
	resp.Body.Close()
	return nil
}

// UpdateWindowCmd is the hook handler that reads Claude hook JSON from stdin
// and updates the session's dynamic window state via the daemon API.
type UpdateWindowCmd struct {
	Port            int    `short:"p" default:"2700" name:"port"`
	SessionID       string `required:"" name:"session-id"`
	Working         bool   `name:"working" help:"Set working=true (Claude is processing)."`
	NoWorking       bool   `name:"no-working" help:"Set working=false (Claude stopped)."`
	Notification    bool   `name:"notification" help:"Set notification=true."`
	NoNotification  bool   `name:"no-notification" help:"Clear notification flag."`
	OnlyOnInterrupt bool   `name:"only-on-interrupt" help:"Only apply the update when the hook payload contains is_interrupt=true."`
	TmuxSocket      string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run reads the hook JSON from stdin, extracts permission_mode and prompt,
// then POSTs the combined state update to the daemon.
func (cmd *UpdateWindowCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	setupFileLogger(logFilePath())

	var hookData struct {
		PermissionMode string `json:"permission_mode"`
		Prompt         string `json:"prompt"`
		IsInterrupt    bool   `json:"is_interrupt"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&hookData); err != nil {
		slog.Debug("update-window: failed to decode hook stdin", "error", err)
		// Continue with flags only — stdin might be empty for some hooks
	}

	if cmd.OnlyOnInterrupt && !hookData.IsInterrupt {
		slog.Debug("update-window: skipping (only-on-interrupt set but is_interrupt is false)")
		return nil
	}

	// Build the request body
	body := map[string]any{
		"session_id": cmd.SessionID,
	}

	if cmd.Working {
		body["working"] = true
	} else if cmd.NoWorking {
		body["working"] = false
	}

	if cmd.Notification {
		body["notification"] = true
	} else if cmd.NoNotification {
		body["notification"] = false
	}

	if hookData.PermissionMode != "" {
		body["permission_mode"] = hookData.PermissionMode
	}

	if hookData.Prompt != "" {
		preview := hookData.Prompt
		if len(preview) > 200 {
			preview = preview[:200]
		}
		body["preview"] = preview
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	slog.Debug("update-window: posting state", "session", cmd.SessionID, "body", string(payload))

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/window-state", cmd.Port),
		"application/json",
		strings.NewReader(string(payload)),
	)
	if err != nil {
		slog.Debug("update-window: failed to post state", "error", err)
		return nil
	}
	resp.Body.Close()
	return nil
}

// KBIngestCmd is the hook handler called by the PostToolUse hook when a Task
// tool completes and KB ingest is enabled. It reads the hook JSON from stdin,
// extracts relevant fields, and POSTs them to the daemon for async evaluation.
type KBIngestCmd struct {
	Port       int    `short:"p" default:"2700" name:"port"`
	SessionID  string `required:"" name:"session-id"`
	TmuxSocket string `name:"tmux-socket" default:"vee" help:"Tmux socket name."`
}

// Run reads the PostToolUse hook JSON from stdin and fires it to the daemon.
func (cmd *KBIngestCmd) Run() error {
	tmuxSocketName = cmd.TmuxSocket
	setupFileLogger(logFilePath())

	var hookData struct {
		ToolInput struct {
			Prompt       string `json:"prompt"`
			SubagentType string `json:"subagent_type"`
			Description  string `json:"description"`
		} `json:"tool_input"`
		ToolResponse json.RawMessage `json:"tool_response"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&hookData); err != nil {
		slog.Debug("kb-ingest: failed to decode hook stdin", "error", err)
		return nil
	}

	taskPrompt := hookData.ToolInput.Prompt
	if len(taskPrompt) > 4096 {
		taskPrompt = taskPrompt[:4096]
	}
	// tool_response is typically a JSON-encoded string (e.g. "\"actual text\"").
	// Try to unwrap the outer quoting; fall back to the raw JSON bytes.
	var taskResponse string
	if err := json.Unmarshal(hookData.ToolResponse, &taskResponse); err != nil {
		taskResponse = string(hookData.ToolResponse)
	}
	if len(taskResponse) > 16384 {
		taskResponse = taskResponse[:16384]
	}

	body, _ := json.Marshal(map[string]string{
		"session_id":    cmd.SessionID,
		"task_prompt":   taskPrompt,
		"task_response": taskResponse,
		"subagent_type": hookData.ToolInput.SubagentType,
		"description":   hookData.ToolInput.Description,
	})

	slog.Debug("kb-ingest: posting to daemon", "session", cmd.SessionID, "subagent_type", hookData.ToolInput.SubagentType)

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/hook/kb-ingest", cmd.Port),
		"application/json",
		strings.NewReader(string(body)),
	)
	if err != nil {
		slog.Debug("kb-ingest: failed to post", "error", err)
		return nil
	}
	resp.Body.Close()
	return nil
}

// sessionTempDir returns the per-session temp directory path.
func sessionTempDir(sessionID string) string {
	return filepath.Join(veeRuntimeDir(), "session-"+sessionID)
}

// cleanStaleTempFiles removes leftover session temp dirs from the runtime directory.
func cleanStaleTempFiles() {
	rtDir := veeRuntimeDir()
	entries, _ := os.ReadDir(rtDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "session-") {
			path := filepath.Join(rtDir, e.Name())
			slog.Debug("cleanup: removing stale session dir", "path", path)
			os.RemoveAll(path)
		}
	}
}

// splitAtDashDash splits args at the first "--".
// Returns (before, after). The "--" itself is consumed.
func splitAtDashDash(args []string) (before, after []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func main() {
	veeArgs, claudePassthrough := splitAtDashDash(os.Args[1:])

	cli := CLI{}
	parser, err := kong.New(&cli,
		kong.Name("vee"),
		kong.Description("A modal code assistant built on top of Claude Code."),
		kong.UsageOnError(),
		kong.Exit(func(code int) {
			os.Exit(code)
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vee: %v\n", err)
		os.Exit(1)
	}
	ctx, err := parser.Parse(veeArgs)
	parser.FatalIfErrorf(err)

	setupLogger(cli.Debug)

	ctx.Bind(claudeArgs(claudePassthrough))

	err = ctx.Run()
	ctx.FatalIfErrorf(err)
}

func setupLogger(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)
}

// setupFileLogger redirects slog to a file at debug level.
func setupFileLogger(path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		slog.Warn("failed to open log file, keeping stderr", "path", path, "error", err)
		return
	}
	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)
}

func readProjectConfig() (string, error) {
	content, err := os.ReadFile(".vee/config.md")
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read .vee/config.md: %w", err)
	}

	return string(content), nil
}

func composeSystemPrompt(base, identityRule, projectConfig string, ephemeral bool) string {
	var sb strings.Builder
	sb.WriteString(base)

	if identityRule != "" {
		sb.WriteString("\n\n")
		sb.WriteString(identityRule)
	}

	if ephemeral {
		sb.WriteString("\n\n<environment type=\"ephemeral\">\nThis session is ephemeral. Your context will not survive past its end.\n</environment>")
	} else {
		sb.WriteString("\n\n<environment type=\"host\">\nThis session is run directly on the user's host.\n</environment>")
	}

	if projectConfig != "" {
		sb.WriteString("\n\n<project_setup>\n")
		sb.WriteString(projectConfig)
		sb.WriteString("\n</project_setup>\n")
	}

	return sb.String()
}

func writeMCPConfig(port int, sessionID string) (string, error) {
	dir := sessionTempDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	path := filepath.Join(dir, "mcp.json")
	content := fmt.Sprintf(`{"mcpServers":{"vee-daemon":{"type":"sse","url":"http://127.0.0.1:%d/sse?session=%s"}}}`, port, sessionID)

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}

	slog.Debug("wrote mcp config", "path", path, "session", sessionID)
	return path, nil
}

func writeSettings(sessionID string, port int, veeBinary string, kbIngest bool) (string, error) {
	dir := sessionTempDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	updateBase := fmt.Sprintf("%s _update-window --port %d --tmux-socket %s --session-id %s",
		veeBinary, port, tmuxSocketName, sessionID)

	promptSubmitCmd := updateBase + " --working --no-notification"
	stopCmd := updateBase + " --no-working"
	interruptCmd := updateBase + " --no-working --only-on-interrupt"
	notifCmd := updateBase + " --notification"

	settings := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []map[string]any{
				{
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": promptSubmitCmd,
						},
					},
				},
			},
			"Stop": []map[string]any{
				{
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": stopCmd,
						},
					},
				},
			},
			"PostToolUseFailure": []map[string]any{
				{
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": interruptCmd,
						},
					},
				},
			},
			"Notification": []map[string]any{
				{
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": notifCmd,
						},
					},
				},
			},
		},
	}

	if kbIngest {
		kbIngestCmd := fmt.Sprintf("%s _kb-ingest --port %d --tmux-socket %s --session-id %s", veeBinary, port, tmuxSocketName, sessionID)
		hooks := settings["hooks"].(map[string]any)
		hooks["PostToolUse"] = []map[string]any{
			{
				"matcher": "Task",
				"hooks": []map[string]any{
					{
						"type":    "command",
						"command": kbIngestCmd,
					},
				},
			},
		}
	}

	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		return "", err
	}

	slog.Debug("wrote settings", "path", path, "session", sessionID, "hooks", "SessionStart,UserPromptSubmit,Stop,Notification")
	return path, nil
}

func buildArgs(originalArgs []string, systemPromptContent string) []string {
	var args []string
	var userAppendPrompt string
	skipNext := false

	for i, arg := range originalArgs {
		if skipNext {
			skipNext = false
			continue
		}

		if arg == "--append-system-prompt" && i+1 < len(originalArgs) {
			userAppendPrompt = originalArgs[i+1]
			skipNext = true
			continue
		}

		if strings.HasPrefix(arg, "--append-system-prompt=") {
			userAppendPrompt = strings.TrimPrefix(arg, "--append-system-prompt=")
			continue
		}

		args = append(args, arg)
	}

	finalPrompt := systemPromptContent
	if userAppendPrompt != "" {
		finalPrompt = finalPrompt + "\n\n" + userAppendPrompt
	}
	args = append(args, "--append-system-prompt", finalPrompt)

	return args
}

// fetchAppConfig fetches the full AppConfig from the running daemon.
func fetchAppConfig(port int) (*AppConfig, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/config", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned %d", resp.StatusCode)
	}

	var cfg AppConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// fetchSession fetches a single session's state from the running daemon.
func fetchSession(port int, sessionID string) (*Session, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/session?id=%s", port, sessionID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned %d", resp.StatusCode)
	}

	var sess Session
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return nil, err
	}

	return &sess, nil
}

// stripSystemPrompt removes --append-system-prompt and its value from args.
func stripSystemPrompt(args []string) []string {
	var out []string
	skipNext := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--append-system-prompt" && i+1 < len(args) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--append-system-prompt=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// buildSessionArgs constructs the claude CLI arguments for a session.
func buildSessionArgs(sessionID string, resume bool, mode Mode, projectConfig, identityRule string, port int, veePath string, passthrough []string, veeBinary string, kbIngest bool) []string {
	var args []string

	if resume {
		args = append(args, stripSystemPrompt(passthrough)...)
		args = append(args, "--resume", sessionID)
	} else {
		fullPrompt := composeSystemPrompt(mode.Prompt, identityRule, projectConfig, false)
		args = buildArgs(passthrough, fullPrompt)
		args = append(args, "--session-id", sessionID)
	}

	// MCP config — always provided (needed for request_suspend and self_drop)
	mcpConfigFile, err := writeMCPConfig(port, sessionID)
	if err != nil {
		slog.Error("failed to write MCP config", "error", err)
	} else {
		args = append(args, "--mcp-config", mcpConfigFile)
	}

	// Settings (includes per-session UserPromptSubmit hook)
	settingsFile, err := writeSettings(sessionID, port, veeBinary, kbIngest)
	if err != nil {
		slog.Error("failed to write settings", "error", err)
	} else {
		args = append(args, "--settings", settingsFile)
	}

	// Always include plugins/vee for the suspend command
	args = append(args, "--plugin-dir", filepath.Join(veePath, "plugins", "vee"))

	return args
}
