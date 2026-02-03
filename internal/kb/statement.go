package kb

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// truncateRunes truncates s to n runes, appending "..." if truncated.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// MaxStatementSize is the maximum allowed length (in bytes) for a statement.
const MaxStatementSize = 2000

// ErrStatementTooLarge is returned when a statement exceeds MaxStatementSize.
var ErrStatementTooLarge = errors.New("statement exceeds 2000 character limit")

// Statement represents a full statement row.
type Statement struct {
	ID           string `json:"id"`
	Content      string `json:"content"`
	Source       string `json:"source"`
	SourceType   string `json:"source_type"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	LastVerified string `json:"last_verified"`
}

// AddStatementResult holds the outcome of adding a statement.
type AddStatementResult struct {
	ID string `json:"id"`
}

// AddStatement creates a new statement with status "pending" and no embedding.
// The background worker will compute the embedding and promote the statement.
func (kb *KnowledgeBase) AddStatement(statement, source, sourceType string) (*AddStatementResult, error) {
	if len(statement) > MaxStatementSize {
		return nil, ErrStatementTooLarge
	}

	if sourceType == "" {
		sourceType = "manual"
	}

	id := newStatementID()
	now := time.Now().Format("2006-01-02")

	_, err := kb.db.Exec(
		`INSERT INTO statements (id, content, source, source_type, status, embedding, model, created_at, last_verified)
		 VALUES (?, ?, ?, ?, 'pending', NULL, '', ?, ?)`,
		id, statement, source, sourceType, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert statement: %w", err)
	}

	slog.Info("statement added (pending)", "id", id, "content", truncateRunes(statement, 80))

	// Notify worker (non-blocking)
	select {
	case kb.notifyCh <- struct{}{}:
	default:
	}

	return &AddStatementResult{ID: id}, nil
}

// PromoteStatement sets a statement's status to "active".
func (kb *KnowledgeBase) PromoteStatement(id string) error {
	result, err := kb.db.Exec(`UPDATE statements SET status = 'active' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("promote statement: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("statement not found: %s", id)
	}
	slog.Info("statement promoted", "id", id)
	return nil
}

// DeleteStatement removes a statement from the database.
func (kb *KnowledgeBase) DeleteStatement(id string) error {
	result, err := kb.db.Exec(`DELETE FROM statements WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete statement: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("statement not found: %s", id)
	}
	slog.Info("statement deleted", "id", id)
	return nil
}

// GetStatement retrieves a statement by ID.
func (kb *KnowledgeBase) GetStatement(id string) (*Statement, error) {
	var s Statement
	err := kb.db.QueryRow(
		`SELECT id, content, source, source_type, status, created_at, last_verified
		 FROM statements WHERE id = ?`, id,
	).Scan(&s.ID, &s.Content, &s.Source, &s.SourceType, &s.Status, &s.CreatedAt, &s.LastVerified)
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
