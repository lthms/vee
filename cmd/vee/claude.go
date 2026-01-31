package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed query_prompt.md
var queryPrompt string

//go:embed readnote_prompt.md
var readNotePrompt string

// QueryResult is the JSON returned by the query agent.
type QueryResult struct {
	Notes    []string `json:"notes"`
	Traverse []string `json:"traverse"`
}

// ReadNoteResult is the JSON returned by the read-note agent.
type ReadNoteResult struct {
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// queryIndex calls claude -p with the query prompt to evaluate an index file.
func queryIndex(ctx context.Context, kbRoot, indexPath, topic string) (QueryResult, error) {
	content, err := os.ReadFile(filepath.Join(kbRoot, indexPath))
	if err != nil {
		return QueryResult{}, fmt.Errorf("read index %s: %w", indexPath, err)
	}

	msg := fmt.Sprintf(
		"<kb-root>%s</kb-root>\n<index-path>%s</index-path>\n<topic>%s</topic>\n<file-content>\n%s\n</file-content>",
		kbRoot, indexPath, topic, string(content),
	)

	slog.Debug("calling claude -p for query", "index", indexPath, "topic", topic)
	out, err := callClaude(ctx, queryPrompt, msg)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query index %s: %w", indexPath, err)
	}
	slog.Debug("query result", "index", indexPath, "raw", out)

	var result QueryResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return QueryResult{}, fmt.Errorf("parse query result for %s: %w\nraw: %s", indexPath, err, out)
	}

	return result, nil
}

// readNote calls claude -p with the read-note prompt to summarize a note.
func readNote(ctx context.Context, kbRoot, notePath, topic string) (ReadNoteResult, error) {
	content, err := os.ReadFile(filepath.Join(kbRoot, notePath))
	if err != nil {
		return ReadNoteResult{}, fmt.Errorf("read note %s: %w", notePath, err)
	}

	msg := fmt.Sprintf(
		"<kb-root>%s</kb-root>\n<note-path>%s</note-path>\n<topic>%s</topic>\n<file-content>\n%s\n</file-content>",
		kbRoot, notePath, topic, string(content),
	)

	slog.Debug("calling claude -p for read-note", "note", notePath, "topic", topic)
	out, err := callClaude(ctx, readNotePrompt, msg)
	if err != nil {
		return ReadNoteResult{}, fmt.Errorf("read note %s: %w", notePath, err)
	}
	slog.Debug("read-note result", "note", notePath, "raw", out)

	var result ReadNoteResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return ReadNoteResult{}, fmt.Errorf("parse read-note result for %s: %w\nraw: %s", notePath, err, out)
	}

	return result, nil
}

// callClaude shells out to claude -p with the given system prompt and user message.
func callClaude(ctx context.Context, sysPrompt, userMessage string) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--model", "haiku",
		"--system-prompt", sysPrompt,
		"--tools", "",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
	)

	cmd.Stdin = strings.NewReader(userMessage)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p failed: %w\nstderr: %s", err, stderr.String())
	}

	return stripMarkdownFences(stdout.String()), nil
}

// stripMarkdownFences removes ```json ... ``` wrapping that claude -p adds.
func stripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) < 2 {
		return s
	}

	// Strip opening fence
	if strings.HasPrefix(lines[0], "```") {
		lines = lines[1:]
	}

	// Strip closing fence
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}
