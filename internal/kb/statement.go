package kb

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// MaxStatementSize is the maximum allowed length (in bytes) for a statement.
const MaxStatementSize = 2000

// ErrStatementTooLarge is returned when a statement exceeds MaxStatementSize.
var ErrStatementTooLarge = errors.New("statement exceeds 2000 character limit")

// Statement represents a full statement row.
type Statement struct {
	ID           string
	Title        string
	Content      string
	Source       string
	SourceType   string
	Status       string
	CreatedAt    string
	LastVerified string
}

// deriveTitle extracts a short title from a statement string.
// Uses the first sentence (to the first '.' or '\n'), capped at 120 chars.
// Falls back to the first 120 chars + "…" if no sentence boundary is found.
func deriveTitle(statement string) string {
	s := strings.TrimSpace(statement)
	if s == "" {
		return ""
	}

	// Find first sentence boundary
	cut := -1
	if i := strings.Index(s, "."); i >= 0 && i < 120 {
		cut = i + 1
	}
	if i := strings.Index(s, "\n"); i >= 0 && (cut < 0 || i < cut) && i < 120 {
		cut = i
	}

	if cut > 0 {
		return strings.TrimSpace(s[:cut])
	}

	if len(s) <= 120 {
		return s
	}
	return s[:120] + "…"
}

// AddStatement creates a new statement, computes its embedding synchronously,
// and enqueues background tasks (cluster_assign, contradiction_check).
// The title is derived automatically from the statement text.
// Returns the statement ID.
func (kb *KnowledgeBase) AddStatement(statement, source, sourceType string) (string, error) {
	if len(statement) > MaxStatementSize {
		return "", ErrStatementTooLarge
	}

	if sourceType == "" {
		sourceType = "manual"
	}

	title := deriveTitle(statement)
	content := statement

	id := newStatementID()
	now := time.Now().Format("2006-01-02")

	// Compute embedding synchronously (~50ms for local model)
	embText := title + "\n" + content
	emb, err := kb.embedText(embText)
	if err != nil {
		slog.Warn("add-statement: embedding failed, storing without", "id", id, "error", err)
	}

	var embBlob []byte
	if emb != nil {
		embBlob = embeddingToBlob(emb)
	}

	_, err = kb.db.Exec(
		`INSERT INTO statements (id, title, content, source, source_type, status, embedding, model, created_at, last_verified)
		 VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)`,
		id, title, content, source, sourceType, embBlob, kb.embeddingModel, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("insert statement: %w", err)
	}

	// Enqueue background tasks
	kb.Enqueue("cluster_assign", id, 5)
	kb.Enqueue("contradiction_check", id, 1)

	slog.Info("statement added", "id", id, "title", title)
	return id, nil
}

// GetStatement retrieves a statement by ID.
func (kb *KnowledgeBase) GetStatement(id string) (*Statement, error) {
	var s Statement
	err := kb.db.QueryRow(
		`SELECT id, title, content, source, source_type, status, created_at, last_verified
		 FROM statements WHERE id = ?`, id,
	).Scan(&s.ID, &s.Title, &s.Content, &s.Source, &s.SourceType, &s.Status, &s.CreatedAt, &s.LastVerified)
	if err != nil {
		return nil, fmt.Errorf("get statement %s: %w", id, err)
	}
	return &s, nil
}

// FetchStatement returns a formatted string representation of a statement.
func (kb *KnowledgeBase) FetchStatement(id string) (string, error) {
	s, err := kb.GetStatement(id)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s\n\n", s.Title))
	sb.WriteString(s.Content)
	sb.WriteString(fmt.Sprintf("\n\n---\nSource: %s\n", s.Source))
	sb.WriteString(fmt.Sprintf("Status: %s\n", s.Status))
	sb.WriteString(fmt.Sprintf("Created: %s\n", s.CreatedAt))
	sb.WriteString(fmt.Sprintf("Last verified: %s\n", s.LastVerified))
	return sb.String(), nil
}

// TouchStatement updates the last_verified timestamp to today.
func (kb *KnowledgeBase) TouchStatement(id string) error {
	now := time.Now().Format("2006-01-02")
	result, err := kb.db.Exec(
		`UPDATE statements SET last_verified = ? WHERE id = ?`, now, id,
	)
	if err != nil {
		return fmt.Errorf("touch statement: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("statement not found: %s", id)
	}
	slog.Info("statement touched", "id", id)
	return nil
}

// newStatementID generates a UUID v4 for statement IDs.
func newStatementID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
