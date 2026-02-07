package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EphemeralConfig holds the [ephemeral] section of .vee/config.
type EphemeralConfig struct {
	Dockerfile    string
	Compose       string
	StartupScript string
	Env           []string
	ExtraArgs     []string
	Mounts        []MountSpec
}

// GitConfig holds the git user identity detected on the host.
type GitConfig struct {
	UserName  string // git config user.name
	UserEmail string // git config user.email
}

// GPGSigningConfig holds the GPG signing configuration detected on the host.
type GPGSigningConfig struct {
	HomeDir    string // gpgconf --list-dirs homedir
	SocketPath string // gpgconf --list-dirs agent-socket
	SigningKey string // git config user.signingkey
	GPGProgram string // git config gpg.program (optional)
}

// MountSpec describes a bind mount for the Docker container.
type MountSpec struct {
	Source string
	Target string
	Mount  string // "overlay" (default), "ro", or "rw"
}

// ephemeralAvailable returns true if .vee/config exists with an [ephemeral]
// section and the docker binary is on PATH. When compose is configured, it
// also verifies that `docker compose` is available.
func ephemeralAvailable() bool {
	cfg, err := readProjectTOML()
	if err != nil {
		return false
	}
	if cfg.Ephemeral == nil {
		return false
	}
	_, err = exec.LookPath("docker")
	if err != nil {
		return false
	}
	if cfg.Ephemeral.Compose != "" {
		out, err := exec.Command("docker", "compose", "version").CombinedOutput()
		if err != nil {
			slog.Debug("docker compose not available", "error", err, "output", string(out))
			return false
		}
	}
	return true
}

// detectGitConfig reads the git user identity from host configuration.
// Returns nil if no identity is configured.
func detectGitConfig() *GitConfig {
	cfg := &GitConfig{}

	out, err := exec.Command("git", "config", "--get", "user.name").Output()
	if err == nil {
		cfg.UserName = strings.TrimSpace(string(out))
	}
	out, err = exec.Command("git", "config", "--get", "user.email").Output()
	if err == nil {
		cfg.UserEmail = strings.TrimSpace(string(out))
	}

	if cfg.UserName == "" && cfg.UserEmail == "" {
		slog.Debug("no git user identity configured")
		return nil
	}

	slog.Debug("detected git config", "user", cfg.UserName, "email", cfg.UserEmail)
	return cfg
}

// detectGPGSigning checks whether the host has GPG signing configured and a
// running agent. Returns nil if any requirement is missing (graceful degradation).
func detectGPGSigning() *GPGSigningConfig {
	// Check if commit.gpgsign is enabled
	out, err := exec.Command("git", "config", "--get", "commit.gpgsign").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		slog.Debug("gpg signing not enabled in git config")
		return nil
	}

	// Get GPG agent socket path
	out, err = exec.Command("gpgconf", "--list-dirs", "agent-socket").Output()
	if err != nil {
		slog.Debug("gpgconf not available", "error", err)
		return nil
	}
	socketPath := strings.TrimSpace(string(out))
	if socketPath == "" {
		slog.Debug("gpg agent socket path is empty")
		return nil
	}

	// Check socket exists
	if _, err := os.Stat(socketPath); err != nil {
		slog.Debug("gpg agent socket not found", "path", socketPath, "error", err)
		return nil
	}

	// Get GPG home directory
	out, err = exec.Command("gpgconf", "--list-dirs", "homedir").Output()
	if err != nil {
		slog.Debug("failed to get gpg homedir", "error", err)
		return nil
	}
	homeDir := strings.TrimSpace(string(out))
	if homeDir == "" {
		slog.Debug("gpg homedir is empty")
		return nil
	}

	cfg := &GPGSigningConfig{
		HomeDir:    homeDir,
		SocketPath: socketPath,
	}

	if out, err = exec.Command("git", "config", "--get", "user.signingkey").Output(); err == nil {
		cfg.SigningKey = strings.TrimSpace(string(out))
	}
	if out, err = exec.Command("git", "config", "--get", "gpg.program").Output(); err == nil {
		cfg.GPGProgram = strings.TrimSpace(string(out))
	}

	slog.Debug("detected gpg signing config",
		"homedir", cfg.HomeDir,
		"socket", cfg.SocketPath,
		"signingkey", cfg.SigningKey,
		"gpg_program", cfg.GPGProgram)

	return cfg
}

// ephemeralImageTag returns a deterministic image tag based on the project root path.
func ephemeralImageTag() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "unknown"
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	h := sha256.Sum256([]byte(abs))
	return fmt.Sprintf("vee-ephemeral-%x", h[:8])
}

// expandHome replaces a leading ~ with $HOME in a path.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home := os.Getenv("HOME")
		if home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// dockerfilePath returns the path to the Dockerfile, defaulting to .vee/Dockerfile.
func dockerfilePath(cfg *EphemeralConfig) string {
	df := cfg.Dockerfile
	if df == "" {
		df = "Dockerfile"
	}
	return filepath.Join(".vee", df)
}

// composePath returns the path to the Compose file, relative to .vee/.
func composePath(cfg *EphemeralConfig) string {
	return filepath.Join(".vee", cfg.Compose)
}

// validateComposeFile runs `docker compose config` as a preflight check.
func validateComposeFile(path string) error {
	cmd := exec.Command("docker", "compose", "-f", path, "config", "--quiet")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("invalid compose file %s: %s", path, strings.TrimSpace(string(out)))
	}
	return nil
}

// composeProjectName derives a Compose-safe project name from a session ID.
// Compose project names must match [a-z0-9][a-z0-9_-]*.
func composeProjectName(sessionID string) string {
	return "vee-" + sessionID
}

// buildEphemeralShellCmd constructs the full shell command for an ephemeral Docker session:
//
//	printf '\033[?25h'; docker build -t <tag> -f .vee/Dockerfile . && docker run --rm -it ... ; vee _session-ended ...
func buildEphemeralShellCmd(cfg *EphemeralConfig, sessionID string, profile Profile, projectConfig, identityRule, platformsRule, feedbackBlock, composeContents, prompt string, port int, veePath, veeBinary string, passthrough []string) (string, error) {
	tag := ephemeralImageTag()
	df := dockerfilePath(cfg)

	// Write Docker-specific MCP config and settings to the session temp dir
	mcpConfigFile, err := writeMCPConfigDocker(port, sessionID)
	if err != nil {
		slog.Error("failed to write docker MCP config", "error", err)
	}
	settingsFile, err := writeEphemeralSettings(sessionID, port)
	if err != nil {
		slog.Error("failed to write ephemeral settings", "error", err)
	}

	// Detect git identity and GPG signing configuration
	gitCfg := detectGitConfig()
	gpgCfg := detectGPGSigning()
	var gitConfigFile string
	if gitCfg != nil {
		gitConfigFile, err = writeGitConfig(sessionID, gitCfg, gpgCfg)
		if err != nil {
			slog.Error("failed to write gitconfig", "error", err)
		}
	}

	// Build the claude CLI arguments (system prompt + session ID + MCP + settings)
	var claudeArgs []string
	fullPrompt := composeSystemPrompt(profile.Prompt, identityRule, platformsRule, feedbackBlock, projectConfig, true, composeContents)
	claudeArgs = buildArgs(passthrough, fullPrompt)
	claudeArgs = append(claudeArgs, "--session-id", sessionID)
	if mcpConfigFile != "" {
		claudeArgs = append(claudeArgs, "--mcp-config", mcpConfigFile)
	}
	if settingsFile != "" {
		claudeArgs = append(claudeArgs, "--settings", settingsFile)
	}
	claudeArgs = append(claudeArgs, "--plugin-dir", "/opt/vee/plugins/vee")
	claudeArgs = append(claudeArgs, "--dangerously-skip-permissions")

	// Build command
	buildCmd := fmt.Sprintf("docker build -t %s -f %s .", shelljoin(tag), shelljoin(df))

	// Compose lifecycle prefix
	var composeUpCmd string
	project := composeProjectName(sessionID)
	if cfg.Compose != "" {
		cp := composePath(cfg)
		composeUpCmd = fmt.Sprintf("docker compose -f %s -p %s up -d --build",
			shelljoin(cp), shelljoin(project))
	}

	// Docker run arguments
	var runParts []string
	runParts = append(runParts, "docker", "run", "--rm", "-it", "--init")
	runParts = append(runParts, "--entrypoint", "''")
	runParts = append(runParts, "--name", shelljoin("vee-"+sessionID))
	runParts = append(runParts, "--add-host", "host.docker.internal:host-gateway")

	// Connect to Compose network when compose is configured
	if cfg.Compose != "" {
		runParts = append(runParts, "--network", shelljoin(project+"_default"))
	}

	// Mount the session temp dir (MCP config + settings)
	tmpDir := sessionTempDir(sessionID)
	runParts = append(runParts, "-v", shelljoin(tmpDir+":"+tmpDir+":ro"))

	// Mount the vee installation directory for plugins
	runParts = append(runParts, "-v", shelljoin(veePath+":/opt/vee:ro"))

	// Mount the startup script (if configured)
	var startupScriptPath string
	if cfg.StartupScript != "" {
		startupScriptPath = filepath.Join(".vee", cfg.StartupScript)
		abs, err := filepath.Abs(startupScriptPath)
		if err == nil {
			startupScriptPath = abs
		}
		if _, err := os.Stat(startupScriptPath); err != nil {
			return "", fmt.Errorf("startup script %s: %w", startupScriptPath, err)
		}
		runParts = append(runParts, "-v", shelljoin(startupScriptPath+":/opt/startup.sh:ro"))
	}

	// Environment variables
	runParts = append(runParts, "-e", "IS_SANDBOX=1")
	for _, env := range cfg.Env {
		runParts = append(runParts, "-e", shelljoin(env))
	}

	// Git identity forwarding (when configured)
	if gitConfigFile != "" {
		runParts = append(runParts, "-v", shelljoin(gitConfigFile+":/etc/gitconfig:ro"))
	}

	// Extra args (passed verbatim)
	for _, arg := range cfg.ExtraArgs {
		runParts = append(runParts, shelljoin(arg))
	}

	// Overlay mounts (user mounts + GPG homedir)
	type overlayMount struct {
		target string
		lower  string
		upper  string
		work   string
	}
	var overlayMounts []overlayMount
	overlayIndex := 0

	// GPG signing uses the daemon's /api/gpg/sign endpoint via the wrapper script.
	// Set the daemon port so the wrapper knows where to connect.
	if gpgCfg != nil {
		runParts = append(runParts, "-e", shelljoin(fmt.Sprintf("VEE_DAEMON_PORT=%d", port)))
	}

	// User mounts
	for _, m := range cfg.Mounts {
		src := expandHome(m.Source)
		switch m.Mount {
		case "ro":
			runParts = append(runParts, "-v", shelljoin(src+":"+m.Target+":ro"))
		case "rw":
			runParts = append(runParts, "-v", shelljoin(src+":"+m.Target))
		default: // "overlay" or empty
			base := fmt.Sprintf("/overlay/%d", overlayIndex)
			lower := base + "/lower"
			upper := base + "/upper"
			work := base + "/work"
			overlayMounts = append(overlayMounts, overlayMount{
				target: m.Target,
				lower:  lower,
				upper:  upper,
				work:   work,
			})
			runParts = append(runParts, "-v", shelljoin(src+":"+lower+":ro"))
			runParts = append(runParts, "--tmpfs", shelljoin(base))
			overlayIndex++
		}
	}

	if len(overlayMounts) > 0 {
		runParts = append(runParts, "--cap-add", "SYS_ADMIN")
	}

	// Image tag
	runParts = append(runParts, shelljoin(tag))

	// If overlay mounts or a startup script are present, wrap the command
	// in sh -c to run setup steps before exec'ing claude. The script runs
	// inside the container; "$@" forwards all remaining args to claude.
	needsWrapper := len(overlayMounts) > 0 || startupScriptPath != ""
	if needsWrapper {
		var wrapperCmds []string
		for _, om := range overlayMounts {
			wrapperCmds = append(wrapperCmds, fmt.Sprintf(
				"mkdir -p %s %s %s && mount -t overlay overlay -o lowerdir=%s,upperdir=%s,workdir=%s %s",
				om.target, om.upper, om.work, om.lower, om.upper, om.work, om.target,
			))
		}
		if startupScriptPath != "" {
			wrapperCmds = append(wrapperCmds, "sh /opt/startup.sh")
		}
		script := strings.Join(wrapperCmds, " && ") + ` && exec "$@"`
		runParts = append(runParts, "sh", "-c", shelljoin(script), "_")
	}

	// Claude command inside container
	runParts = append(runParts, "claude")
	if prompt != "" {
		runParts = append(runParts, shelljoin(prompt))
	}
	for _, arg := range claudeArgs {
		runParts = append(runParts, shelljoin(arg))
	}

	runCmd := strings.Join(runParts, " ")

	// Cleanup command — runs on host after Docker exits.
	// Docker/compose teardown is handled by the daemon via cleanupEphemeralSession.
	cleanupCmd := fmt.Sprintf("%s _session-ended --port %d --tmux-socket %s --session-id %s --wait-for-user",
		shelljoin(veeBinary), port, tmuxSocketName, sessionID)

	// Assemble the full command chain
	var chain string
	if composeUpCmd != "" {
		chain = "printf '\\033[?25h'; " + composeUpCmd + " && " + buildCmd + " && " + runCmd + "; " + cleanupCmd
	} else {
		chain = "printf '\\033[?25h'; " + buildCmd + " && " + runCmd + "; " + cleanupCmd
	}
	return chain, nil
}

// writeMCPConfigDocker writes an MCP config file that uses host.docker.internal
// for container-to-host communication.
func writeMCPConfigDocker(port int, sessionID string) (string, error) {
	dir := sessionTempDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	path := filepath.Join(dir, "mcp.json")
	content := fmt.Sprintf(`{"mcpServers":{"vee-daemon":{"type":"sse","url":"http://host.docker.internal:%d/sse?session=%s"}}}`, port, sessionID)

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}

	slog.Debug("wrote docker mcp config", "path", path, "session", sessionID)
	return path, nil
}

// writeEphemeralSettings writes a settings file with curl-based hooks suitable
// for use inside a Docker container (no vee binary required, just curl).
// Uses a shell pipeline that reads stdin once, enriches it with flags, and
// POSTs the combined JSON to /api/hook/window-state.
func writeEphemeralSettings(sessionID string, port int) (string, error) {
	dir := sessionTempDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	baseURL := fmt.Sprintf("http://host.docker.internal:%d/api/hook/window-state?session=%s",
		port, sessionID)

	// UserPromptSubmit: merge working=true, notification=false into the hook JSON, then POST
	promptSubmitCmd := fmt.Sprintf(
		`jq -c '. + {"working":true,"notification":false}' | curl -sf -X POST '%s' -H 'Content-Type: application/json' -d @-`,
		baseURL)

	// Stop: merge working=false into the hook JSON, then POST
	stopCmd := fmt.Sprintf(
		`jq -c '. + {"working":false}' | curl -sf -X POST '%s' -H 'Content-Type: application/json' -d @-`,
		baseURL)

	// PostToolUseFailure: clear working only when is_interrupt is true
	interruptCmd := fmt.Sprintf(
		`jq -ce 'select(.is_interrupt == true) | . + {"working":false}' | curl -sf -X POST '%s' -H 'Content-Type: application/json' -d @-`,
		baseURL)

	// Notification: merge notification=true into the hook JSON, then POST
	notifCmd := fmt.Sprintf(
		`jq -c '. + {"notification":true}' | curl -sf -X POST '%s' -H 'Content-Type: application/json' -d @-`,
		baseURL)

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

	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		return "", err
	}

	slog.Debug("wrote ephemeral settings", "path", path, "session", sessionID, "hooks", "UserPromptSubmit,Stop,PostToolUseFailure,Notification")
	return path, nil
}

// writeGitConfig writes a minimal .gitconfig file to the session temp dir.
// It always includes user.name and user.email from gitCfg. When gpgCfg is
// provided, it also adds signing configuration (signingkey, commit.gpgsign,
// and optionally gpg.program).
func writeGitConfig(sessionID string, gitCfg *GitConfig, gpgCfg *GPGSigningConfig) (string, error) {
	dir := sessionTempDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("[user]\n")
	if gitCfg.UserName != "" {
		b.WriteString(fmt.Sprintf("\tname = %s\n", gitCfg.UserName))
	}
	if gitCfg.UserEmail != "" {
		b.WriteString(fmt.Sprintf("\temail = %s\n", gitCfg.UserEmail))
	}

	// Add GPG signing configuration if available
	// Uses the wrapper script that delegates to the Vee daemon's /api/gpg/sign endpoint
	if gpgCfg != nil {
		if gpgCfg.SigningKey != "" {
			b.WriteString(fmt.Sprintf("\tsigningkey = %s\n", gpgCfg.SigningKey))
		}
		b.WriteString("[commit]\n")
		b.WriteString("\tgpgsign = true\n")
		b.WriteString("[gpg]\n")
		b.WriteString("\tprogram = /opt/vee/scripts/gpg-sign-wrapper\n")
	}

	// Allow all directories to avoid "dubious ownership" errors in containers
	b.WriteString("[safe]\n")
	b.WriteString("\tdirectory = *\n")

	path := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		return "", err
	}

	slog.Debug("wrote gitconfig", "path", path, "session", sessionID, "gpg", gpgCfg != nil)
	return path, nil
}
