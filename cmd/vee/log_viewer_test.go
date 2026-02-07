package main

import (
	"strings"
	"testing"
)

func TestHighlightSlog(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, result string)
	}{
		{
			name:  "info level",
			input: `time=2026-02-07T10:00:00.000Z level=INFO msg="test message" key=value`,
			check: func(t *testing.T, result string) {
				// Should preserve original content
				if !strings.Contains(result, "test message") {
					t.Error("message content should be preserved")
				}
				if !strings.Contains(result, "key=") {
					t.Error("key-value pairs should be preserved")
				}
				if !strings.Contains(result, "INFO") {
					t.Error("level should be preserved")
				}
			},
		},
		{
			name:  "debug level",
			input: `time=2026-02-07T10:00:00.000Z level=DEBUG msg="debug info"`,
			check: func(t *testing.T, result string) {
				if !strings.Contains(result, "DEBUG") {
					t.Error("DEBUG level should be preserved")
				}
			},
		},
		{
			name:  "warn level",
			input: `time=2026-02-07T10:00:00.000Z level=WARN msg="warning"`,
			check: func(t *testing.T, result string) {
				if !strings.Contains(result, "WARN") {
					t.Error("WARN level should be preserved")
				}
			},
		},
		{
			name:  "error level",
			input: `time=2026-02-07T10:00:00.000Z level=ERROR msg="error occurred"`,
			check: func(t *testing.T, result string) {
				if !strings.Contains(result, "ERROR") {
					t.Error("ERROR level should be preserved")
				}
			},
		},
		{
			name:  "plain text (non-slog)",
			input: `This is just plain text`,
			check: func(t *testing.T, result string) {
				// Should return as-is without modification
				if result != "This is just plain text" {
					t.Errorf("plain text should be unchanged, got: %q", result)
				}
			},
		},
		{
			name:  "quoted values with spaces",
			input: `time=2026-02-07T10:00:00.000Z level=INFO msg="hello world" query="SELECT * FROM users"`,
			check: func(t *testing.T, result string) {
				if !strings.Contains(result, "hello world") {
					t.Error("quoted message should be preserved")
				}
				if !strings.Contains(result, "SELECT * FROM users") {
					t.Error("quoted value should be preserved")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := highlightSlog(tt.input, "")
			tt.check(t, result)
		})
	}
}

func TestHighlightFilter(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		filter string
		check  func(t *testing.T, result string)
	}{
		{
			name:   "simple match",
			line:   "hello world",
			filter: "world",
			check: func(t *testing.T, result string) {
				// Should preserve matched text
				if !strings.Contains(result, "world") {
					t.Error("matched text should be preserved")
				}
				if !strings.Contains(result, "hello") {
					t.Error("surrounding text should be preserved")
				}
			},
		},
		{
			name:   "case insensitive",
			line:   "Hello World",
			filter: "world",
			check: func(t *testing.T, result string) {
				if !strings.Contains(result, "World") {
					t.Error("original case should be preserved")
				}
			},
		},
		{
			name:   "no match",
			line:   "hello world",
			filter: "foo",
			check: func(t *testing.T, result string) {
				if result != "hello world" {
					t.Error("line without match should be unchanged")
				}
			},
		},
		{
			name:   "empty filter",
			line:   "hello world",
			filter: "",
			check: func(t *testing.T, result string) {
				if result != "hello world" {
					t.Error("empty filter should return unchanged line")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := highlightFilter(tt.line, tt.filter)
			tt.check(t, result)
		})
	}
}

func TestLogModelScroll(t *testing.T) {
	m := logModel{
		lines:  make([]string, 100),
		height: 20, // viewableHeight = 18
		width:  80,
	}

	// Test scrollToBottom
	m.scrollToBottom()
	expectedMax := 100 - 18 // 82
	if m.scroll != expectedMax {
		t.Errorf("scrollToBottom: expected %d, got %d", expectedMax, m.scroll)
	}

	// Test clampScroll (too high)
	m.scroll = 200
	m.clampScroll()
	if m.scroll != expectedMax {
		t.Errorf("clampScroll high: expected %d, got %d", expectedMax, m.scroll)
	}

	// Test clampScroll (negative)
	m.scroll = -10
	m.clampScroll()
	if m.scroll != 0 {
		t.Errorf("clampScroll low: expected 0, got %d", m.scroll)
	}
}

func TestLogModelMatches(t *testing.T) {
	m := logModel{
		lines: []string{
			"line one",
			"line two with error",
			"line three",
			"another error here",
			"line five",
		},
		height: 10,
		width:  80,
	}

	m.filter = "error"
	m.updateMatches()

	if len(m.matches) != 2 {
		t.Errorf("expected 2 matches, got %d", len(m.matches))
	}
	if m.matches[0] != 1 || m.matches[1] != 3 {
		t.Errorf("wrong match indices: %v", m.matches)
	}
}

func TestSliceAnsi(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		start  int
		width  int
		expect string
	}{
		{
			name:   "no offset plain",
			input:  "hello world",
			start:  0,
			width:  5,
			expect: "hello\033[0m",
		},
		{
			name:   "offset plain",
			input:  "hello world",
			start:  6,
			width:  5,
			expect: "world\033[0m",
		},
		{
			name:   "offset into styled text preserves style",
			input:  "\033[31mhello world\033[0m",
			start:  6,
			width:  5,
			expect: "\033[31mworld\033[0m",
		},
		{
			name:   "offset past reset picks up new style",
			input:  "\033[31mhello\033[0m \033[32mworld\033[0m",
			start:  6,
			width:  5,
			expect: "\033[32mworld\033[0m",
		},
		{
			name:   "offset past content",
			input:  "short",
			start:  10,
			width:  5,
			expect: "",
		},
		{
			name:   "multiple styles accumulated",
			input:  "\033[1m\033[31mbold red text\033[0m",
			start:  5,
			width:  3,
			expect: "\033[1m\033[31mred\033[0m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sliceAnsi(tt.input, tt.start, tt.width)
			if result != tt.expect {
				t.Errorf("sliceAnsi(%q, %d, %d) = %q, want %q",
					tt.input, tt.start, tt.width, result, tt.expect)
			}
		})
	}
}

func TestTruncateAnsi(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		width    int
		expected string
	}{
		{
			name:     "plain text no truncation",
			input:    "hello",
			width:    10,
			expected: "hello",
		},
		{
			name:     "plain text truncation",
			input:    "hello world",
			width:    5,
			expected: "hello\033[0m",
		},
		{
			name:     "ansi codes not counted",
			input:    "\033[31mred\033[0m",
			width:    10,
			expected: "\033[31mred\033[0m",
		},
		{
			name:     "ansi with truncation",
			input:    "\033[31mhello world\033[0m",
			width:    5,
			expected: "\033[31mhello\033[0m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateAnsi(tt.input, tt.width)
			if result != tt.expected {
				t.Errorf("truncateAnsi(%q, %d) = %q, want %q",
					tt.input, tt.width, result, tt.expected)
			}
		})
	}
}

func TestFormatLineRange(t *testing.T) {
	tests := []struct {
		start, end, total int
		expected          string
	}{
		{1, 10, 100, "1-10/100"},
		{5, 5, 100, "5/100"},
		{0, 0, 0, "0 lines"},
	}

	for _, tt := range tests {
		result := formatLineRange(tt.start, tt.end, tt.total)
		if result != tt.expected {
			t.Errorf("formatLineRange(%d,%d,%d) = %q, want %q",
				tt.start, tt.end, tt.total, result, tt.expected)
		}
	}
}
