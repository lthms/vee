package main

import (
	_ "embed"
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

//go:embed system_prompt.md
var systemPrompt string

// claudeArgs holds the arguments after "--" that are forwarded to claude.
type claudeArgs []string


// CLI is the top-level command structure for vee.
type CLI struct {
	Debug bool   `env:"VEE_DEBUG" help:"Enable debug logging."`
	Start  StartCmd  `cmd:"" help:"Start an interactive Vee session."`
	Daemon DaemonCmd `cmd:"" help:"Run the Vee daemon (MCP server + dashboard)."`
}

// StartCmd supervises the daemon and claude processes.
type StartCmd struct {
	VeePath      string `required:"" type:"path" help:"Path to the vee installation directory." name:"vee-path"`
	Zettelkasten bool   `short:"z" help:"Enable the vee-zettelkasten plugin." name:"zettelkasten"`
	Port         int    `short:"p" help:"Port for the daemon dashboard." default:"2700" name:"port"`
}

// Run starts a Vee session: forks the daemon, starts claude as a child, and
// supervises both. When claude exits, the daemon is killed.
func (cmd *StartCmd) Run(args claudeArgs) error {
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

	// 3. Build claude args
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

	mcpConfigFile, err := writeMCPConfig(cmd.Port)
	if err != nil {
		return fmt.Errorf("failed to write MCP config: %w", err)
	}
	defer os.Remove(mcpConfigFile)

	finalArgs = append(finalArgs, "--mcp-config", mcpConfigFile)

	slog.Debug("built args", "argCount", len(finalArgs))

	// 4. Start claude with terminal passthrough
	claude := exec.Command("claude", finalArgs...)
	claude.Stdin = os.Stdin
	claude.Stdout = os.Stdout
	claude.Stderr = os.Stderr

	if err := claude.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("claude failed: %w", err)
	}

	return nil
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
