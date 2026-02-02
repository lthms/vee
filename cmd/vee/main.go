package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kong"
)

//go:embed prompts/*.md
var promptFS embed.FS

// Mode describes a Vee operating mode.
type Mode struct {
	Name        string
	Indicator   string
	Description string
	Prompt      string // embedded mode prompt content
	NoPrompt    bool   // skip --append-system-prompt entirely (vanilla claude)
}

// veeLogFile is the fixed log path for the Vee daemon.
const veeLogFile = "/tmp/vee.log"

// modeRegistry holds all known modes, keyed by name.
var modeRegistry map[string]Mode

// modeOrder defines the display order for help output.
var modeOrder = []string{"claude", "normal", "vibe", "contradictor"}

func initModeRegistry() error {
	basePrompt, err := promptFS.ReadFile("prompts/base.md")
	if err != nil {
		return fmt.Errorf("read base prompt: %w", err)
	}

	modes := []struct {
		name        string
		file        string
		indicator   string
		description string
	}{
		{"normal", "prompts/normal.md", "🦊", "Read-only exploration (default)"},
		{"vibe", "prompts/vibe.md", "⚡", "Perform tasks with side-effects"},
		{"contradictor", "prompts/contradictor.md", "😈", "Devil's advocate mode"},
	}

	modeRegistry = make(map[string]Mode, len(modes)+1)
	for _, m := range modes {
		modeContent, err := promptFS.ReadFile(m.file)
		if err != nil {
			return fmt.Errorf("read mode prompt %s: %w", m.file, err)
		}

		// Compose: base + mode
		composed := string(basePrompt) + "\n\n" + string(modeContent)

		modeRegistry[m.name] = Mode{
			Name:        m.name,
			Indicator:   m.indicator,
			Description: m.description,
			Prompt:      composed,
		}
	}

	// Vanilla Claude mode — inject only the KB section so Claude knows about the tools
	kbPrompt := extractSection(string(basePrompt), "<knowledge-base>", "</knowledge-base>")
	modeRegistry["claude"] = Mode{
		Name:        "claude",
		Indicator:   "🤖",
		Description: "Vanilla Claude Code session",
		Prompt:      kbPrompt,
	}

	return nil
}

// extractSection returns the content between start and end markers (inclusive).
func extractSection(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return s[i : i+j+len(end)]
}

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
}

// StartCmd runs the in-process server and manages the tmux session.
type StartCmd struct {
	VeePath string `required:"" type:"path" help:"Path to the vee installation directory." name:"vee-path"`
}

// Run starts the Vee tmux session with an in-process HTTP/MCP server.
func (cmd *StartCmd) Run(args claudeArgs) error {
	// Clean up stale temp directories and old-style temp files from previous runs
	cleanStaleTempFiles()

	if err := initModeRegistry(); err != nil {
		return fmt.Errorf("failed to init mode registry: %w", err)
	}

	projectConfig, err := readProjectConfig()
	if err != nil {
		return fmt.Errorf("failed to read project config: %w", err)
	}

	// Redirect slog to a file so it can be viewed in the logs window
	setupFileLogger(veeLogFile)

	userCfg, err := loadUserConfig()
	if err != nil {
		slog.Warn("failed to load user config, using defaults", "error", err)
		userCfg = &UserConfig{
			Judgment:  JudgmentConfig{URL: "http://localhost:11434", Model: "qwen2.5:7b"},
			Knowledge: KnowledgeConfig{EmbeddingModel: "nomic-embed-text"},
		}
	}

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

	// Resolve own binary path for _new-pane invocations
	veeBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// Store config so _new-pane can fetch it via /api/config
	app.SetConfig(&AppConfig{
		VeePath:       cmd.VeePath,
		Port:          port,
		Passthrough:   []string(args),
		ProjectConfig: projectConfig,
	})

	// Build the dashboard command
	dashboardShellCmd := fmt.Sprintf("%s _dashboard --port %d", shelljoin(veeBinary), port)

	// Create or reuse tmux session
	if tmuxSessionExists() {
		// Kill the stale session so we start fresh with a dashboard
		tmuxRun("kill-session", "-t", "vee")
	}
	if err := tmuxCreateSession(dashboardShellCmd); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Apply tmux configuration (idempotent)
	if err := tmuxConfigure(veeBinary, port, cmd.VeePath, []string(args)); err != nil {
		return fmt.Errorf("failed to configure tmux: %w", err)
	}

	// Attach to tmux — blocks until detach or session end
	err = tmuxAttach()
	// Clear the terminal to suppress tmux's [exited] message
	fmt.Print("\033[H\033[2J")
	return err
}

// NewPaneCmd is the internal subcommand called by tmux display-menu entries.
type NewPaneCmd struct {
	VeePath   string `required:"" type:"path" name:"vee-path"`
	Port      int    `short:"p" default:"2700" name:"port"`
	Mode      string `required:"" name:"mode"`
	Prompt    string `name:"prompt" help:"Initial prompt for the session."`
	Ephemeral bool   `name:"ephemeral" help:"Run session in an ephemeral Docker container."`
	KBIngest  bool   `name:"kb-ingest" help:"Enable KB ingest hook on Task completion."`
}

// Run creates a new tmux window with a Claude session for the given mode.
func (cmd *NewPaneCmd) Run(args claudeArgs) error {
	if err := initModeRegistry(); err != nil {
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

	// Fetch project config from the running daemon
	projectConfig, err := fetchProjectConfig(cmd.Port)
	if err != nil {
		slog.Warn("failed to fetch project config from daemon, proceeding without", "error", err)
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
		shellCmd = buildEphemeralShellCmd(cfg.Ephemeral, sessionID, mode, projectConfig, cmd.Prompt, cmd.Port, cmd.VeePath, veeBinary, []string(args), cmd.KBIngest)
	} else {
		sessionArgs := buildSessionArgs(sessionID, false, mode, projectConfig, cmd.Port, cmd.VeePath, []string(args), veeBinary, cmd.KBIngest)
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

// fetchProjectConfig fetches the project config from the running daemon.
func fetchProjectConfig(port int) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/config", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon returned %d", resp.StatusCode)
	}

	var cfg AppConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return "", err
	}

	return cfg.ProjectConfig, nil
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
	Port     int    `short:"p" default:"2700" name:"port"`
	WindowID string `required:"" name:"window-id"`
}

// Run suspends the session by its tmux window ID.
func (cmd *SuspendWindowCmd) Run() error {
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
	Port     int    `short:"p" default:"2700" name:"port"`
	WindowID string `required:"" name:"window-id"`
}

// Run marks the session as completed by its tmux window ID.
func (cmd *CompleteWindowCmd) Run() error {
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
	Port int `short:"p" default:"2700" name:"port"`
}

// Run fetches suspended sessions and shows a tmux picker.
func (cmd *ResumeMenuCmd) Run() error {
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
		label := fmt.Sprintf("%s %s", sess.Indicator, sess.Mode)
		if sess.Preview != "" {
			preview := sess.Preview
			if len(preview) > 40 {
				preview = preview[:40] + "..."
			}
			label += "  " + preview
		}

		resumeCmd := fmt.Sprintf("%s _resume-session --port %d --session-id %s --mode %s",
			shelljoin(veeBinary), cmd.Port, sess.ID, sess.Mode)

		args = append(args, label, "", "run-shell "+shelljoin(resumeCmd))
	}

	_, err = tmuxRun(args...)
	return err
}

// ResumeSessionCmd resumes a suspended session in a new tmux window.
type ResumeSessionCmd struct {
	Port      int    `short:"p" default:"2700" name:"port"`
	SessionID string `required:"" name:"session-id"`
	Mode      string `required:"" name:"mode"`
}

// Run resumes a suspended session.
func (cmd *ResumeSessionCmd) Run() error {
	if err := initModeRegistry(); err != nil {
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

	// Fetch config from daemon
	cfg, err := fetchAppConfig(cmd.Port)
	if err != nil {
		return fmt.Errorf("failed to fetch config from daemon: %w", err)
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
	sessionArgs := buildSessionArgs(cmd.SessionID, true, mode, cfg.ProjectConfig, cfg.Port, cfg.VeePath, cfg.Passthrough, veeBinary, kbIngest)

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
	cleanupCmd := fmt.Sprintf("%s _session-ended --port %d --session-id %s",
		shelljoin(veeBinary), port, sessionID)

	return "printf '\\033[?25h'; " + claudeCmd + "; " + cleanupCmd
}

// SessionEndedCmd is called when Claude exits to clean up stale sessions.
type SessionEndedCmd struct {
	Port        int    `short:"p" default:"2700" name:"port"`
	SessionID   string `required:"" name:"session-id"`
	WaitForUser bool   `name:"wait-for-user" help:"Pause for user input before closing (used for ephemeral sessions)."`
}

// Run notifies the daemon that a Claude process has exited and cleans up temp files.
func (cmd *SessionEndedCmd) Run() error {
	setupFileLogger(veeLogFile)

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
	Port int `short:"p" default:"2700" name:"port"`
}

func (cmd *ShutdownCmd) Run() error {
	setupFileLogger(veeLogFile)
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

	// Kill the tmux session
	slog.Debug("shutdown: killing tmux session")
	tmuxRun("kill-session", "-t", "vee")
	return nil
}

// UpdatePreviewCmd is the hook handler that reads the user prompt from stdin
// and updates the session preview via the daemon API.
type UpdatePreviewCmd struct {
	Port      int    `short:"p" default:"2700" name:"port"`
	SessionID string `required:"" name:"session-id"`
}

// Run reads the hook JSON from stdin, extracts the prompt, and POSTs it to the daemon.
func (cmd *UpdatePreviewCmd) Run() error {
	setupFileLogger(veeLogFile)
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
}

// Run reads the hook JSON from stdin, extracts permission_mode and prompt,
// then POSTs the combined state update to the daemon.
func (cmd *UpdateWindowCmd) Run() error {
	setupFileLogger(veeLogFile)

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
	Port      int    `short:"p" default:"2700" name:"port"`
	SessionID string `required:"" name:"session-id"`
}

// Run reads the PostToolUse hook JSON from stdin and fires it to the daemon.
func (cmd *KBIngestCmd) Run() error {
	setupFileLogger(veeLogFile)

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
	return filepath.Join(os.TempDir(), "vee-"+sessionID)
}

// cleanStaleTempFiles removes leftover session temp dirs and old-style temp files.
func cleanStaleTempFiles() {
	tmpDir := os.TempDir()

	// Session temp directories: /tmp/vee-UUID/
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "vee-") && len(name) > 10 {
			path := filepath.Join(tmpDir, name)
			slog.Debug("cleanup: removing stale temp dir", "path", path)
			os.RemoveAll(path)
		}
	}

	// Old-style temp files from before the per-session dir refactor
	for _, pattern := range []string{"vee-mcp-*.json", "vee-settings-*.json"} {
		matches, _ := filepath.Glob(filepath.Join(tmpDir, pattern))
		for _, m := range matches {
			slog.Debug("cleanup: removing old-style temp file", "path", m)
			os.Remove(m)
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

func composeSystemPrompt(base, projectConfig string) string {
	if projectConfig == "" {
		return base
	}

	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\n<project_setup>\n")
	sb.WriteString(projectConfig)
	sb.WriteString("\n</project_setup>\n")
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

	updateBase := fmt.Sprintf("%s _update-window --port %d --session-id %s",
		veeBinary, port, sessionID)

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
		kbIngestCmd := fmt.Sprintf("%s _kb-ingest --port %d --session-id %s", veeBinary, port, sessionID)
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
func buildSessionArgs(sessionID string, resume bool, mode Mode, projectConfig string, port int, veePath string, passthrough []string, veeBinary string, kbIngest bool) []string {
	var args []string

	if resume {
		args = append(args, stripSystemPrompt(passthrough)...)
		args = append(args, "--resume", sessionID)
	} else {
		if mode.NoPrompt {
			args = append(args, passthrough...)
		} else {
			fullPrompt := composeSystemPrompt(mode.Prompt, projectConfig)
			args = buildArgs(passthrough, fullPrompt)
		}
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
