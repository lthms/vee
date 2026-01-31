package main

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"
)

//go:embed system_prompt.md
var systemPrompt string

// claudeArgs holds the arguments after "--" that are forwarded to claude.
type claudeArgs []string


// CLI is the top-level command structure for vee.
type CLI struct {
	Debug bool   `env:"VEE_DEBUG" help:"Enable debug logging."`
	Start StartCmd `cmd:"" help:"Start an interactive Vee session."`
	MCP   MCPCmd `cmd:"" help:"Run the built-in MCP server."`
}

// StartCmd is the default command that execs into claude.
type StartCmd struct {
	VeePath      string `required:"" type:"path" help:"Path to the vee installation directory." name:"vee-path"`
	Zettelkasten bool   `short:"z" help:"Enable the vee-zettelkasten plugin." name:"zettelkasten"`
}

// Run starts a Vee session by exec-ing into claude with the composed system prompt.
func (cmd *StartCmd) Run(args claudeArgs) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found in PATH: %w", err)
	}

	if err := ensureMCPServer(cmd.Zettelkasten); err != nil {
		return fmt.Errorf("MCP server check failed: %w", err)
	}

	projectConfig, err := readProjectConfig()
	if err != nil {
		return fmt.Errorf("failed to read project config: %w", err)
	}

	fullPrompt := composeSystemPrompt(systemPrompt, projectConfig)
	slog.Debug("composed system prompt", "content", fullPrompt)

	finalArgs := buildArgs([]string(args), fullPrompt)
	finalArgs = append(finalArgs, "--plugin-dir", filepath.Join(cmd.VeePath, "plugins", "vee"))
	if cmd.Zettelkasten {
		finalArgs = append(finalArgs, "--plugin-dir", filepath.Join(cmd.VeePath, "plugins", "vee-zettelkasten"))
	}
	slog.Debug("built args", "argCount", len(finalArgs))

	return syscall.Exec(claudePath, append([]string{"claude"}, finalArgs...), os.Environ())
}

// MCPCmd runs the built-in MCP server.
type MCPCmd struct {
	Zettelkasten bool `short:"z" help:"Enable the vee-zettelkasten tools." name:"zettelkasten"`
}

// Run starts the MCP server.
func (cmd *MCPCmd) Run() error {
	return runMCPServer(cmd.Zettelkasten)
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
