package main

import (
	"bufio"
	"embed"
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
	Prompt      string   // embedded mode prompt content
	PluginDirs  []string // plugin dirs to pass to claude (relative to vee-path)
	NeedsMCP    bool     // whether this mode needs the daemon MCP connection
}

// modeRegistry holds all known modes, keyed by name.
var modeRegistry map[string]Mode

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

// StartCmd supervises the daemon and runs the TUI loop.
type StartCmd struct {
	VeePath      string `required:"" type:"path" help:"Path to the vee installation directory." name:"vee-path"`
	Zettelkasten bool   `short:"z" help:"Enable the vee-zettelkasten plugin." name:"zettelkasten"`
	Port         int    `short:"p" help:"Port for the daemon dashboard." default:"2700" name:"port"`
}

// Run starts the Vee TUI: forks the daemon, then enters a readline loop that
// spawns a fresh Claude session for each mode invocation.
func (cmd *StartCmd) Run(args claudeArgs) error {
	if err := initModeRegistry(); err != nil {
		return fmt.Errorf("failed to init mode registry: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// 1. Fork the daemon
	daemonArgs := []string{"daemon", "-p", fmt.Sprintf("%d", cmd.Port)}
	if cmd.Zettelkasten {
		daemonArgs = append(daemonArgs, "-z")
	}
	daemon := exec.Command(self, daemonArgs...)
	daemon.Stdout = nil
	daemon.Stderr = nil
	if err := daemon.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	defer func() {
		slog.Debug("killing daemon", "pid", daemon.Process.Pid)
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	}()
	slog.Debug("daemon started", "pid", daemon.Process.Pid)

	// 2. Wait for the daemon to be ready
	if err := waitForDaemon(cmd.Port); err != nil {
		return fmt.Errorf("daemon not ready: %w", err)
	}

	// 3. Load project config
	projectConfig, err := readProjectConfig()
	if err != nil {
		return fmt.Errorf("failed to read project config: %w", err)
	}

	// 4. Report idle state
	reportMode(cmd.Port, "idle", "💤")

	// 5. TUI loop
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("💤 vee> ")
		if !scanner.Scan() {
			break // EOF
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if line == "quit" || line == "exit" {
			break
		}

		if line == "help" {
			printHelp()
			continue
		}

		// Parse: first word = mode name, rest = initial message
		parts := strings.SplitN(line, " ", 2)
		modeName := parts[0]
		var initialMsg string
		if len(parts) > 1 {
			initialMsg = parts[1]
		}

		mode, ok := modeRegistry[modeName]
		if !ok {
			fmt.Fprintf(os.Stderr, "Unknown mode: %q. Type \"help\" to see available modes.\n", modeName)
			continue
		}

		// Skip zettelkasten modes if not enabled
		if mode.NeedsMCP && !cmd.Zettelkasten {
			fmt.Fprintf(os.Stderr, "Mode %q requires --zettelkasten (-z) flag.\n", modeName)
			continue
		}

		// Report mode change to daemon
		reportMode(cmd.Port, mode.Name, mode.Indicator)

		// Launch session
		if err := runSession(mode, initialMsg, projectConfig, cmd, []string(args)); err != nil {
			slog.Debug("session ended with error", "mode", modeName, "error", err)
		}

		// Report back to idle
		reportMode(cmd.Port, "idle", "💤")
		fmt.Println()
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	return nil
}

func printHelp() {
	fmt.Println("Available modes:")
	// Print in a stable order
	order := []string{"normal", "vibe", "contradictor", "query", "record"}
	for _, name := range order {
		m, ok := modeRegistry[name]
		if !ok {
			continue
		}
		fmt.Printf("  %-14s %s  %s\n", m.Name, m.Indicator, m.Description)
	}
	fmt.Println()
	fmt.Println("Usage: <mode> [initial message]")
	fmt.Println("  quit/exit  — exit vee")
	fmt.Println("  help       — show this message")
}

// runSession spawns a single interactive Claude session for the given mode.
func runSession(mode Mode, initialMsg, projectConfig string, cmd *StartCmd, passthrough []string) error {
	// Compose full system prompt: mode prompt + project config
	fullPrompt := composeSystemPrompt(mode.Prompt, projectConfig)
	slog.Debug("launching session", "mode", mode.Name, "prompt_len", len(fullPrompt))

	var args []string

	// Pass through any extra args from the user (filtering out --append-system-prompt)
	args = buildArgs(passthrough, fullPrompt)

	// MCP config if needed
	if mode.NeedsMCP {
		mcpConfigFile, err := writeMCPConfig(cmd.Port)
		if err != nil {
			return fmt.Errorf("failed to write MCP config: %w", err)
		}
		defer os.Remove(mcpConfigFile)
		args = append(args, "--mcp-config", mcpConfigFile)
	}

	// Settings
	settingsFile, err := writeSettings()
	if err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}
	defer os.Remove(settingsFile)
	args = append(args, "--settings", settingsFile)

	// Plugin dirs
	for _, dir := range mode.PluginDirs {
		args = append(args, "--plugin-dir", filepath.Join(cmd.VeePath, dir))
	}

	// Initial message as positional argument
	if initialMsg != "" {
		args = append(args, initialMsg)
	}

	slog.Debug("claude args", "args", args)

	claude := exec.Command("claude", args...)
	claude.Stdin = os.Stdin
	claude.Stdout = os.Stdout
	claude.Stderr = os.Stderr

	if err := claude.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			slog.Debug("claude exited", "code", exitErr.ExitCode())
			return nil // Normal exit with non-zero code is fine for session end
		}
		return fmt.Errorf("claude failed: %w", err)
	}

	return nil
}

// reportMode POSTs the current mode to the daemon's /api/mode endpoint.
func reportMode(port int, mode, indicator string) {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/mode", port)
	body := fmt.Sprintf(`{"mode":%q,"indicator":%q}`, mode, indicator)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		slog.Debug("failed to report mode", "error", err)
		return
	}
	resp.Body.Close()
}

// waitForDaemon polls the daemon's API until it responds or a timeout is reached.
func waitForDaemon(port int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/state", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for range 50 {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			slog.Debug("daemon ready")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("daemon did not respond on port %d", port)
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
