package main

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/lthms/vee/internal/feedback"
	"github.com/lthms/vee/internal/kb"
)

//go:embed prompts/*.md
var promptFS embed.FS

// Profile describes a Vee behavioral profile.
type Profile struct {
	Name              string
	Indicator         string
	Description       string
	Priority          int
	Prompt            string // composed system prompt content (wrapped body, no base.md)
	DefaultPrompt     string // template for the initial prompt (optional)
	PromptPlaceholder string // hint text for the picker's prompt field (optional)
}

// profileRegistry holds all known profiles, keyed by name.
var profileRegistry map[string]Profile

// profileOrder defines the display order, populated by initProfileRegistry.
var profileOrder []string

// CLI is the top-level command structure for vee.
type CLI struct {
	Debug     bool      `env:"VEE_DEBUG" help:"Enable debug logging."`
	Profile   string    `help:"Behavioral profile name." short:"p"`
	KB        bool      `help:"Enable knowledge base." name:"kb"`
	Feedback  bool      `help:"Enable feedback system."`
	Ephemeral bool      `help:"Run in ephemeral Docker container."`
	VeePath   string    `type:"path" help:"Path to vee installation." name:"vee-path"`
	Run       RunCmd    `cmd:"" default:"withargs" help:"Run a Claude session."`
	Resume    ResumeCmd `cmd:"" help:"Resume a previous session."`
}

// RunCmd is the default command — starts a new Claude session.
type RunCmd struct {
	Prompt []string `arg:"" optional:""`
}

// ResumeCmd resumes a previous Claude session.
// Flags --kb, --feedback, and --profile are inherited from the parent CLI struct.
type ResumeCmd struct {
	SessionID string `arg:"" required:"" help:"Session ID to resume."`
}

func main() {
	veeArgs, claudePassthrough := splitAtDashDash(os.Args[1:])

	cli := CLI{}
	parser, err := kong.New(&cli,
		kong.Name("vee"),
		kong.Description("A Claude Code wrapper with behavioral profiles."),
		kong.UsageOnError(),
		kong.DefaultEnvars("VEE"),
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

	switch ctx.Command() {
	case "resume <session-id>":
		err = runResume(&cli, claudePassthrough)
	case "run", "run <prompt>":
		err = runDefault(&cli, claudePassthrough)
	default:
		err = runDefault(&cli, claudePassthrough)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "vee: %v\n", err)
		os.Exit(1)
	}
}

// runDefault implements the default (no subcommand) flow.
func runDefault(cli *CLI, passthrough []string) error {
	// Validate: --feedback requires --profile
	if cli.Feedback && cli.Profile == "" {
		return fmt.Errorf("--feedback requires --profile (feedback is recorded per-profile)")
	}

	// Default VeePath
	veePath := cli.VeePath
	if veePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		veePath = filepath.Join(home, ".local", "share", "vee")
	}
	veePath, _ = filepath.Abs(veePath)

	// Load configs
	userCfg, err := loadUserConfig()
	if err != nil {
		slog.Warn("failed to load user config, using defaults", "error", err)
		userCfg = hydrateUserConfig(nil)
	}

	// Resolve identity + platforms rules
	var projectIdentity *IdentityConfig
	var platRule string
	if projCfg, err := readProjectTOML(); err == nil {
		projectIdentity = projCfg.Identity
		platRule = platformsRule(projCfg.Platforms)
	}
	resolvedIdentity := resolveIdentity(userCfg.Identity, projectIdentity)
	if err := validateIdentity(resolvedIdentity); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	idRule := identityRule(resolvedIdentity)

	// Resolve profile (if requested)
	var profile Profile
	if cli.Profile != "" {
		if err := initProfileRegistry(veePath); err != nil {
			return fmt.Errorf("failed to init profile registry: %w", err)
		}
		var ok bool
		profile, ok = profileRegistry[cli.Profile]
		if !ok {
			return profileNotFoundError(cli.Profile)
		}
	}

	// Read KB prompt (if requested)
	var kbPrompt string
	if cli.KB {
		if data, err := promptFS.ReadFile("prompts/base.md"); err == nil {
			kbPrompt = string(data)
		} else {
			return fmt.Errorf("read KB prompt: %w", err)
		}
	}

	// Sample feedback (if requested)
	var feedbackBlock string
	if cli.Feedback {
		fblock, fstore, err := sampleFeedback(userCfg, profile.Name)
		if err != nil {
			slog.Warn("failed to sample feedback", "error", err)
		} else {
			feedbackBlock = fblock
			if fstore != nil {
				fstore.Close()
			}
		}
	}

	// Read project config
	projectConfig, err := readProjectConfig()
	if err != nil {
		return fmt.Errorf("failed to read project config: %w", err)
	}

	// Read compose file contents for prompt injection (if ephemeral + compose configured)
	isEphemeral := cli.Ephemeral
	var composeContents string
	if isEphemeral {
		if cfg, err := readProjectTOML(); err == nil && cfg.Ephemeral != nil && cfg.Ephemeral.Compose != "" {
			cp := composePath(cfg.Ephemeral)
			if data, err := os.ReadFile(cp); err == nil {
				composeContents = string(data)
			}
		}
	}

	// Compose system prompt
	systemPrompt := composeSystemPrompt(idRule, platRule, kbPrompt, profile.Prompt, feedbackBlock, projectConfig, isEphemeral, composeContents)

	// Generate session ID
	sessionID := newUUID()

	// Start MCP sidecar if KB or feedback is enabled
	var sidecarPort int
	sidecarCtx, sidecarCancel := context.WithCancel(context.Background())
	defer sidecarCancel()

	if cli.KB || cli.Feedback {
		var kbase *kb.KnowledgeBase
		var fstore *feedback.Store

		if cli.KB {
			kbase, err = openKB(userCfg)
			if err != nil {
				return fmt.Errorf("open knowledge base: %w", err)
			}
			defer kbase.Close()
			go kbase.RunWorker(sidecarCtx)
		}

		if cli.Feedback {
			stDir, err := stateDir()
			if err != nil {
				return fmt.Errorf("state dir: %w", err)
			}
			fstore, err = feedback.Open(filepath.Join(stDir, "feedback.db"))
			if err != nil {
				return fmt.Errorf("open feedback store: %w", err)
			}
			defer fstore.Close()
		}

		sidecarPort, err = startMCPSidecar(sidecarCtx, kbase, fstore, profile.Name, isEphemeral)
		if err != nil {
			return fmt.Errorf("start MCP sidecar: %w", err)
		}
	}

	// Build claude args
	claudeArgs := buildArgs(passthrough, systemPrompt)
	claudeArgs = append(claudeArgs, "--session-id", sessionID)

	if sidecarPort > 0 {
		mcpConfigFile, err := writeMCPConfig(sidecarPort, sessionID)
		if err != nil {
			slog.Error("failed to write MCP config", "error", err)
		} else {
			claudeArgs = append(claudeArgs, "--mcp-config", mcpConfigFile)
		}
	}

	if cli.Feedback {
		claudeArgs = append(claudeArgs, "--plugin-dir", filepath.Join(veePath, "plugins", "vee"))
	}

	// Clean up stale temp files from previous runs
	cleanStaleTempFiles()

	// Build prompt string from positional args
	prompt := strings.Join(cli.Run.Prompt, " ")

	if isEphemeral {
		return runEphemeral(sessionID, profile, systemPrompt, claudeArgs, prompt, sidecarPort, veePath, cli.Feedback)
	}

	// Exec into claude
	return execClaude(claudeArgs, prompt)
}

// runResume implements the "resume" subcommand flow.
func runResume(cli *CLI, passthrough []string) error {
	// Validate: --feedback requires --profile
	if cli.Feedback && cli.Profile == "" {
		return fmt.Errorf("--feedback requires --profile on resume (feedback is recorded per-profile)")
	}

	sessionID := cli.Resume.SessionID

	// Start MCP sidecar if KB or feedback is enabled
	var sidecarPort int
	sidecarCtx, sidecarCancel := context.WithCancel(context.Background())
	defer sidecarCancel()

	if cli.KB || cli.Feedback {
		userCfg, err := loadUserConfig()
		if err != nil {
			slog.Warn("failed to load user config, using defaults", "error", err)
			userCfg = hydrateUserConfig(nil)
		}

		var kbase *kb.KnowledgeBase
		var fstore *feedback.Store

		if cli.KB {
			kbase, err = openKB(userCfg)
			if err != nil {
				return fmt.Errorf("open knowledge base: %w", err)
			}
			defer kbase.Close()
			go kbase.RunWorker(sidecarCtx)
		}

		if cli.Feedback {
			stDir, err := stateDir()
			if err != nil {
				return fmt.Errorf("state dir: %w", err)
			}
			fstore, err = feedback.Open(filepath.Join(stDir, "feedback.db"))
			if err != nil {
				return fmt.Errorf("open feedback store: %w", err)
			}
			defer fstore.Close()
		}

		sidecarPort, err = startMCPSidecar(sidecarCtx, kbase, fstore, cli.Profile, false)
		if err != nil {
			return fmt.Errorf("start MCP sidecar: %w", err)
		}
	}

	// Build args: claude --resume <session-id>
	var claudeArgs []string
	claudeArgs = append(claudeArgs, passthrough...)
	claudeArgs = append(claudeArgs, "--resume", sessionID)

	if sidecarPort > 0 {
		mcpConfigFile, err := writeMCPConfig(sidecarPort, sessionID)
		if err != nil {
			slog.Error("failed to write MCP config", "error", err)
		} else {
			claudeArgs = append(claudeArgs, "--mcp-config", mcpConfigFile)
		}
	}

	if cli.Feedback {
		home, _ := os.UserHomeDir()
		veePath := filepath.Join(home, ".local", "share", "vee")
		claudeArgs = append(claudeArgs, "--plugin-dir", filepath.Join(veePath, "plugins", "vee"))
	}

	return execClaude(claudeArgs, "")
}

// execClaude replaces the current process with claude.
func execClaude(args []string, prompt string) error {
	claudePath, err := findClaude()
	if err != nil {
		return err
	}

	var execArgs []string
	execArgs = append(execArgs, "claude")
	if prompt != "" {
		execArgs = append(execArgs, prompt)
	}
	execArgs = append(execArgs, args...)

	return syscall.Exec(claudePath, execArgs, os.Environ())
}

// findClaude locates the claude binary on PATH.
func findClaude() (string, error) {
	path, err := findExecutable("claude")
	if err != nil {
		return "", fmt.Errorf("claude not found on PATH: %w", err)
	}
	return path, nil
}

// findExecutable searches PATH for a named executable, skipping the current binary
// to avoid self-referencing loops.
func findExecutable(name string) (string, error) {
	pathDirs := filepath.SplitList(os.Getenv("PATH"))
	self, _ := os.Executable()
	selfResolved, _ := filepath.EvalSymlinks(self)

	for _, dir := range pathDirs {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			resolved, _ := filepath.EvalSymlinks(candidate)
			if resolved == selfResolved {
				continue
			}
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", name)
}

// profileNotFoundError returns a helpful error when a profile is not found.
func profileNotFoundError(name string) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("unknown profile: %s", name))

	if len(profileOrder) > 0 {
		sb.WriteString("\n\nAvailable profiles:")
		for _, p := range profileOrder {
			prof := profileRegistry[p]
			sb.WriteString(fmt.Sprintf("\n  %s %s — %s", prof.Indicator, prof.Name, prof.Description))
		}

		// Suggest close matches
		var suggestions []string
		for _, p := range profileOrder {
			if strings.Contains(p, name) || strings.Contains(name, p) {
				suggestions = append(suggestions, p)
			}
		}
		if len(suggestions) > 0 {
			sb.WriteString(fmt.Sprintf("\n\nDid you mean: %s?", strings.Join(suggestions, ", ")))
		}
	}

	return fmt.Errorf("%s", sb.String())
}

// sampleFeedback opens the feedback store, samples entries, and returns the
// formatted block. The caller must close the returned store.
func sampleFeedback(userCfg *UserConfig, profile string) (string, *feedback.Store, error) {
	maxExamples := userCfg.Feedback.MaxExamples
	if maxExamples <= 0 {
		return "", nil, nil
	}

	stDir, err := stateDir()
	if err != nil {
		return "", nil, err
	}
	fstore, err := feedback.Open(filepath.Join(stDir, "feedback.db"))
	if err != nil {
		return "", nil, err
	}

	project, _ := filepath.Abs(".")
	entries, err := fstore.Sample(profile, project, maxExamples)
	if err != nil {
		fstore.Close()
		return "", nil, err
	}

	return formatFeedbackBlock(entries), fstore, nil
}

// composeSystemPrompt assembles the full system prompt from its parts.
func composeSystemPrompt(identityRule, platformsRule, kbPrompt, profileBody, feedbackBlock, projectConfig string, ephemeral bool, composeContents string) string {
	var sb strings.Builder

	if identityRule != "" {
		sb.WriteString(identityRule)
	}

	if platformsRule != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(platformsRule)
	}

	if kbPrompt != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(kbPrompt)
	}

	if profileBody != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(profileBody)
	}

	if feedbackBlock != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(feedbackBlock)
	}

	if ephemeral {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("<environment type=\"ephemeral\">\nThis session is ephemeral. Your context will not survive past its end.")
		if composeContents != "" {
			sb.WriteString("\n\nThe following Docker Compose services are available on the container network. You can reach them by service name (e.g., `postgres:5432`).\n\n```yaml\n")
			sb.WriteString(composeContents)
			sb.WriteString("\n```")
		}
		sb.WriteString("\n</environment>")
	} else {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("<environment type=\"host\">\nThis session is run directly on the user's host.\n</environment>")
	}

	if projectConfig != "" {
		sb.WriteString("\n\n<project_setup>\n")
		sb.WriteString(projectConfig)
		sb.WriteString("\n</project_setup>\n")
	}

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

// formatFeedbackBlock renders a list of feedback entries as a <rule> block
// for injection into the system prompt. Returns "" if entries is empty.
func formatFeedbackBlock(entries []feedback.Entry) string {
	if len(entries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<rule object=\"Feedback\">\n")
	sb.WriteString("The following examples have been curated by the user to guide your behavior.\n")
	sb.WriteString("Follow good examples. Avoid bad examples.\n")

	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("\n<example status=%q>\n%s\n</example>\n", e.Kind, e.Statement))
	}

	sb.WriteString("</rule>")
	return sb.String()
}

func writeMCPConfig(port int, sessionID string) (string, error) {
	dir := sessionTempDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	path := filepath.Join(dir, "mcp.json")
	content := fmt.Sprintf(`{"mcpServers":{"vee-daemon":{"type":"sse","url":"http://127.0.0.1:%d/sse"}}}`, port)

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}

	slog.Debug("wrote mcp config", "path", path, "session", sessionID)
	return path, nil
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
