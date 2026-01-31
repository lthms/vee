package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	Prompt      string   // embedded mode prompt content
	PluginDirs  []string // plugin dirs to pass to claude (relative to vee-path)
	NeedsMCP    bool     // whether this mode needs zettelkasten MCP tools
	NoPrompt    bool     // skip --append-system-prompt entirely (vanilla claude)
}

// modeRegistry holds all known modes, keyed by name.
var modeRegistry map[string]Mode

// modeOrder defines the display order for help output.
var modeOrder = []string{"normal", "vibe", "contradictor", "query", "record", "claude"}

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
		pluginDirs  []string
		needsMCP    bool
	}{
		{"normal", "prompts/normal.md", "🦊", "Read-only exploration (default)", nil, false},
		{"vibe", "prompts/vibe.md", "⚡", "Perform tasks with side-effects", nil, false},
		{"contradictor", "prompts/contradictor.md", "😈", "Devil's advocate mode", nil, false},
		{"query", "prompts/zettelkasten_query.md", "🔍", "Query the knowledge base", nil, true},
		{"record", "prompts/zettelkasten_record.md", "📚", "Record into the knowledge base", []string{"plugins/vee-zettelkasten"}, true},
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
			PluginDirs:  m.pluginDirs,
			NeedsMCP:    m.needsMCP,
		}
	}

	// Vanilla Claude mode — no system prompt injection
	modeRegistry["claude"] = Mode{
		Name:        "claude",
		Indicator:   "🤖",
		Description: "Vanilla Claude Code session",
		NoPrompt:    true,
	}

	return nil
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
	SuspendWindow SuspendWindowCmd `cmd:"" name:"_suspend-window" hidden:"" help:"Internal: suspend session by window."`
	ResumeMenu    ResumeMenuCmd    `cmd:"" name:"_resume-menu" hidden:"" help:"Internal: show resume picker."`
	ResumeSession ResumeSessionCmd `cmd:"" name:"_resume-session" hidden:"" help:"Internal: resume a suspended session."`
	SessionEnded  SessionEndedCmd  `cmd:"" name:"_session-ended" hidden:"" help:"Internal: clean up after Claude exits."`
}

// StartCmd runs the in-process server and manages the tmux session.
type StartCmd struct {
	VeePath      string `required:"" type:"path" help:"Path to the vee installation directory." name:"vee-path"`
	Zettelkasten bool   `short:"z" help:"Enable the vee-zettelkasten plugin." name:"zettelkasten"`
	Port         int    `short:"p" help:"Port for the daemon dashboard." default:"2700" name:"port"`
}

// Run starts the Vee tmux session with an in-process HTTP/MCP server.
func (cmd *StartCmd) Run(args claudeArgs) error {
	if err := initModeRegistry(); err != nil {
		return fmt.Errorf("failed to init mode registry: %w", err)
	}

	projectConfig, err := readProjectConfig()
	if err != nil {
		return fmt.Errorf("failed to read project config: %w", err)
	}

	app := newApp()

	srv, err := startHTTPServerInBackground(app, cmd.Port, cmd.Zettelkasten)
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
		Port:          cmd.Port,
		Zettelkasten:  cmd.Zettelkasten,
		Passthrough:   []string(args),
		ProjectConfig: projectConfig,
	})

	// Build the dashboard command
	dashboardShellCmd := fmt.Sprintf("%s _dashboard --port %d", shelljoin(veeBinary), cmd.Port)

	// Create or reuse tmux session
	if !tmuxSessionExists() {
		if err := tmuxCreateSession(dashboardShellCmd); err != nil {
			return fmt.Errorf("failed to create tmux session: %w", err)
		}
	}

	// Apply tmux configuration (idempotent)
	if err := tmuxConfigure(veeBinary, cmd.Port, cmd.VeePath, cmd.Zettelkasten, []string(args)); err != nil {
		return fmt.Errorf("failed to configure tmux: %w", err)
	}

	// Attach to tmux — blocks until detach or session end
	return tmuxAttach()
}

// NewPaneCmd is the internal subcommand called by tmux display-menu entries.
type NewPaneCmd struct {
	VeePath      string `required:"" type:"path" name:"vee-path"`
	Zettelkasten bool   `short:"z" name:"zettelkasten"`
	Port         int    `short:"p" default:"2700" name:"port"`
	Mode         string `required:"" name:"mode"`
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

	// Fetch project config from the running daemon
	projectConfig, err := fetchProjectConfig(cmd.Port)
	if err != nil {
		slog.Warn("failed to fetch project config from daemon, proceeding without", "error", err)
	}

	// Generate session ID
	sessionID := newUUID()

	// Build claude args
	sessionArgs := buildSessionArgs(sessionID, false, mode, projectConfig, &StartCmd{
		VeePath:      cmd.VeePath,
		Zettelkasten: cmd.Zettelkasten,
		Port:         cmd.Port,
	}, []string(args))

	veeBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	shellCmd := buildWindowShellCmd(veeBinary, cmd.Port, sessionID, sessionArgs)
	windowName := fmt.Sprintf("%s %s", mode.Indicator, mode.Name)

	// Create the tmux window first so we have the window ID
	windowID, err := tmuxNewWindow(windowName, shellCmd)
	if err != nil {
		return fmt.Errorf("failed to create tmux window: %w", err)
	}

	// Register session with daemon, including the window target
	if err := registerSession(cmd.Port, sessionID, mode, windowID); err != nil {
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
func registerSession(port int, sessionID string, mode Mode, windowTarget string) error {
	body := fmt.Sprintf(`{"id":%q,"mode":%q,"indicator":%q,"preview":"","window_target":%q}`,
		sessionID, mode.Name, mode.Indicator, windowTarget)

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

	// Fetch config from daemon
	cfg, err := fetchAppConfig(cmd.Port)
	if err != nil {
		return fmt.Errorf("failed to fetch config from daemon: %w", err)
	}

	// Build claude args with --resume
	sessionArgs := buildSessionArgs(cmd.SessionID, true, mode, cfg.ProjectConfig, &StartCmd{
		VeePath:      cfg.VeePath,
		Zettelkasten: cfg.Zettelkasten,
		Port:         cfg.Port,
	}, cfg.Passthrough)

	veeBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	shellCmd := buildWindowShellCmd(veeBinary, cfg.Port, cmd.SessionID, sessionArgs)
	windowName := fmt.Sprintf("%s %s", mode.Indicator, mode.Name)

	windowID, err := tmuxNewWindow(windowName, shellCmd)
	if err != nil {
		return fmt.Errorf("failed to create tmux window: %w", err)
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
//	claude <args>; vee _session-ended --port <port> --session-id <id>
//
// The cleanup tail ensures the daemon is notified when Claude exits for any reason.
func buildWindowShellCmd(veeBinary string, port int, sessionID string, claudeArgs []string) string {
	var cmdParts []string
	cmdParts = append(cmdParts, "claude")
	for _, arg := range claudeArgs {
		cmdParts = append(cmdParts, shelljoin(arg))
	}

	claudeCmd := strings.Join(cmdParts, " ")
	cleanupCmd := fmt.Sprintf("%s _session-ended --port %d --session-id %s",
		shelljoin(veeBinary), port, sessionID)

	return claudeCmd + "; " + cleanupCmd
}

// SessionEndedCmd is called when Claude exits to clean up stale sessions.
type SessionEndedCmd struct {
	Port      int    `short:"p" default:"2700" name:"port"`
	SessionID string `required:"" name:"session-id"`
}

// Run notifies the daemon that a Claude process has exited.
func (cmd *SessionEndedCmd) Run() error {
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

func writeMCPConfig(port int) (string, error) {
	content := fmt.Sprintf(`{"mcpServers":{"vee-daemon":{"type":"sse","url":"http://127.0.0.1:%d/sse"}}}`, port)

	f, err := os.CreateTemp("", "vee-mcp-*.json")
	if err != nil {
		return "", err
	}

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
}

func writeSettings() (string, error) {
	content := `{
  "hooks": {}
}`

	f, err := os.CreateTemp("", "vee-settings-*.json")
	if err != nil {
		return "", err
	}

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
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
func buildSessionArgs(sessionID string, resume bool, mode Mode, projectConfig string, cmd *StartCmd, passthrough []string) []string {
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
	mcpConfigFile, err := writeMCPConfig(cmd.Port)
	if err != nil {
		slog.Error("failed to write MCP config", "error", err)
	} else {
		args = append(args, "--mcp-config", mcpConfigFile)
	}

	// Settings
	settingsFile, err := writeSettings()
	if err != nil {
		slog.Error("failed to write settings", "error", err)
	} else {
		args = append(args, "--settings", settingsFile)
	}

	// Always include plugins/vee for the suspend command
	args = append(args, "--plugin-dir", filepath.Join(cmd.VeePath, "plugins", "vee"))

	// Mode-specific plugin dirs
	for _, dir := range mode.PluginDirs {
		args = append(args, "--plugin-dir", filepath.Join(cmd.VeePath, dir))
	}

	return args
}
