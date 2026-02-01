package kb

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// noteRow is an internal representation of a notes table row.
type noteRow struct {
	id           int
	path         string
	title        string
	tags         string
	summary      string
	lastVerified string
}

// AddNote writes a markdown file to the vault and inserts it into the
// notes table. Returns the note ID and relative file path.
func (kb *KnowledgeBase) AddNote(title, content string, sources []string) (int, string, error) {
	filename := sanitizeFilename(title) + ".md"
	relPath := filename
	absPath := filepath.Join(kb.vaultDir, filename)

	now := time.Now().Format("2006-01-02")

	var sourcesYAML string
	if len(sources) > 0 {
		var sb strings.Builder
		sb.WriteString("sources:\n")
		for _, s := range sources {
			sb.WriteString(fmt.Sprintf("  - %q\n", s))
		}
		sourcesYAML = sb.String()
	} else {
		sourcesYAML = "sources: []\n"
	}

	md := fmt.Sprintf("---\ntitle: %q\ntags: []\n%screated: %s\nlast_verified: %s\n---\n\n%s\n", title, sourcesYAML, now, now, content)

	if err := os.WriteFile(absPath, []byte(md), 0600); err != nil {
		return 0, "", fmt.Errorf("write note: %w", err)
	}

	result, err := kb.db.Exec(
		`INSERT OR REPLACE INTO notes (path, title, tags, created_at, last_verified) VALUES (?, ?, '', ?, ?)`,
		relPath, title, now, now,
	)
	if err != nil {
		return 0, "", fmt.Errorf("insert note: %w", err)
	}

	noteID, err := result.LastInsertId()
	if err != nil {
		return 0, "", fmt.Errorf("last insert id: %w", err)
	}

	return int(noteID), relPath, nil
}

// FetchNote reads a vault markdown file by its relative path and returns
// the raw markdown content.
func (kb *KnowledgeBase) FetchNote(relPath string) (string, error) {
	absPath := filepath.Join(kb.vaultDir, relPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read vault file: %w", err)
	}
	return string(data), nil
}

// TouchNote updates the last_verified timestamp for a note in both the DB
// and the vault file.
func (kb *KnowledgeBase) TouchNote(noteID int) error {
	now := time.Now().Format("2006-01-02")

	if _, err := kb.db.Exec(`UPDATE notes SET last_verified = ? WHERE id = ?`, now, noteID); err != nil {
		return fmt.Errorf("update db: %w", err)
	}

	note, err := kb.getNote(noteID)
	if err != nil {
		return fmt.Errorf("get note: %w", err)
	}

	absPath := filepath.Join(kb.vaultDir, note.path)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("read vault file: %w", err)
	}

	content := string(data)
	if strings.Contains(content, "last_verified:") {
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "last_verified:") {
				lines[i] = "last_verified: " + now
				break
			}
		}
		content = strings.Join(lines, "\n")
	} else if strings.Contains(content, "created:") {
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "created:") {
				rest := append([]string{"last_verified: " + now}, lines[i+1:]...)
				lines = append(lines[:i+1], rest...)
				break
			}
		}
		content = strings.Join(lines, "\n")
	}

	if err := os.WriteFile(absPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write vault file: %w", err)
	}

	slog.Info("touch: note verified", "noteID", noteID, "title", note.title)
	return nil
}

// GetNoteByPath looks up a note by its relative vault path.
func (kb *KnowledgeBase) GetNoteByPath(relPath string) (*NoteInfo, error) {
	var n NoteInfo
	err := kb.db.QueryRow(
		`SELECT id, path, title FROM notes WHERE path = ?`, relPath,
	).Scan(&n.ID, &n.Path, &n.Title)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// --- Internal helpers ---

func (kb *KnowledgeBase) getNote(id int) (*noteRow, error) {
	var n noteRow
	err := kb.db.QueryRow(
		`SELECT id, path, title, tags, summary, last_verified FROM notes WHERE id = ?`, id,
	).Scan(&n.id, &n.path, &n.title, &n.tags, &n.summary, &n.lastVerified)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (kb *KnowledgeBase) recentNoteSummaries(excludeID, limit int) ([]noteRow, error) {
	rows, err := kb.db.Query(
		`SELECT id, path, title, tags, summary FROM notes WHERE id != ? AND summary != '' ORDER BY id DESC LIMIT ?`,
		excludeID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []noteRow
	for rows.Next() {
		var n noteRow
		if err := rows.Scan(&n.id, &n.path, &n.title, &n.tags, &n.summary); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

func (kb *KnowledgeBase) rewriteVaultFile(note *noteRow, content string, tags, relatedTitles []string) {
	absPath := filepath.Join(kb.vaultDir, note.path)

	tagsYAML := "[]"
	if len(tags) > 0 {
		quoted := make([]string, len(tags))
		for i, t := range tags {
			quoted[i] = fmt.Sprintf("%q", t)
		}
		tagsYAML = "[" + strings.Join(quoted, ", ") + "]"
	}

	now := time.Now().Format("2006-01-02")
	lastVerified := note.lastVerified
	if lastVerified == "" {
		lastVerified = now
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("---\ntitle: %q\ntags: %s\ncreated: %s\nlast_verified: %s\n---\n\n%s\n", note.title, tagsYAML, now, lastVerified, content))

	if len(relatedTitles) > 0 {
		sb.WriteString("\n## Related\n\n")
		for _, t := range relatedTitles {
			sb.WriteString(fmt.Sprintf("- [[%s]]\n", t))
		}
	}

	if err := os.WriteFile(absPath, []byte(sb.String()), 0600); err != nil {
		slog.Error("index: failed to rewrite vault file", "path", absPath, "error", err)
	}
}
