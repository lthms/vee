package main

import (
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
	mode := Mode{Name: "vibe", Indicator: "⚡", Prompt: "test prompt"}

	cmd := buildEphemeralShellCmd(cfg, sessionID, mode, "", "", "", "", "", "", 2700, "/opt/vee", "/usr/bin/vee", nil)

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

	// Should contain --compose-path and --compose-project in cleanup
	if !strings.Contains(cmd, "--compose-path") {
		t.Errorf("expected --compose-path in cleanup tail, got:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--compose-project") {
		t.Errorf("expected --compose-project in cleanup tail, got:\n%s", cmd)
	}
}

func TestBuildEphemeralShellCmdWithoutCompose(t *testing.T) {
	cfg := &EphemeralConfig{
		Dockerfile: "Dockerfile",
	}
	sessionID := "test-session-456"
	mode := Mode{Name: "vibe", Indicator: "⚡", Prompt: "test prompt"}

	cmd := buildEphemeralShellCmd(cfg, sessionID, mode, "", "", "", "", "", "", 2700, "/opt/vee", "/usr/bin/vee", nil)

	// Should NOT contain docker compose commands
	if strings.Contains(cmd, "docker compose") {
		t.Errorf("unexpected compose command in output, got:\n%s", cmd)
	}

	// Should NOT contain --network flag
	if strings.Contains(cmd, "--network") {
		t.Errorf("unexpected --network flag in output, got:\n%s", cmd)
	}

	// Should NOT contain --compose-path or --compose-project
	if strings.Contains(cmd, "--compose-path") {
		t.Errorf("unexpected --compose-path in output, got:\n%s", cmd)
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
