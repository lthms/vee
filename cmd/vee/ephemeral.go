package main

import (
	"crypto/sha256"
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

// runEphemeral builds the Docker image, starts optional Compose services, and
// runs Claude inside a container with stdio attached. Cleans up on exit.
func runEphemeral(sessionID string, profile Profile, systemPrompt string, claudeArgs []string, prompt string, sidecarPort int, veePath string, feedbackEnabled bool) error {
	cfg, err := readProjectTOML()
	if err != nil {
		return fmt.Errorf("failed to read .vee/config: %w", err)
	}
	if cfg.Ephemeral == nil {
		return fmt.Errorf("no [ephemeral] section in .vee/config")
	}
	ecfg := cfg.Ephemeral

	tag := ephemeralImageTag()
	df := dockerfilePath(ecfg)

	// Write Docker-specific MCP config to the session temp dir
	var mcpConfigFile string
	if sidecarPort > 0 {
		mcpConfigFile, err = writeMCPConfigDocker(sidecarPort, sessionID)
		if err != nil {
			slog.Error("failed to write docker MCP config", "error", err)
		}
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

	// Validate compose file if configured
	var composeProject string
	if ecfg.Compose != "" {
		cp := composePath(ecfg)
		if err := validateComposeFile(cp); err != nil {
			return fmt.Errorf("compose validation failed: %w", err)
		}
		composeProject = composeProjectName(sessionID)

		// Start compose services
		slog.Info("starting compose services")
		composeUp := exec.Command("docker", "compose", "-f", cp, "-p", composeProject, "up", "-d", "--build")
		composeUp.Stdout = os.Stdout
		composeUp.Stderr = os.Stderr
		if err := composeUp.Run(); err != nil {
			return fmt.Errorf("docker compose up: %w", err)
		}
	}

	// Docker build
	slog.Info("building ephemeral image", "tag", tag)
	buildCmd := exec.Command("docker", "build", "-t", tag, "-f", df, ".")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		cleanupCompose(ecfg, composeProject)
		return fmt.Errorf("docker build: %w", err)
	}

	// Build docker run args
	runArgs := []string{"run", "--rm", "-it", "--init"}
	runArgs = append(runArgs, "--entrypoint", "")
	runArgs = append(runArgs, "--name", "vee-"+sessionID)
	runArgs = append(runArgs, "--add-host", "host.docker.internal:host-gateway")

	// Connect to Compose network when compose is configured
	if ecfg.Compose != "" {
		runArgs = append(runArgs, "--network", composeProject+"_default")
	}

	// Mount the session temp dir (MCP config)
	tmpDir := sessionTempDir(sessionID)
	runArgs = append(runArgs, "-v", tmpDir+":"+tmpDir+":ro")

	// Mount the vee installation directory for plugins
	runArgs = append(runArgs, "-v", veePath+":/opt/vee:ro")

	// Mount the startup script (if configured)
	var startupScriptPath string
	if ecfg.StartupScript != "" {
		startupScriptPath = filepath.Join(".vee", ecfg.StartupScript)
		abs, err := filepath.Abs(startupScriptPath)
		if err == nil {
			startupScriptPath = abs
		}
		if _, err := os.Stat(startupScriptPath); err != nil {
			cleanupCompose(ecfg, composeProject)
			return fmt.Errorf("startup script %s: %w", startupScriptPath, err)
		}
		runArgs = append(runArgs, "-v", startupScriptPath+":/opt/startup.sh:ro")
	}

	// Environment variables
	runArgs = append(runArgs, "-e", "IS_SANDBOX=1")
	for _, env := range ecfg.Env {
		runArgs = append(runArgs, "-e", env)
	}

	// Git identity forwarding (when configured)
	if gitConfigFile != "" {
		runArgs = append(runArgs, "-v", gitConfigFile+":/etc/gitconfig:ro")
	}

	// Extra args (passed verbatim)
	for _, arg := range ecfg.ExtraArgs {
		runArgs = append(runArgs, arg)
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

	// GPG signing uses the sidecar's /api/gpg/sign endpoint via the wrapper script.
	if gpgCfg != nil && sidecarPort > 0 {
		runArgs = append(runArgs, "-e", fmt.Sprintf("VEE_DAEMON_PORT=%d", sidecarPort))
	}

	// User mounts
	for _, m := range ecfg.Mounts {
		src := expandHome(m.Source)
		switch m.Mount {
		case "ro":
			runArgs = append(runArgs, "-v", src+":"+m.Target+":ro")
		case "rw":
			runArgs = append(runArgs, "-v", src+":"+m.Target)
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
			runArgs = append(runArgs, "-v", src+":"+lower+":ro")
			runArgs = append(runArgs, "--tmpfs", base)
			overlayIndex++
		}
	}

	if len(overlayMounts) > 0 {
		runArgs = append(runArgs, "--cap-add", "SYS_ADMIN")
	}

	// Image tag
	runArgs = append(runArgs, tag)

	// Build the claude command args for inside the container
	var containerClaudeArgs []string
	containerClaudeArgs = append(containerClaudeArgs, claudeArgs...)
	if mcpConfigFile != "" {
		// Override MCP config with Docker-specific one
		containerClaudeArgs = stripFlag(containerClaudeArgs, "--mcp-config")
		containerClaudeArgs = append(containerClaudeArgs, "--mcp-config", mcpConfigFile)
	}
	if feedbackEnabled {
		containerClaudeArgs = append(containerClaudeArgs, "--plugin-dir", "/opt/vee/plugins/vee")
	}
	containerClaudeArgs = append(containerClaudeArgs, "--dangerously-skip-permissions")

	// If overlay mounts or a startup script are present, wrap the command
	// in sh -c to run setup steps before exec'ing claude.
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
		runArgs = append(runArgs, "sh", "-c", script, "_")
	}

	// Claude command inside container
	runArgs = append(runArgs, "claude")
	if prompt != "" {
		runArgs = append(runArgs, prompt)
	}
	runArgs = append(runArgs, containerClaudeArgs...)

	// Run docker
	slog.Info("starting ephemeral session", "session", sessionID)
	dockerRun := exec.Command("docker", runArgs...)
	dockerRun.Stdin = os.Stdin
	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	runErr := dockerRun.Run()

	// Cleanup
	cleanupEphemeralSession(sessionID, ecfg, composeProject)

	if runErr != nil {
		return fmt.Errorf("docker run: %w", runErr)
	}
	return nil
}

// cleanupCompose tears down a Compose stack if one was started.
func cleanupCompose(cfg *EphemeralConfig, composeProject string) {
	if cfg.Compose == "" || composeProject == "" {
		return
	}
	cp := composePath(cfg)
	slog.Debug("ephemeral cleanup: compose down", "path", cp, "project", composeProject)
	down := exec.Command("docker", "compose", "-f", cp, "-p", composeProject, "down")
	if err := down.Run(); err != nil {
		slog.Warn("ephemeral cleanup: compose down failed", "error", err)
	}
}

// cleanupEphemeralSession tears down all resources associated with an ephemeral session:
// the Docker container (best-effort), the Compose stack (if any), and the per-session temp directory.
func cleanupEphemeralSession(sessionID string, cfg *EphemeralConfig, composeProject string) {
	// 1. Kill the container (may already be dead from --rm)
	dockerKill := exec.Command("docker", "kill", "vee-"+sessionID)
	dockerKill.Run()
	slog.Debug("ephemeral cleanup: docker kill", "session", sessionID)

	// 2. Tear down Compose stack if one was started
	cleanupCompose(cfg, composeProject)

	// 3. Remove per-session temp directory
	dir := sessionTempDir(sessionID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("ephemeral cleanup: failed to remove temp dir", "session", sessionID, "dir", dir, "error", err)
	} else {
		slog.Debug("ephemeral cleanup: removed temp dir", "session", sessionID, "dir", dir)
	}
}

// stripFlag removes a flag and its value from args.
func stripFlag(args []string, flag string) []string {
	var out []string
	skipNext := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == flag && i+1 < len(args) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, flag+"=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// writeMCPConfigDocker writes an MCP config file that uses host.docker.internal
// for container-to-host communication.
func writeMCPConfigDocker(port int, sessionID string) (string, error) {
	dir := sessionTempDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	path := filepath.Join(dir, "mcp.json")
	content := fmt.Sprintf(`{"mcpServers":{"vee-daemon":{"type":"sse","url":"http://host.docker.internal:%d/sse"}}}`, port)

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}

	slog.Debug("wrote docker mcp config", "path", path, "session", sessionID)
	return path, nil
}

// writeGitConfig writes a minimal .gitconfig file to the session temp dir.
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
