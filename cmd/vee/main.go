package main

import (
	"bufio"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	Prompt      string   // embedded mode prompt content
	PluginDirs  []string // plugin dirs to pass to claude (relative to vee-path)
	NeedsMCP    bool     // whether this mode needs zettelkasten MCP tools
}

// modeRegistry holds all known modes, keyed by name.
var modeRegistry map[string]Mode

// modeOrder defines the display order for help output.
var modeOrder = []string{"normal", "vibe", "contradictor", "query", "record"}

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

	modeRegistry = make(map[string]Mode, len(modes))
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

	return nil
}

// claudeArgs holds the arguments after "--" that are forwarded to claude.
type claudeArgs []string

// CLI is the top-level command structure for vee.
type CLI struct {
	Debug  bool      `env:"VEE_DEBUG" help:"Enable debug logging."`
	Start  StartCmd  `cmd:"" help:"Start an interactive Vee session."`
	Daemon DaemonCmd `cmd:"" help:"Run the Vee daemon (MCP server + dashboard)."`
}

// StartCmd runs the in-process server and the TUI loop.
type StartCmd struct {
	VeePath      string `required:"" type:"path" help:"Path to the vee installation directory." name:"vee-path"`
	Zettelkasten bool   `short:"z" help:"Enable the vee-zettelkasten plugin." name:"zettelkasten"`
	Port         int    `short:"p" help:"Port for the daemon dashboard." default:"2700" name:"port"`
}

// Run starts the Vee TUI with an in-process HTTP/MCP server.
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

	tuiLoop(app, projectConfig, cmd, []string(args))

	return nil
}

func tuiLoop(app *App, projectConfig string, cmd *StartCmd, passthrough []string) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("💤 vee> ")
		if !scanner.Scan() {
			break
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if !strings.HasPrefix(line, "/") {
			fmt.Fprintf(os.Stderr, "Commands must start with /. Type /help to see available commands.\n")
			continue
		}

		// Strip leading / and parse command + argument
		line = line[1:]
		parts := strings.SplitN(line, " ", 2)
		command := parts[0]
		var argument string
		if len(parts) > 1 {
			argument = parts[1]
		}

		switch command {
		case "quit", "exit":
			return

		case "help":
			printHelp(cmd.Zettelkasten)

		case "sessions", "ls":
			printSessions(app)

		case "resume":
			handleResume(app, argument, projectConfig, cmd, passthrough)

		case "drop":
			handleDrop(app, argument)

		default:
			// Try as mode name
			mode, ok := modeRegistry[command]
			if !ok {
				fmt.Fprintf(os.Stderr, "Unknown command: /%s. Type /help to see available commands.\n", command)
				continue
			}

			if mode.NeedsMCP && !cmd.Zettelkasten {
				fmt.Fprintf(os.Stderr, "Mode %q requires --zettelkasten (-z) flag.\n", command)
				continue
			}

			if err := runSession(app, mode, argument, projectConfig, cmd, passthrough); err != nil {
				slog.Debug("session ended with error", "mode", command, "error", err)
			}

			fmt.Println()
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Error("reading stdin", "error", err)
	}
}

func printHelp(zettelkasten bool) {
	fmt.Println("Modes:")
	for _, name := range modeOrder {
		m, ok := modeRegistry[name]
		if !ok {
			continue
		}
		flag := ""
		if m.NeedsMCP && !zettelkasten {
			flag = " (requires -z)"
		}
		fmt.Printf("  /%-13s %s  %s%s\n", m.Name, m.Indicator, m.Description, flag)
	}
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  /sessions, /ls     — list suspended sessions")
	fmt.Println("  /resume <n>        — resume suspended session by index")
	fmt.Println("  /drop <n>          — drop a suspended session")
	fmt.Println("  /help              — show this message")
	fmt.Println("  /quit, /exit       — exit vee")
}

func printSessions(app *App) {
	sessions := app.Sessions.suspended()
	if len(sessions) == 0 {
		fmt.Println("No suspended sessions.")
		return
	}
	for i, s := range sessions {
		age := time.Since(s.StartedAt).Truncate(time.Second)
		preview := s.Preview
		if preview == "" {
			preview = "(no preview)"
		}
		fmt.Printf("  %d. %s %s — %s (%s ago)\n", i+1, s.Indicator, s.Mode, preview, age)
	}
}

func handleResume(app *App, argument, projectConfig string, cmd *StartCmd, passthrough []string) {
	sessions := app.Sessions.suspended()
	if len(sessions) == 0 {
		fmt.Println("No suspended sessions to resume.")
		return
	}

	if argument == "" {
		fmt.Fprintf(os.Stderr, "Usage: /resume <n> (1-%d)\n", len(sessions))
		return
	}

	idx, err := strconv.Atoi(argument)
	if err != nil || idx < 1 || idx > len(sessions) {
		fmt.Fprintf(os.Stderr, "Invalid index: %s. Use 1-%d.\n", argument, len(sessions))
		return
	}

	sess := sessions[idx-1]
	mode, ok := modeRegistry[sess.Mode]
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown mode %q for session %s.\n", sess.Mode, sess.ID)
		return
	}

	if err := resumeSession(app, mode, sess.ID, projectConfig, cmd, passthrough); err != nil {
		slog.Debug("resume ended with error", "session", sess.ID, "error", err)
	}

	fmt.Println()
}

func handleDrop(app *App, argument string) {
	sessions := app.Sessions.suspended()
	if len(sessions) == 0 {
		fmt.Println("No suspended sessions to drop.")
		return
	}

	if argument == "" {
		fmt.Fprintf(os.Stderr, "Usage: /drop <n> (1-%d)\n", len(sessions))
		return
	}

	idx, err := strconv.Atoi(argument)
	if err != nil || idx < 1 || idx > len(sessions) {
		fmt.Fprintf(os.Stderr, "Invalid index: %s. Use 1-%d.\n", argument, len(sessions))
		return
	}

	sess := sessions[idx-1]
	app.Sessions.drop(sess.ID)
	fmt.Printf("Dropped session: %s %s\n", sess.Indicator, sess.Mode)
}

// runSession spawns a new interactive Claude session for the given mode.
func runSession(app *App, mode Mode, initialMsg, projectConfig string, cmd *StartCmd, passthrough []string) error {
	id := newUUID()

	preview := initialMsg
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	app.Sessions.create(id, mode.Name, mode.Indicator, preview)
	suspendCh, selfDropCh := app.Control.newSession()
	defer app.Control.clearSession()

	args := buildSessionArgs(id, false, mode, projectConfig, cmd, passthrough)
	if initialMsg != "" {
		args = append([]string{initialMsg}, args...)
	}
	return execSession(app, id, args, suspendCh, selfDropCh)
}

// resumeSession resumes an existing suspended session.
func resumeSession(app *App, mode Mode, sessionID, projectConfig string, cmd *StartCmd, passthrough []string) error {
	app.Sessions.setStatus(sessionID, "active")
	suspendCh, selfDropCh := app.Control.newSession()
	defer app.Control.clearSession()

	args := buildSessionArgs(sessionID, true, mode, projectConfig, cmd, passthrough)
	return execSession(app, sessionID, args, suspendCh, selfDropCh)
}

// buildSessionArgs constructs the claude CLI arguments for a session.
func buildSessionArgs(sessionID string, resume bool, mode Mode, projectConfig string, cmd *StartCmd, passthrough []string) []string {
	fullPrompt := composeSystemPrompt(mode.Prompt, projectConfig)

	var args []string
	args = buildArgs(passthrough, fullPrompt)

	if resume {
		args = append(args, "--resume", sessionID)
	} else {
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

// execSession runs a claude process and handles suspend, self-drop, and natural exit.
func execSession(app *App, sessionID string, args []string, suspendCh, selfDropCh chan struct{}) error {
	slog.Debug("claude args", "args", args)

	claude := exec.Command("claude", args...)
	claude.Stdin = os.Stdin
	claude.Stdout = os.Stdout
	claude.Stderr = os.Stderr

	if err := claude.Start(); err != nil {
		app.Sessions.drop(sessionID)
		return fmt.Errorf("claude start failed: %w", err)
	}

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- claude.Wait()
	}()

	select {
	case err := <-doneCh:
		// Natural exit — drop the session
		app.Sessions.drop(sessionID)
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				slog.Debug("claude exited", "code", exitErr.ExitCode())
				return nil
			}
			return fmt.Errorf("claude failed: %w", err)
		}
		return nil

	case <-suspendCh:
		// Suspend requested — send SIGINT and wait for graceful exit
		slog.Debug("suspending session", "id", sessionID)
		_ = claude.Process.Signal(syscall.SIGINT)
		waitOrKill(claude, doneCh)
		app.Sessions.setStatus(sessionID, "suspended")
		fmt.Println("\nSession suspended.")
		return nil

	case <-selfDropCh:
		// Self-drop — task is done, terminate and record as completed
		slog.Debug("self-drop session", "id", sessionID)
		_ = claude.Process.Signal(syscall.SIGINT)
		waitOrKill(claude, doneCh)
		app.Sessions.setStatus(sessionID, "completed")
		fmt.Println("\nSession completed.")
		return nil
	}
}

// waitOrKill waits up to 5 seconds for the process to exit, then force-kills it.
func waitOrKill(cmd *exec.Cmd, doneCh <-chan error) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-doneCh:
	case <-timer.C:
		slog.Debug("force killing process", "pid", cmd.Process.Pid)
		_ = cmd.Process.Kill()
		<-doneCh
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
