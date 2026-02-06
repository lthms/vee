package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProfileFile(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		content     string
		wantProfile Profile
		wantErr     bool
	}{
		{
			name:     "full profile with priority",
			filename: "normal.md",
			content: `---
indicator: "🦊"
description: "Read-only exploration"
priority: 10
---
You are in read-only mode.`,
			wantProfile: Profile{
				Name:        "normal",
				Indicator:   "🦊",
				Description: "Read-only exploration",
				Priority:    10,
				Prompt:      "You are in read-only mode.",
			},
		},
		{
			name:     "profile with empty body",
			filename: "claude.md",
			content: `---
indicator: "🤖"
description: "Vanilla session"
priority: 0
---`,
			wantProfile: Profile{
				Name:        "claude",
				Indicator:   "🤖",
				Description: "Vanilla session",
				Priority:    0,
				Prompt:      "",
			},
		},
		{
			name:     "no priority defaults to MaxInt",
			filename: "custom.md",
			content: `---
indicator: "🔧"
description: "Custom profile"
---
Custom body.`,
			wantProfile: Profile{
				Name:        "custom",
				Indicator:   "🔧",
				Description: "Custom profile",
				Priority:    math.MaxInt,
				Prompt:      "Custom body.",
			},
		},
		{
			name:     "derives name from path with directory",
			filename: "/some/path/myprofile.md",
			content: `---
indicator: "✨"
description: "test"
priority: 5
---
Body text.`,
			wantProfile: Profile{
				Name:        "myprofile",
				Indicator:   "✨",
				Description: "test",
				Priority:    5,
				Prompt:      "Body text.",
			},
		},
		{
			name:     "missing frontmatter",
			filename: "bad.md",
			content:  "No frontmatter here.",
			wantErr:  true,
		},
		{
			name:     "unclosed frontmatter",
			filename: "bad.md",
			content:  "---\nindicator: x\n",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := parseProfileFile(tt.filename, []byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if profile.Name != tt.wantProfile.Name {
				t.Errorf("Name = %q, want %q", profile.Name, tt.wantProfile.Name)
			}
			if profile.Indicator != tt.wantProfile.Indicator {
				t.Errorf("Indicator = %q, want %q", profile.Indicator, tt.wantProfile.Indicator)
			}
			if profile.Description != tt.wantProfile.Description {
				t.Errorf("Description = %q, want %q", profile.Description, tt.wantProfile.Description)
			}
			if profile.Priority != tt.wantProfile.Priority {
				t.Errorf("Priority = %d, want %d", profile.Priority, tt.wantProfile.Priority)
			}
			if profile.Prompt != tt.wantProfile.Prompt {
				t.Errorf("Prompt = %q, want %q", profile.Prompt, tt.wantProfile.Prompt)
			}
		})
	}
}

func TestWrapProfileBody(t *testing.T) {
	got := wrapProfileBody("🦊", "Read-only mode.")
	if !strings.Contains(got, `<rule object="Script">`) {
		t.Error("wrapProfileBody should include the script rule")
	}
	if !strings.Contains(got, "<script>\nRead-only mode.\n</script>") {
		t.Error("wrapProfileBody should wrap body in script tags")
	}
	if !strings.Contains(got, `<rule object="Indicator">`) {
		t.Error("wrapProfileBody should include the indicator rule")
	}
	if !strings.Contains(got, "🦊") {
		t.Error("wrapProfileBody should include the indicator value")
	}
}

func TestLoadProfilesFromDir(t *testing.T) {
	dir := t.TempDir()

	// Write two profile files.
	os.WriteFile(filepath.Join(dir, "alpha.md"), []byte(`---
indicator: "A"
description: "Alpha profile"
priority: 2
---
Alpha body.`), 0644)

	os.WriteFile(filepath.Join(dir, "beta.md"), []byte(`---
indicator: "B"
description: "Beta profile"
priority: 1
---
Beta body.`), 0644)

	// Write a non-md file that should be ignored.
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not a profile"), 0644)

	profiles, err := loadProfilesFromDir(dir)
	if err != nil {
		t.Fatalf("loadProfilesFromDir: %v", err)
	}

	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}

	found := make(map[string]Profile)
	for _, m := range profiles {
		found[m.Name] = m
	}

	if _, ok := found["alpha"]; !ok {
		t.Error("missing profile: alpha")
	}
	if _, ok := found["beta"]; !ok {
		t.Error("missing profile: beta")
	}
	if found["alpha"].Priority != 2 {
		t.Errorf("alpha priority = %d, want 2", found["alpha"].Priority)
	}
	if found["beta"].Priority != 1 {
		t.Errorf("beta priority = %d, want 1", found["beta"].Priority)
	}
}

// writeTestProfiles populates a veePath/profiles/ directory with test profile files
// and returns the veePath.
func writeTestProfiles(t *testing.T) string {
	t.Helper()
	veePath := t.TempDir()
	profilesDir := filepath.Join(veePath, "profiles")
	os.MkdirAll(profilesDir, 0755)

	os.WriteFile(filepath.Join(profilesDir, "claude.md"), []byte(`---
indicator: "🤖"
description: "Vanilla Claude Code session"
priority: 0
---`), 0644)

	os.WriteFile(filepath.Join(profilesDir, "normal.md"), []byte(`---
indicator: "🦊"
description: "Read-only exploration (default)"
priority: 10
---
Read-only exploration mode. You answer questions about the codebase.

You do not write files.`), 0644)

	os.WriteFile(filepath.Join(profilesDir, "vibe.md"), []byte(`---
indicator: "⚡"
description: "Perform tasks with side-effects"
priority: 20
---
Task execution mode.`), 0644)

	os.WriteFile(filepath.Join(profilesDir, "contradictor.md"), []byte(`---
indicator: "😈"
description: "Devil's advocate mode"
priority: 30
---
Devil's advocate mode. ALWAYS challenge the user's position.`), 0644)

	return veePath
}

func TestInitProfileRegistryFromInstalledProfiles(t *testing.T) {
	veePath := writeTestProfiles(t)

	origRegistry := profileRegistry
	origOrder := profileOrder
	defer func() {
		profileRegistry = origRegistry
		profileOrder = origOrder
	}()

	if err := initProfileRegistry(veePath); err != nil {
		t.Fatalf("initProfileRegistry: %v", err)
	}

	// Verify registry was populated.
	if len(profileRegistry) != 4 {
		t.Fatalf("expected 4 profiles in registry, got %d", len(profileRegistry))
	}

	// Verify order is sorted by priority.
	if len(profileOrder) != 4 {
		t.Fatalf("expected 4 entries in profileOrder, got %d", len(profileOrder))
	}

	// Claude (priority 0) should be first.
	if profileOrder[0] != "claude" {
		t.Errorf("profileOrder[0] = %q, want %q", profileOrder[0], "claude")
	}

	// Expected order: claude(0), normal(10), vibe(20), contradictor(30).
	expectedOrder := []string{"claude", "normal", "vibe", "contradictor"}
	for i, want := range expectedOrder {
		if profileOrder[i] != want {
			t.Errorf("profileOrder[%d] = %q, want %q", i, profileOrder[i], want)
		}
	}

	// Verify prompt composition for a regular profile.
	normal := profileRegistry["normal"]
	if !strings.Contains(normal.Prompt, "Knowledge base") {
		t.Error("normal prompt should contain base prompt (Knowledge base rule)")
	}
	if !strings.Contains(normal.Prompt, `<script>`) {
		t.Error("normal prompt should contain wrapped profile body")
	}
	if !strings.Contains(normal.Prompt, "Read-only exploration mode") {
		t.Error("normal prompt should contain profile body text")
	}

	// Verify claude profile (empty body) gets base prompt without script wrapper.
	claude := profileRegistry["claude"]
	if !strings.Contains(claude.Prompt, "Knowledge base") {
		t.Error("claude prompt should contain base prompt (Knowledge base rule)")
	}
	if strings.Contains(claude.Prompt, "<script>") {
		t.Error("claude prompt should not contain script wrapper (empty body)")
	}
}

func TestInitProfileRegistryMergesUserOverrides(t *testing.T) {
	veePath := writeTestProfiles(t)

	// Create a temporary HOME with a user profiles override.
	fakeHome := t.TempDir()
	userProfilesDir := filepath.Join(fakeHome, ".config", "vee", "profiles")
	os.MkdirAll(userProfilesDir, 0755)

	// Override normal profile with different indicator and body.
	os.WriteFile(filepath.Join(userProfilesDir, "normal.md"), []byte(`---
indicator: "🐱"
description: "Custom read-only"
priority: 5
---
Custom normal body.`), 0644)

	// Add a brand new user profile.
	os.WriteFile(filepath.Join(userProfilesDir, "custom.md"), []byte(`---
indicator: "🌟"
description: "Custom user profile"
priority: 15
---
Custom profile body.`), 0644)

	// Override HOME so initProfileRegistry picks up user dir.
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	origRegistry := profileRegistry
	origOrder := profileOrder
	defer func() {
		profileRegistry = origRegistry
		profileOrder = origOrder
	}()

	if err := initProfileRegistry(veePath); err != nil {
		t.Fatalf("initProfileRegistry: %v", err)
	}

	// Should have 5 profiles: claude, normal (overridden), vibe, contradictor, custom (new).
	if len(profileRegistry) != 5 {
		t.Fatalf("expected 5 profiles, got %d", len(profileRegistry))
	}

	// Normal should be overridden.
	normal := profileRegistry["normal"]
	if normal.Indicator != "🐱" {
		t.Errorf("normal indicator = %q, want %q (user override)", normal.Indicator, "🐱")
	}
	if normal.Priority != 5 {
		t.Errorf("normal priority = %d, want 5 (user override)", normal.Priority)
	}
	if !strings.Contains(normal.Prompt, "Custom normal body") {
		t.Error("normal prompt should contain user override body")
	}

	// Custom profile should exist.
	custom, ok := profileRegistry["custom"]
	if !ok {
		t.Fatal("missing user-added profile: custom")
	}
	if custom.Indicator != "🌟" {
		t.Errorf("custom indicator = %q, want %q", custom.Indicator, "🌟")
	}

	// Installed profiles that weren't overridden should still be present.
	if _, ok := profileRegistry["claude"]; !ok {
		t.Error("missing installed profile: claude")
	}
	if _, ok := profileRegistry["vibe"]; !ok {
		t.Error("missing installed profile: vibe")
	}
	if _, ok := profileRegistry["contradictor"]; !ok {
		t.Error("missing installed profile: contradictor")
	}

	// Verify priority ordering: claude(0), normal(5), custom(15), vibe(20), contradictor(30).
	expectedOrder := []string{"claude", "normal", "custom", "vibe", "contradictor"}
	if len(profileOrder) != len(expectedOrder) {
		t.Fatalf("profileOrder length = %d, want %d", len(profileOrder), len(expectedOrder))
	}
	for i, want := range expectedOrder {
		if profileOrder[i] != want {
			t.Errorf("profileOrder[%d] = %q, want %q", i, profileOrder[i], want)
		}
	}
}
