package main

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

//go:embed system_prompt.md
var systemPrompt string

func main() {
	setupLogger()

	// Handle mcp subcommand
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := runMCPServer(); err != nil {
			fmt.Fprintf(os.Stderr, "vee mcp: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "vee: %v\n", err)
		os.Exit(1)
	}
}

func setupLogger() {
	level := slog.LevelInfo
	if os.Getenv("VEE_DEBUG") != "" {
		level = slog.LevelDebug
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)
}

func run() error {
	// Find claude binary
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found in PATH: %w", err)
	}

	// Ensure MCP server is configured
	if err := ensureMCPServer(); err != nil {
		return fmt.Errorf("MCP server check failed: %w", err)
	}

	// Read project config
	projectConfig, err := readProjectConfig()
	if err != nil {
		return fmt.Errorf("failed to read project config: %w", err)
	}

	// Compose full system prompt
	fullPrompt := composeSystemPrompt(systemPrompt, projectConfig)
	slog.Debug("composed system prompt", "content", fullPrompt)

	// Build arguments
	args := buildArgs(os.Args[1:], fullPrompt)
	slog.Debug("built args", "argCount", len(args))

	// Exec into claude
	return syscall.Exec(claudePath, append([]string{"claude"}, args...), os.Environ())
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

	// Parse original args to find --append-system-prompt (we merge it with ours)
	// All other args pass through unchanged, including --plugin-dir
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

	// Compose final system prompt (ours + user's)
	finalPrompt := systemPromptContent
	if userAppendPrompt != "" {
		finalPrompt = finalPrompt + "\n\n" + userAppendPrompt
	}
	args = append(args, "--append-system-prompt", finalPrompt)

	return args
}
