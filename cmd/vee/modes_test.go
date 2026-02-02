package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseModeFile(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		wantMode Mode
		wantErr  bool
	}{
		{
			name:     "full mode with priority",
			filename: "normal.md",
			content: `---
indicator: "🦊"
description: "Read-only exploration"
priority: 10
---
You are in read-only mode.`,
			wantMode: Mode{
				Name:        "normal",
				Indicator:   "🦊",
				Description: "Read-only exploration",
				Priority:    10,
				Prompt:      "You are in read-only mode.",
			},
		},
		{
			name:     "no_prompt mode",
			filename: "claude.md",
			content: `---
indicator: "🤖"
description: "Vanilla session"
priority: 0
no_prompt: true
---`,
			wantMode: Mode{
				Name:        "claude",
				Indicator:   "🤖",
				Description: "Vanilla session",
				Priority:    0,
				NoPrompt:    true,
				Prompt:      "",
			},
		},
		{
			name:     "no priority defaults to MaxInt",
			filename: "custom.md",
			content: `---
indicator: "🔧"
description: "Custom mode"
---
Custom body.`,
			wantMode: Mode{
				Name:        "custom",
				Indicator:   "🔧",
				Description: "Custom mode",
				Priority:    math.MaxInt,
				Prompt:      "Custom body.",
			},
		},
		{
			name:     "derives name from path with directory",
			filename: "/some/path/mymode.md",
			content: `---
indicator: "✨"
description: "test"
priority: 5
---
Body text.`,
			wantMode: Mode{
				Name:        "mymode",
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
			mode, err := parseModeFile(tt.filename, []byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if mode.Name != tt.wantMode.Name {
				t.Errorf("Name = %q, want %q", mode.Name, tt.wantMode.Name)
			}
			if mode.Indicator != tt.wantMode.Indicator {
				t.Errorf("Indicator = %q, want %q", mode.Indicator, tt.wantMode.Indicator)
			}
			if mode.Description != tt.wantMode.Description {
				t.Errorf("Description = %q, want %q", mode.Description, tt.wantMode.Description)
			}
			if mode.Priority != tt.wantMode.Priority {
				t.Errorf("Priority = %d, want %d", mode.Priority, tt.wantMode.Priority)
			}
			if mode.NoPrompt != tt.wantMode.NoPrompt {
				t.Errorf("NoPrompt = %v, want %v", mode.NoPrompt, tt.wantMode.NoPrompt)
			}
			if mode.Prompt != tt.wantMode.Prompt {
				t.Errorf("Prompt = %q, want %q", mode.Prompt, tt.wantMode.Prompt)
			}
		})
	}
}

func TestWrapModeBody(t *testing.T) {
	got := wrapModeBody("normal", "🦊", "Read-only mode.")
	want := `<mode name="normal"><indicator value="🦊" />

Read-only mode.
</mode>`
	if got != want {
		t.Errorf("wrapModeBody:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestLoadModesFromDir(t *testing.T) {
	dir := t.TempDir()

	// Write two mode files.
	os.WriteFile(filepath.Join(dir, "alpha.md"), []byte(`---
indicator: "A"
description: "Alpha mode"
priority: 2
---
Alpha body.`), 0644)

	os.WriteFile(filepath.Join(dir, "beta.md"), []byte(`---
indicator: "B"
description: "Beta mode"
priority: 1
---
Beta body.`), 0644)

	// Write a non-md file that should be ignored.
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not a mode"), 0644)

	modes, err := loadModesFromDir(dir)
	if err != nil {
		t.Fatalf("loadModesFromDir: %v", err)
	}

	if len(modes) != 2 {
		t.Fatalf("expected 2 modes, got %d", len(modes))
	}

	found := make(map[string]Mode)
	for _, m := range modes {
		found[m.Name] = m
	}

	if _, ok := found["alpha"]; !ok {
		t.Error("missing mode: alpha")
	}
	if _, ok := found["beta"]; !ok {
		t.Error("missing mode: beta")
	}
	if found["alpha"].Priority != 2 {
		t.Errorf("alpha priority = %d, want 2", found["alpha"].Priority)
	}
	if found["beta"].Priority != 1 {
		t.Errorf("beta priority = %d, want 1", found["beta"].Priority)
	}
}

// writeTestModes populates a veePath/modes/ directory with test mode files
// and returns the veePath.
func writeTestModes(t *testing.T) string {
	t.Helper()
	veePath := t.TempDir()
	modesDir := filepath.Join(veePath, "modes")
	os.MkdirAll(modesDir, 0755)

	os.WriteFile(filepath.Join(modesDir, "claude.md"), []byte(`---
indicator: "🤖"
description: "Vanilla Claude Code session"
priority: 0
no_prompt: true
---`), 0644)

	os.WriteFile(filepath.Join(modesDir, "normal.md"), []byte(`---
indicator: "🦊"
description: "Read-only exploration (default)"
priority: 10
---
Read-only exploration mode. You answer questions about the codebase.

You do not write files.`), 0644)

	os.WriteFile(filepath.Join(modesDir, "vibe.md"), []byte(`---
indicator: "⚡"
description: "Perform tasks with side-effects"
priority: 20
---
Task execution mode.`), 0644)

	os.WriteFile(filepath.Join(modesDir, "contradictor.md"), []byte(`---
indicator: "😈"
description: "Devil's advocate mode"
priority: 30
---
Devil's advocate mode. ALWAYS challenge the user's position.`), 0644)

	return veePath
}

func TestInitModeRegistryFromInstalledModes(t *testing.T) {
	veePath := writeTestModes(t)

	origRegistry := modeRegistry
	origOrder := modeOrder
	defer func() {
		modeRegistry = origRegistry
		modeOrder = origOrder
	}()

	if err := initModeRegistry(veePath); err != nil {
		t.Fatalf("initModeRegistry: %v", err)
	}

	// Verify registry was populated.
	if len(modeRegistry) != 4 {
		t.Fatalf("expected 4 modes in registry, got %d", len(modeRegistry))
	}

	// Verify order is sorted by priority.
	if len(modeOrder) != 4 {
		t.Fatalf("expected 4 entries in modeOrder, got %d", len(modeOrder))
	}

	// Claude (priority 0) should be first.
	if modeOrder[0] != "claude" {
		t.Errorf("modeOrder[0] = %q, want %q", modeOrder[0], "claude")
	}

	// Expected order: claude(0), normal(10), vibe(20), contradictor(30).
	expectedOrder := []string{"claude", "normal", "vibe", "contradictor"}
	for i, want := range expectedOrder {
		if modeOrder[i] != want {
			t.Errorf("modeOrder[%d] = %q, want %q", i, modeOrder[i], want)
		}
	}

	// Verify prompt composition for a regular mode.
	normal := modeRegistry["normal"]
	if normal.NoPrompt {
		t.Error("normal should not be NoPrompt")
	}
	if !strings.Contains(normal.Prompt, "<identity>") {
		t.Error("normal prompt should contain base prompt (<identity> tag)")
	}
	if !strings.Contains(normal.Prompt, `<mode name="normal">`) {
		t.Error("normal prompt should contain wrapped mode body")
	}
	if !strings.Contains(normal.Prompt, `<indicator value="🦊"`) {
		t.Error("normal prompt should contain indicator in XML wrapper")
	}
	if !strings.Contains(normal.Prompt, "Read-only exploration mode") {
		t.Error("normal prompt should contain mode body text")
	}

	// Verify no_prompt mode gets only KB section.
	claude := modeRegistry["claude"]
	if !claude.NoPrompt {
		t.Error("claude should be NoPrompt")
	}
	if !strings.Contains(claude.Prompt, "<knowledge-base>") {
		t.Error("claude prompt should contain KB section")
	}
	if strings.Contains(claude.Prompt, "<identity>") {
		t.Error("claude prompt should NOT contain identity/base prompt")
	}
}

func TestInitModeRegistryMergesUserOverrides(t *testing.T) {
	veePath := writeTestModes(t)

	// Create a temporary HOME with a user modes override.
	fakeHome := t.TempDir()
	userModesDir := filepath.Join(fakeHome, ".config", "vee", "modes")
	os.MkdirAll(userModesDir, 0755)

	// Override normal mode with different indicator and body.
	os.WriteFile(filepath.Join(userModesDir, "normal.md"), []byte(`---
indicator: "🐱"
description: "Custom read-only"
priority: 5
---
Custom normal body.`), 0644)

	// Add a brand new user mode.
	os.WriteFile(filepath.Join(userModesDir, "custom.md"), []byte(`---
indicator: "🌟"
description: "Custom user mode"
priority: 15
---
Custom mode body.`), 0644)

	// Override HOME so initModeRegistry picks up user dir.
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	origRegistry := modeRegistry
	origOrder := modeOrder
	defer func() {
		modeRegistry = origRegistry
		modeOrder = origOrder
	}()

	if err := initModeRegistry(veePath); err != nil {
		t.Fatalf("initModeRegistry: %v", err)
	}

	// Should have 5 modes: claude, normal (overridden), vibe, contradictor, custom (new).
	if len(modeRegistry) != 5 {
		t.Fatalf("expected 5 modes, got %d", len(modeRegistry))
	}

	// Normal should be overridden.
	normal := modeRegistry["normal"]
	if normal.Indicator != "🐱" {
		t.Errorf("normal indicator = %q, want %q (user override)", normal.Indicator, "🐱")
	}
	if normal.Priority != 5 {
		t.Errorf("normal priority = %d, want 5 (user override)", normal.Priority)
	}
	if !strings.Contains(normal.Prompt, "Custom normal body") {
		t.Error("normal prompt should contain user override body")
	}

	// Custom mode should exist.
	custom, ok := modeRegistry["custom"]
	if !ok {
		t.Fatal("missing user-added mode: custom")
	}
	if custom.Indicator != "🌟" {
		t.Errorf("custom indicator = %q, want %q", custom.Indicator, "🌟")
	}

	// Installed modes that weren't overridden should still be present.
	if _, ok := modeRegistry["claude"]; !ok {
		t.Error("missing installed mode: claude")
	}
	if _, ok := modeRegistry["vibe"]; !ok {
		t.Error("missing installed mode: vibe")
	}
	if _, ok := modeRegistry["contradictor"]; !ok {
		t.Error("missing installed mode: contradictor")
	}

	// Verify priority ordering: claude(0), normal(5), normal(10 would be wrong),
	// custom(15), vibe(20), contradictor(30).
	expectedOrder := []string{"claude", "normal", "custom", "vibe", "contradictor"}
	if len(modeOrder) != len(expectedOrder) {
		t.Fatalf("modeOrder length = %d, want %d", len(modeOrder), len(expectedOrder))
	}
	for i, want := range expectedOrder {
		if modeOrder[i] != want {
			t.Errorf("modeOrder[%d] = %q, want %q", i, modeOrder[i], want)
		}
	}
}
