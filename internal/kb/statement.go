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
	ID              string   `json:"id"`
	NearDuplicates  []string `json:"near_duplicates,omitempty"`
	CandidatePairs  int      `json:"candidate_pairs,omitempty"`
}

// AddStatement creates a new statement, computes its embedding synchronously,
// detects near-duplicates, and queues candidate pairs for review.
// No model calls — only pure-math cosine similarity.
func (kb *KnowledgeBase) AddStatement(statement, source, sourceType string) (*AddStatementResult, error) {
	if len(statement) > MaxStatementSize {
		return nil, ErrStatementTooLarge
	}

	if sourceType == "" {
		sourceType = "manual"
	}

	id := newStatementID()
	now := time.Now().Format("2006-01-02")

	// Compute embedding synchronously
	emb, err := kb.embedText(statement)
	if err != nil {
		slog.Warn("add-statement: embedding failed, storing without", "id", id, "error", err)
	}

	var embBlob []byte
	if emb != nil {
		embBlob = embeddingToBlob(emb)
	}

	// Tx 1: insert the statement
	_, err = kb.db.Exec(
		`INSERT INTO statements (id, content, source, source_type, status, embedding, model, created_at, last_verified)
		 VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?)`,
		id, statement, source, sourceType, embBlob, kb.embeddingModel, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert statement: %w", err)
	}

	preview := statement
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	slog.Info("statement added", "id", id, "content", preview)

	result := &AddStatementResult{ID: id}

	// Find near-duplicates and queue candidate pairs (only if we have an embedding)
	if emb != nil {
		dups, queued := kb.queueCandidatePairs(id, emb)
		result.NearDuplicates = dups
		result.CandidatePairs = queued
	}

	return result, nil
}

// queueCandidatePairs finds near-duplicate statements by cosine similarity
// and inserts candidate pairs into the nogoods table for later review.
// Pure math — no model calls. Returns near-duplicate IDs and the number
// of new candidate pairs queued.
func (kb *KnowledgeBase) queueCandidatePairs(stmtID string, stmtEmb []float64) (nearDups []string, queued int) {
	rows, err := kb.db.Query(
		`SELECT id, embedding FROM statements
		 WHERE status = 'active' AND id != ? AND embedding IS NOT NULL`,
		stmtID,
	)
	if err != nil {
		slog.Warn("queue-candidates: load candidates failed", "error", err)
		return nil, 0
	}
	defer rows.Close()

	type candidate struct {
		id    string
		score float64
	}
	var candidates []candidate

	for rows.Next() {
		var cID string
		var cBlob []byte
		if err := rows.Scan(&cID, &cBlob); err != nil {
			continue
		}
		score := cosineSimilarity(stmtEmb, blobToEmbedding(cBlob))
		if score >= kb.threshold {
			candidates = append(candidates, candidate{id: cID, score: score})
		}
	}

	// Sort by similarity descending, take top 5
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}

	now := time.Now().Format("2006-01-02T15:04:05Z")

	for _, c := range candidates {
		nearDups = append(nearDups, c.id)

		// Skip if a nogood already exists for this pair
		var exists int
		kb.db.QueryRow(
			`SELECT COUNT(*) FROM nogoods WHERE (stmt_a_id = ? AND stmt_b_id = ?) OR (stmt_a_id = ? AND stmt_b_id = ?)`,
			stmtID, c.id, c.id, stmtID,
		).Scan(&exists)
		if exists > 0 {
			continue
		}

		_, err := kb.db.Exec(
			`INSERT OR IGNORE INTO nogoods (stmt_a_id, stmt_b_id, status, detected_at)
			 VALUES (?, ?, 'pending', ?)`,
			stmtID, c.id, now,
		)
		if err != nil {
			slog.Warn("queue-candidates: insert failed", "error", err)
		} else {
			slog.Info("queue-candidates: pair queued for review", "a", stmtID, "b", c.id)
			queued++
		}
	}

	return nearDups, queued
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
