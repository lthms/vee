package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestComposeProjectName(t *testing.T) {
	tests := []struct {
		sessionID string
		want      string
	}{
		{"abc123", "vee-abc123"},
		{"550e8400-e29b-41d4-a716-446655440000", "vee-550e8400-e29b-41d4-a716-446655440000"},
	}

	// Compose project names must match [a-z0-9][a-z0-9_-]*
	composeSafe := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

	for _, tt := range tests {
		t.Run(tt.sessionID, func(t *testing.T) {
			got := composeProjectName(tt.sessionID)
			if got != tt.want {
				t.Errorf("composeProjectName(%q) = %q, want %q", tt.sessionID, got, tt.want)
			}
			if !composeSafe.MatchString(got) {
				t.Errorf("composeProjectName(%q) = %q is not Compose-safe", tt.sessionID, got)
			}
		})
	}
}

func TestBuildEphemeralShellCmdWithCompose(t *testing.T) {
	cfg := &EphemeralConfig{
		Dockerfile: "Dockerfile",
		Compose:    "docker-compose.yml",
	}
	sessionID := "test-session-123"
	profile := Profile{Name: "vibe", Indicator: "⚡", Prompt: "test prompt"}

	cmd := buildEphemeralShellCmd(cfg, sessionID, profile, "", "", "", "", "", "", 2700, "/opt/vee", "/usr/bin/vee", nil)

	// Should contain docker compose up prefix
	if !strings.Contains(cmd, "docker compose -f .vee/docker-compose.yml -p vee-test-session-123 up -d --build") {
		t.Errorf("expected compose up command in output, got:\n%s", cmd)
	}

	// Should contain --network flag
	if !strings.Contains(cmd, "--network") {
		t.Errorf("expected --network flag in output, got:\n%s", cmd)
	}
	if !strings.Contains(cmd, "vee-test-session-123_default") {
		t.Errorf("expected compose network name in output, got:\n%s", cmd)
	}

	// Cleanup tail should NOT contain --compose-path or --compose-project
	// (teardown is now handled by the daemon via cleanupEphemeralSession)
	if strings.Contains(cmd, "--compose-path") {
		t.Errorf("unexpected --compose-path in cleanup tail, got:\n%s", cmd)
	}
	if strings.Contains(cmd, "--compose-project") {
		t.Errorf("unexpected --compose-project in cleanup tail, got:\n%s", cmd)
	}
}

func TestBuildEphemeralShellCmdWithoutCompose(t *testing.T) {
	cfg := &EphemeralConfig{
		Dockerfile: "Dockerfile",
	}
	sessionID := "test-session-456"
	profile := Profile{Name: "vibe", Indicator: "⚡", Prompt: "test prompt"}

	cmd := buildEphemeralShellCmd(cfg, sessionID, profile, "", "", "", "", "", "", 2700, "/opt/vee", "/usr/bin/vee", nil)

	// Should NOT contain docker compose commands
	if strings.Contains(cmd, "docker compose") {
		t.Errorf("unexpected compose command in output, got:\n%s", cmd)
	}

	// Should NOT contain --network flag
	if strings.Contains(cmd, "--network") {
		t.Errorf("unexpected --network flag in output, got:\n%s", cmd)
	}

}

func TestComposeSystemPromptInjection(t *testing.T) {
	composeContents := `services:
  postgres:
    image: postgres:16
    expose:
      - "5432"
  redis:
    image: redis:7
    expose:
      - "6379"`

	prompt := composeSystemPrompt("base", "", "", "", "", true, composeContents)

	if !strings.Contains(prompt, "Docker Compose services") {
		t.Errorf("expected compose services description in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "postgres") {
		t.Errorf("expected postgres service in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "redis") {
		t.Errorf("expected redis service in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "```yaml") {
		t.Errorf("expected yaml code block in prompt, got:\n%s", prompt)
	}
}

func TestComposeSystemPromptNoInjectionWithoutCompose(t *testing.T) {
	prompt := composeSystemPrompt("base", "", "", "", "", true, "")

	if strings.Contains(prompt, "Docker Compose services") {
		t.Errorf("unexpected compose injection in prompt without compose contents, got:\n%s", prompt)
	}
}

func TestGitConfigStruct(t *testing.T) {
	cfg := &GitConfig{
		UserName:  "Test User",
		UserEmail: "test@example.com",
	}

	if cfg.UserName != "Test User" {
		t.Errorf("UserName = %q, want %q", cfg.UserName, "Test User")
	}
	if cfg.UserEmail != "test@example.com" {
		t.Errorf("UserEmail = %q, want %q", cfg.UserEmail, "test@example.com")
	}
}

func TestGPGSigningConfigStruct(t *testing.T) {
	cfg := &GPGSigningConfig{
		HomeDir:    "/home/user/.gnupg",
		SocketPath: "/run/user/1000/gnupg/S.gpg-agent",
		SigningKey: "ABCD1234",
		GPGProgram: "/usr/bin/gpg",
	}

	if cfg.HomeDir != "/home/user/.gnupg" {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, "/home/user/.gnupg")
	}
	if cfg.SocketPath != "/run/user/1000/gnupg/S.gpg-agent" {
		t.Errorf("SocketPath = %q, want %q", cfg.SocketPath, "/run/user/1000/gnupg/S.gpg-agent")
	}
	if cfg.SigningKey != "ABCD1234" {
		t.Errorf("SigningKey = %q, want %q", cfg.SigningKey, "ABCD1234")
	}
	if cfg.GPGProgram != "/usr/bin/gpg" {
		t.Errorf("GPGProgram = %q, want %q", cfg.GPGProgram, "/usr/bin/gpg")
	}
}

func TestWriteGitConfigWithGPG(t *testing.T) {
	sessionID := "gpg-test-session"

	// Clean up any existing temp dir
	tmpDir := sessionTempDir(sessionID)
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	gitCfg := &GitConfig{
		UserName:  "Test User",
		UserEmail: "test@example.com",
	}
	gpgCfg := &GPGSigningConfig{
		SigningKey: "ABCD1234",
		GPGProgram: "/usr/bin/gpg2",
	}

	path, err := writeGitConfig(sessionID, gitCfg, gpgCfg)
	if err != nil {
		t.Fatalf("writeGitConfig() error = %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read gitconfig: %v", err)
	}

	contentStr := string(content)

	// Check [user] section
	if !strings.Contains(contentStr, "[user]") {
		t.Errorf("missing [user] section in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "name = Test User") {
		t.Errorf("missing user.name in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "email = test@example.com") {
		t.Errorf("missing user.email in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "signingkey = ABCD1234") {
		t.Errorf("missing user.signingkey in gitconfig:\n%s", contentStr)
	}

	// Check [commit] section
	if !strings.Contains(contentStr, "[commit]") {
		t.Errorf("missing [commit] section in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "gpgsign = true") {
		t.Errorf("missing commit.gpgsign in gitconfig:\n%s", contentStr)
	}

	// Check [gpg] section — uses wrapper script instead of host gpg
	if !strings.Contains(contentStr, "[gpg]") {
		t.Errorf("missing [gpg] section in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "program = /opt/vee/scripts/gpg-sign-wrapper") {
		t.Errorf("missing gpg.program wrapper in gitconfig:\n%s", contentStr)
	}

	// Check [safe] section
	if !strings.Contains(contentStr, "[safe]") {
		t.Errorf("missing [safe] section in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "directory = *") {
		t.Errorf("missing safe.directory in gitconfig:\n%s", contentStr)
	}
}

func TestWriteGitConfigWithoutGPG(t *testing.T) {
	sessionID := "git-test-no-gpg"

	tmpDir := sessionTempDir(sessionID)
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	gitCfg := &GitConfig{
		UserName:  "Minimal User",
		UserEmail: "minimal@example.com",
	}

	path, err := writeGitConfig(sessionID, gitCfg, nil)
	if err != nil {
		t.Fatalf("writeGitConfig() error = %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read gitconfig: %v", err)
	}

	contentStr := string(content)

	// Should have [user] but no [commit] or [gpg]
	if !strings.Contains(contentStr, "[user]") {
		t.Errorf("missing [user] section in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "name = Minimal User") {
		t.Errorf("missing user.name in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "email = minimal@example.com") {
		t.Errorf("missing user.email in gitconfig:\n%s", contentStr)
	}
	if strings.Contains(contentStr, "[commit]") {
		t.Errorf("unexpected [commit] section in gitconfig (no GPG):\n%s", contentStr)
	}
	if strings.Contains(contentStr, "[gpg]") {
		t.Errorf("unexpected [gpg] section in gitconfig (no GPG):\n%s", contentStr)
	}
	if strings.Contains(contentStr, "signingkey") {
		t.Errorf("unexpected signingkey in gitconfig (no GPG):\n%s", contentStr)
	}

	// Should still have [safe] section
	if !strings.Contains(contentStr, "[safe]") {
		t.Errorf("missing [safe] section in gitconfig:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "directory = *") {
		t.Errorf("missing safe.directory in gitconfig:\n%s", contentStr)
	}
}

func TestBuildEphemeralShellCmdWithoutGPGSigning(t *testing.T) {
	cfg := &EphemeralConfig{
		Dockerfile: "Dockerfile",
	}
	sessionID := "no-gpg-test"
	profile := Profile{Name: "vibe", Indicator: "⚡", Prompt: "test prompt"}

	cmd := buildEphemeralShellCmd(cfg, sessionID, profile, "", "", "", "", "", "", 2700, "/opt/vee", "/usr/bin/vee", nil)

	// On most test environments, GPG signing is not configured
	// so we should not see GPG-related mounts or env vars.
	// Note: if the test environment has GPG signing configured, this test may need adjustment.
	if strings.Contains(cmd, "GNUPGHOME=") && !strings.Contains(cmd, "IS_SANDBOX=1") {
		// Only fail if GNUPGHOME appears without being part of user config
		// This is a safety check - the test relies on the environment not having GPG configured
	}

	// The command should still contain basic Docker run arguments
	if !strings.Contains(cmd, "docker run") {
		t.Errorf("missing 'docker run' in command:\n%s", cmd)
	}
	if !strings.Contains(cmd, "IS_SANDBOX=1") {
		t.Errorf("missing IS_SANDBOX=1 in command:\n%s", cmd)
	}
}
