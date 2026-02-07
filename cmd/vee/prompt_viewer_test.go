package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestParseHeading(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedText  string
		expectedLevel int
	}{
		{
			name:          "h1",
			input:         "# Heading One",
			expectedText:  "Heading One",
			expectedLevel: 1,
		},
		{
			name:          "h2",
			input:         "## Heading Two",
			expectedText:  "Heading Two",
			expectedLevel: 2,
		},
		{
			name:          "h3",
			input:         "### Heading Three",
			expectedText:  "Heading Three",
			expectedLevel: 3,
		},
		{
			name:          "h4",
			input:         "#### Heading Four",
			expectedText:  "Heading Four",
			expectedLevel: 4,
		},
		{
			name:          "h5 not a heading",
			input:         "##### Five hashes",
			expectedText:  "",
			expectedLevel: 0,
		},
		{
			name:          "not a heading - no space",
			input:         "#NoSpace",
			expectedText:  "",
			expectedLevel: 0,
		},
		{
			name:          "not a heading - plain text",
			input:         "Just some text",
			expectedText:  "",
			expectedLevel: 0,
		},
		{
			name:          "empty string",
			input:         "",
			expectedText:  "",
			expectedLevel: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, level := parseHeading(tt.input)
			if text != tt.expectedText {
				t.Errorf("parseHeading(%q) text = %q, want %q", tt.input, text, tt.expectedText)
			}
			if level != tt.expectedLevel {
				t.Errorf("parseHeading(%q) level = %d, want %d", tt.input, level, tt.expectedLevel)
			}
		})
	}
}

func TestParseBullet(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedIndent string
		expectedRest   string
		expectedOk     bool
	}{
		{
			name:           "dash bullet",
			input:          "- Item one",
			expectedIndent: "",
			expectedRest:   "Item one",
			expectedOk:     true,
		},
		{
			name:           "asterisk bullet",
			input:          "* Item two",
			expectedIndent: "",
			expectedRest:   "Item two",
			expectedOk:     true,
		},
		{
			name:           "indented bullet",
			input:          "  - Nested item",
			expectedIndent: "  ",
			expectedRest:   "Nested item",
			expectedOk:     true,
		},
		{
			name:           "deeply indented",
			input:          "    * Deep item",
			expectedIndent: "    ",
			expectedRest:   "Deep item",
			expectedOk:     true,
		},
		{
			name:           "not a bullet - no space after dash",
			input:          "-NoSpace",
			expectedIndent: "",
			expectedRest:   "",
			expectedOk:     false,
		},
		{
			name:           "not a bullet - plain text",
			input:          "Just text",
			expectedIndent: "",
			expectedRest:   "",
			expectedOk:     false,
		},
		{
			name:           "not a bullet - dash in middle",
			input:          "some - thing",
			expectedIndent: "",
			expectedRest:   "",
			expectedOk:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indent, rest, ok := parseBullet(tt.input)
			if indent != tt.expectedIndent {
				t.Errorf("parseBullet(%q) indent = %q, want %q", tt.input, indent, tt.expectedIndent)
			}
			if rest != tt.expectedRest {
				t.Errorf("parseBullet(%q) rest = %q, want %q", tt.input, rest, tt.expectedRest)
			}
			if ok != tt.expectedOk {
				t.Errorf("parseBullet(%q) ok = %v, want %v", tt.input, ok, tt.expectedOk)
			}
		})
	}
}

func TestRenderPromptLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		check    func(t *testing.T, lines []string)
	}{
		{
			name:     "heading is styled",
			input:    "# My Heading",
			maxWidth: 80,
			check: func(t *testing.T, lines []string) {
				if len(lines) != 1 {
					t.Fatalf("expected 1 line, got %d", len(lines))
				}
				if !strings.Contains(lines[0], "My Heading") {
					t.Error("heading text should be preserved")
				}
				if !strings.Contains(lines[0], ansiBold) {
					t.Error("heading should be bold")
				}
			},
		},
		{
			name:     "bullet list preserved",
			input:    "- First\n- Second",
			maxWidth: 80,
			check: func(t *testing.T, lines []string) {
				if len(lines) != 2 {
					t.Fatalf("expected 2 lines, got %d", len(lines))
				}
				if !strings.Contains(lines[0], "- First") {
					t.Errorf("first bullet wrong: %q", lines[0])
				}
				if !strings.Contains(lines[1], "- Second") {
					t.Errorf("second bullet wrong: %q", lines[1])
				}
			},
		},
		{
			name:     "fenced code block is dim",
			input:    "```\ncode here\n```",
			maxWidth: 80,
			check: func(t *testing.T, lines []string) {
				if len(lines) != 3 {
					t.Fatalf("expected 3 lines, got %d", len(lines))
				}
				for _, line := range lines {
					if !strings.Contains(line, ansiDim) {
						t.Errorf("fenced block line should be dim: %q", line)
					}
				}
			},
		},
		{
			name:     "xml tag styled",
			input:    "<rule object=\"test\">",
			maxWidth: 80,
			check: func(t *testing.T, lines []string) {
				if len(lines) != 1 {
					t.Fatalf("expected 1 line, got %d", len(lines))
				}
				if !strings.Contains(lines[0], "rule") {
					t.Error("tag name should be preserved")
				}
			},
		},
		{
			name:     "empty lines preserved",
			input:    "Line one\n\nLine two",
			maxWidth: 80,
			check: func(t *testing.T, lines []string) {
				if len(lines) != 3 {
					t.Fatalf("expected 3 lines, got %d", len(lines))
				}
				if lines[1] != "" {
					t.Errorf("empty line should be empty, got %q", lines[1])
				}
			},
		},
		{
			name:     "long line wrapped",
			input:    "This is a very long line that should be wrapped when it exceeds the maximum width",
			maxWidth: 30,
			check: func(t *testing.T, lines []string) {
				if len(lines) < 2 {
					t.Error("long line should be wrapped into multiple lines")
				}
			},
		},
		{
			name:     "minimum width handled",
			input:    "test",
			maxWidth: 0,
			check: func(t *testing.T, lines []string) {
				// Should not panic, width clamped to 1
				if len(lines) == 0 {
					t.Error("should produce at least one line")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := renderPromptLines(tt.input, tt.maxWidth)
			tt.check(t, lines)
		})
	}
}

func TestPromptViewerModelInit(t *testing.T) {
	result := promptResult{
		profile:      "normal",
		indicator:    "🦊",
		systemPrompt: "Test prompt content",
	}

	m := initialPromptViewerModel(result)

	if m.profile != "normal" {
		t.Errorf("profile = %q, want %q", m.profile, "normal")
	}
	if m.indicator != "🦊" {
		t.Errorf("indicator = %q, want %q", m.indicator, "🦊")
	}
	if m.rawContent != "Test prompt content" {
		t.Errorf("rawContent = %q, want %q", m.rawContent, "Test prompt content")
	}
	if m.ready {
		t.Error("model should not be ready initially")
	}
	if len(m.lines) != 0 {
		t.Error("lines should be empty initially")
	}
}

func TestPromptViewerModelWithError(t *testing.T) {
	result := promptResult{
		errorMsg: "Could not reach the daemon.",
	}

	m := initialPromptViewerModel(result)

	if m.errorMsg != "Could not reach the daemon." {
		t.Errorf("errorMsg = %q, want %q", m.errorMsg, "Could not reach the daemon.")
	}

	// Simulate window size message
	m.width = 80
	m.height = 24

	view := m.viewError()
	if !strings.Contains(view, "Could not reach the daemon.") {
		t.Error("error view should contain error message")
	}
}

func TestPromptViewerModelKeyHandling(t *testing.T) {
	result := promptResult{
		profile:      "normal",
		indicator:    "🦊",
		systemPrompt: "Test content",
	}

	m := initialPromptViewerModel(result)

	// First, simulate WindowSizeMsg to initialize viewport
	sizeMsg := tea.WindowSizeMsg{Width: 80, Height: 24}
	newModel, _ := m.Update(sizeMsg)
	m = newModel.(promptViewerModel)

	if !m.ready {
		t.Error("model should be ready after WindowSizeMsg")
	}
	if len(m.lines) == 0 {
		t.Error("lines should be populated after WindowSizeMsg")
	}

	tests := []struct {
		name     string
		key      tea.KeyMsg
		shouldQuit bool
	}{
		{
			name:     "q quits",
			key:      tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}},
			shouldQuit: true,
		},
		{
			name:     "Esc quits",
			key:      tea.KeyMsg{Type: tea.KeyEsc},
			shouldQuit: true,
		},
		{
			name:     "Ctrl+C quits",
			key:      tea.KeyMsg{Type: tea.KeyCtrlC},
			shouldQuit: true,
		},
		{
			name:     "j is swallowed",
			key:      tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}},
			shouldQuit: false,
		},
		{
			name:     "k is swallowed",
			key:      tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}},
			shouldQuit: false,
		},
		{
			name:     "d is swallowed",
			key:      tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}},
			shouldQuit: false,
		},
		{
			name:     "u is swallowed",
			key:      tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}},
			shouldQuit: false,
		},
		{
			name:     "Ctrl+D is swallowed",
			key:      tea.KeyMsg{Type: tea.KeyCtrlD},
			shouldQuit: false,
		},
		{
			name:     "Ctrl+U is swallowed",
			key:      tea.KeyMsg{Type: tea.KeyCtrlU},
			shouldQuit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testModel := m // Fresh copy for each test
			_, cmd := testModel.Update(tt.key)

			isQuit := cmd != nil && cmd() == tea.Quit()
			if isQuit != tt.shouldQuit {
				t.Errorf("key %v: shouldQuit = %v, got cmd that quits = %v", tt.key, tt.shouldQuit, isQuit)
			}
		})
	}
}

func TestPromptViewerModelGotoKeys(t *testing.T) {
	// Create model with enough content to scroll
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "Line content")
	}
	content := strings.Join(lines, "\n")

	result := promptResult{
		profile:      "normal",
		indicator:    "🦊",
		systemPrompt: content,
	}

	m := initialPromptViewerModel(result)

	// Initialize with WindowSizeMsg
	sizeMsg := tea.WindowSizeMsg{Width: 80, Height: 24}
	newModel, _ := m.Update(sizeMsg)
	m = newModel.(promptViewerModel)

	// Test 'G' goes to bottom
	gKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}
	newModel, _ = m.Update(gKey)
	m = newModel.(promptViewerModel)

	if m.viewport.AtTop() {
		t.Error("after 'G', viewport should not be at top")
	}

	// Test 'g' goes to top
	gLowerKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}
	newModel, _ = m.Update(gLowerKey)
	m = newModel.(promptViewerModel)

	if !m.viewport.AtTop() {
		t.Error("after 'g', viewport should be at top")
	}
}

func TestStripAnsiForMatch(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no ansi",
			input:    "plain text",
			expected: "plain text",
		},
		{
			name:     "simple color",
			input:    "\033[31mred\033[0m",
			expected: "red",
		},
		{
			name:     "bold and color",
			input:    "\033[1m\033[38;2;137;180;250mheading\033[0m",
			expected: "heading",
		},
		{
			name:     "mixed content",
			input:    "before \033[32mgreen\033[0m after",
			expected: "before green after",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stripAnsiForMatch(tt.input)
			if result != tt.expected {
				t.Errorf("stripAnsiForMatch(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestPreparePromptLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		check    func(t *testing.T, lines []promptLine)
	}{
		{
			name:     "heading detected",
			input:    "# My Heading",
			maxWidth: 80,
			check: func(t *testing.T, lines []promptLine) {
				if len(lines) != 1 {
					t.Fatalf("expected 1 line, got %d", len(lines))
				}
				if lines[0].kind != lineKindHeading {
					t.Errorf("expected heading kind, got %v", lines[0].kind)
				}
				if lines[0].text != "My Heading" {
					t.Errorf("text = %q, want %q", lines[0].text, "My Heading")
				}
			},
		},
		{
			name:     "bullet detected",
			input:    "- Item one",
			maxWidth: 80,
			check: func(t *testing.T, lines []promptLine) {
				if len(lines) != 1 {
					t.Fatalf("expected 1 line, got %d", len(lines))
				}
				if lines[0].kind != lineKindBullet {
					t.Errorf("expected bullet kind, got %v", lines[0].kind)
				}
			},
		},
		{
			name:     "fenced block",
			input:    "```\ncode\n```",
			maxWidth: 80,
			check: func(t *testing.T, lines []promptLine) {
				if len(lines) != 3 {
					t.Fatalf("expected 3 lines, got %d", len(lines))
				}
				for _, pl := range lines {
					if pl.kind != lineKindFenced {
						t.Errorf("expected fenced kind, got %v", pl.kind)
					}
				}
			},
		},
		{
			name:     "xml tag",
			input:    "<rule object=\"test\">",
			maxWidth: 80,
			check: func(t *testing.T, lines []promptLine) {
				if len(lines) != 1 {
					t.Fatalf("expected 1 line, got %d", len(lines))
				}
				if lines[0].kind != lineKindXMLTag {
					t.Errorf("expected xml tag kind, got %v", lines[0].kind)
				}
			},
		},
		{
			name:     "plain text",
			input:    "Just some text",
			maxWidth: 80,
			check: func(t *testing.T, lines []promptLine) {
				if len(lines) != 1 {
					t.Fatalf("expected 1 line, got %d", len(lines))
				}
				if lines[0].kind != lineKindPlain {
					t.Errorf("expected plain kind, got %v", lines[0].kind)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := preparePromptLines(tt.input, tt.maxWidth)
			tt.check(t, lines)
		})
	}
}

func TestPromptViewerSearch(t *testing.T) {
	content := "Line one\nLine two with error\nLine three\nAnother error here\nLine five"

	result := promptResult{
		profile:      "normal",
		indicator:    "🦊",
		systemPrompt: content,
	}

	m := initialPromptViewerModel(result)

	// Initialize with WindowSizeMsg
	sizeMsg := tea.WindowSizeMsg{Width: 80, Height: 24}
	newModel, _ := m.Update(sizeMsg)
	m = newModel.(promptViewerModel)

	// Enter search mode with '/'
	slashKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}
	newModel, _ = m.Update(slashKey)
	m = newModel.(promptViewerModel)

	if !m.searching {
		t.Error("should be in search mode after '/'")
	}

	// Type "error"
	for _, r := range "error" {
		key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		newModel, _ = m.Update(key)
		m = newModel.(promptViewerModel)
	}

	if m.filter != "error" {
		t.Errorf("filter = %q, want %q", m.filter, "error")
	}

	// Press Enter to confirm search
	enterKey := tea.KeyMsg{Type: tea.KeyEnter}
	newModel, _ = m.Update(enterKey)
	m = newModel.(promptViewerModel)

	if m.searching {
		t.Error("should exit search mode after Enter")
	}

	if len(m.matches) != 2 {
		t.Errorf("expected 2 matches, got %d", len(m.matches))
	}

	// Test 'n' for next match
	nKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}
	newModel, _ = m.Update(nKey)
	m = newModel.(promptViewerModel)

	if m.matchIdx != 1 {
		t.Errorf("matchIdx = %d, want 1", m.matchIdx)
	}

	// Test 'N' for previous match (wraps around)
	shiftNKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}}
	newModel, _ = m.Update(shiftNKey)
	m = newModel.(promptViewerModel)

	if m.matchIdx != 0 {
		t.Errorf("matchIdx = %d, want 0", m.matchIdx)
	}

	// Test Esc clears search
	escKey := tea.KeyMsg{Type: tea.KeyEsc}
	newModel, _ = m.Update(escKey)
	m = newModel.(promptViewerModel)

	if m.filter != "" {
		t.Errorf("filter should be cleared after Esc, got %q", m.filter)
	}
	if len(m.matches) != 0 {
		t.Errorf("matches should be cleared after Esc, got %d", len(m.matches))
	}
}

func TestPromptViewerSearchInputEditing(t *testing.T) {
	result := promptResult{
		profile:      "normal",
		indicator:    "🦊",
		systemPrompt: "test content",
	}

	m := initialPromptViewerModel(result)

	// Initialize
	sizeMsg := tea.WindowSizeMsg{Width: 80, Height: 24}
	newModel, _ := m.Update(sizeMsg)
	m = newModel.(promptViewerModel)

	// Enter search mode
	slashKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}
	newModel, _ = m.Update(slashKey)
	m = newModel.(promptViewerModel)

	// Type "hello world"
	for _, r := range "hello world" {
		key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		newModel, _ = m.Update(key)
		m = newModel.(promptViewerModel)
	}

	// Test backspace
	bsKey := tea.KeyMsg{Type: tea.KeyBackspace}
	newModel, _ = m.Update(bsKey)
	m = newModel.(promptViewerModel)

	if m.filter != "hello worl" {
		t.Errorf("filter after backspace = %q, want %q", m.filter, "hello worl")
	}

	// Test Ctrl+W (delete word)
	ctrlWKey := tea.KeyMsg{Type: tea.KeyCtrlW}
	newModel, _ = m.Update(ctrlWKey)
	m = newModel.(promptViewerModel)

	if m.filter != "hello " {
		t.Errorf("filter after Ctrl+W = %q, want %q", m.filter, "hello ")
	}

	// Test Ctrl+U (clear)
	ctrlUKey := tea.KeyMsg{Type: tea.KeyCtrlU}
	newModel, _ = m.Update(ctrlUKey)
	m = newModel.(promptViewerModel)

	if m.filter != "" {
		t.Errorf("filter after Ctrl+U = %q, want empty", m.filter)
	}

	// Test Esc cancels search
	escKey := tea.KeyMsg{Type: tea.KeyEsc}
	newModel, _ = m.Update(escKey)
	m = newModel.(promptViewerModel)

	if m.searching {
		t.Error("should exit search mode after Esc")
	}
}
