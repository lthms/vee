package main

import (
	"regexp"
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
