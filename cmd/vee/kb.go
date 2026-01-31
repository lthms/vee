package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// KnowledgeBase provides persistent note storage backed by markdown files
// and a SQLite FTS5 index for fast search.
type KnowledgeBase struct {
	db       *sql.DB
	vaultDir string
}

// QueryResult is a single search hit from the FTS5 index.
type QueryResult struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

// OpenKnowledgeBase opens (or creates) the knowledge base at
// ~/.local/state/vee/. The vault directory and SQLite database are
// created on first use.
func OpenKnowledgeBase() (*KnowledgeBase, error) {
	stateDir, err := stateDir()
	if err != nil {
		return nil, err
	}

	vaultDir := filepath.Join(stateDir, "vault")
	if err := os.MkdirAll(vaultDir, 0700); err != nil {
		return nil, fmt.Errorf("create vault dir: %w", err)
	}

	dbPath := filepath.Join(stateDir, "kb.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &KnowledgeBase{db: db, vaultDir: vaultDir}, nil
}

// AddNote writes a markdown file to the vault and inserts it into the
// FTS5 index. Returns the absolute file path.
func (kb *KnowledgeBase) AddNote(title, content string, tags []string) (string, error) {
	filename := sanitizeFilename(title) + ".md"
	relPath := filename
	absPath := filepath.Join(kb.vaultDir, filename)

	// Build Obsidian-compatible YAML frontmatter
	tagsYAML := "[]"
	if len(tags) > 0 {
		quoted := make([]string, len(tags))
		for i, t := range tags {
			quoted[i] = fmt.Sprintf("%q", t)
		}
		tagsYAML = "[" + strings.Join(quoted, ", ") + "]"
	}

	now := time.Now().Format("2006-01-02")
	md := fmt.Sprintf("---\ntitle: %q\ntags: %s\ncreated: %s\n---\n\n%s\n", title, tagsYAML, now, content)

	if err := os.WriteFile(absPath, []byte(md), 0600); err != nil {
		return "", fmt.Errorf("write note: %w", err)
	}

	tagStr := strings.Join(tags, ",")
	_, err := kb.db.Exec(
		`INSERT OR REPLACE INTO notes (path, title, tags, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		relPath, title, tagStr, content, now,
	)
	if err != nil {
		return "", fmt.Errorf("insert note: %w", err)
	}

	return absPath, nil
}

// Query performs a full-text search and returns up to limit results with snippets.
func (kb *KnowledgeBase) Query(query string, limit int) ([]QueryResult, error) {
	rows, err := kb.db.Query(`
		SELECT n.path, n.title, snippet(notes_fts, 2, '**', '**', '...', 32)
		FROM notes_fts
		JOIN notes n ON n.id = notes_fts.rowid
		WHERE notes_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var results []QueryResult
	for rows.Next() {
		var r QueryResult
		if err := rows.Scan(&r.Path, &r.Title, &r.Snippet); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// Close closes the underlying database connection.
func (kb *KnowledgeBase) Close() error {
	return kb.db.Close()
}

// QueryResultsJSON marshals query results to JSON text suitable for MCP responses.
func QueryResultsJSON(results []QueryResult) string {
	if results == nil {
		results = []QueryResult{}
	}
	out, _ := json.Marshal(results)
	return string(out)
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".local", "state", "vee")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notes (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			path       TEXT UNIQUE NOT NULL,
			title      TEXT NOT NULL,
			tags       TEXT NOT NULL DEFAULT '',
			content    TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS notes_fts USING fts5(
			title, tags, content,
			content=notes, content_rowid=id
		)`,
		`CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes BEGIN
			INSERT INTO notes_fts(rowid, title, tags, content)
			VALUES (new.id, new.title, new.tags, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS notes_ad AFTER DELETE ON notes BEGIN
			INSERT INTO notes_fts(notes_fts, rowid, title, tags, content)
			VALUES ('delete', old.id, old.title, old.tags, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS notes_au AFTER UPDATE ON notes BEGIN
			INSERT INTO notes_fts(notes_fts, rowid, title, tags, content)
			VALUES ('delete', old.id, old.title, old.tags, old.content);
			INSERT INTO notes_fts(rowid, title, tags, content)
			VALUES (new.id, new.title, new.tags, new.content);
		END`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", s[:40], err)
		}
	}
	return nil
}

// sanitizeFilename turns a title into a safe filename (no path separators, etc).
func sanitizeFilename(title string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	name := replacer.Replace(title)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "untitled"
	}
	return name
}
