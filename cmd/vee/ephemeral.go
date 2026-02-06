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
	Dockerfile string
	Compose    string
	Env        []string
	ExtraArgs  []string
	Mounts     []MountSpec
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
func buildEphemeralShellCmd(cfg *EphemeralConfig, sessionID string, mode Mode, projectConfig, identityRule, platformsRule, feedbackBlock, prompt string, port int, veePath, veeBinary string, passthrough []string) string {
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

	// Build the claude CLI arguments (system prompt + session ID + MCP + settings)
	var claudeArgs []string
	fullPrompt := composeSystemPrompt(mode.Prompt, identityRule, platformsRule, feedbackBlock, projectConfig, true)
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

	// Docker run arguments
	var runParts []string
	runParts = append(runParts, "docker", "run", "--rm", "-it", "--init")
	runParts = append(runParts, "--entrypoint", "''")
	runParts = append(runParts, "--name", shelljoin("vee-"+sessionID))
	runParts = append(runParts, "--add-host", "host.docker.internal:host-gateway")

	// Mount the session temp dir (MCP config + settings)
	tmpDir := sessionTempDir(sessionID)
	runParts = append(runParts, "-v", shelljoin(tmpDir+":"+tmpDir+":ro"))

	// Mount the vee installation directory for plugins
	runParts = append(runParts, "-v", shelljoin(veePath+":/opt/vee:ro"))

	// Environment variables
	runParts = append(runParts, "-e", "IS_SANDBOX=1")
	for _, env := range cfg.Env {
		runParts = append(runParts, "-e", shelljoin(env))
	}

	// Extra args (passed verbatim)
	for _, arg := range cfg.ExtraArgs {
		runParts = append(runParts, shelljoin(arg))
	}

	// User mounts
	type overlayMount struct {
		target string
		lower  string
		upper  string
		work   string
	}
	var overlayMounts []overlayMount

	for i, m := range cfg.Mounts {
		src := expandHome(m.Source)
		switch m.Mount {
		case "ro":
			runParts = append(runParts, "-v", shelljoin(src+":"+m.Target+":ro"))
		case "rw":
			runParts = append(runParts, "-v", shelljoin(src+":"+m.Target))
		default: // "overlay" or empty
			base := fmt.Sprintf("/overlay/%d", i)
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
		}
	}

	if len(overlayMounts) > 0 {
		runParts = append(runParts, "--cap-add", "SYS_ADMIN")
	}

	// Image tag
	runParts = append(runParts, shelljoin(tag))

	// If overlay mounts are present, wrap the command in sh -c to set up
	// overlayfs before exec'ing claude. The mount script runs inside the
	// container; "$@" forwards all remaining args to claude.
	if len(overlayMounts) > 0 {
		var mountCmds []string
		for _, om := range overlayMounts {
			mountCmds = append(mountCmds, fmt.Sprintf(
				"mkdir -p %s %s %s && mount -t overlay overlay -o lowerdir=%s,upperdir=%s,workdir=%s %s",
				om.target, om.upper, om.work, om.lower, om.upper, om.work, om.target,
			))
		}
		script := strings.Join(mountCmds, " && ") + ` && exec "$@"`
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

	// Cleanup command — runs on host after Docker exits
	cleanupCmd := fmt.Sprintf("%s _session-ended --port %d --tmux-socket %s --session-id %s --wait-for-user",
		shelljoin(veeBinary), port, tmuxSocketName, sessionID)

	return "printf '\\033[?25h'; " + buildCmd + " && " + runCmd + "; " + cleanupCmd
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
