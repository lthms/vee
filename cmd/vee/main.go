package main

import (
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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

	// Set up log ring buffer for the log viewer pane and redirect slog to it
	logBuf := newRingBuffer(10000)
	ringHandler := newSlogRingHandler(logBuf, slog.LevelDebug)
	slog.SetDefault(slog.New(ringHandler))

	app := newApp()

	srv, err := startHTTPServerInBackground(app, cmd.Port, cmd.Zettelkasten)
	if err != nil {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}
	defer srv.Close()

	m := newModel(app, projectConfig, cmd, []string(args), logBuf)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	m.program = p

	_, err = p.Run()
	return err
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

// buildSessionArgs constructs the claude CLI arguments for a session.
func buildSessionArgs(sessionID string, resume bool, mode Mode, projectConfig string, cmd *StartCmd, passthrough []string) []string {
	var args []string
	if mode.NoPrompt {
		args = append(args, passthrough...)
	} else {
		fullPrompt := composeSystemPrompt(mode.Prompt, projectConfig)
		args = buildArgs(passthrough, fullPrompt)
	}

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
