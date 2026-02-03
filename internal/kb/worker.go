package kb

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"
)

// RunWorker processes pending statements in a loop: computes embeddings,
// checks for duplicates, and promotes clean statements to active.
// It listens on the notify channel for new inserts and polls every 30s as fallback.
// Blocks until ctx is cancelled.
func (kb *KnowledgeBase) RunWorker(ctx context.Context) {
	slog.Info("kb worker started")
	defer slog.Info("kb worker stopped")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		kb.processPending(ctx)

		select {
		case <-ctx.Done():
			return
		case <-kb.notifyCh:
			// New statement inserted, process it
		case <-ticker.C:
			// Fallback poll
		}
	}
}

// processPending processes all pending statements one at a time in FIFO order.
// Statements that can't be promoted (have open issues or failed embedding) are
// skipped for this cycle and retried on the next notification or poll.
func (kb *KnowledgeBase) processPending(ctx context.Context) {
	seen := make(map[string]bool)

	for {
		if ctx.Err() != nil {
			return
		}

		stmt, err := kb.nextPendingExcluding(seen)
		if err != nil {
			slog.Warn("worker: failed to fetch next pending", "error", err)
			return
		}
		if stmt == nil {
			return // no more pending statements to process this cycle
		}

		promoted := kb.processOne(ctx, stmt)
		if !promoted {
			seen[stmt.id] = true
		}
	}
}

// pendingRow holds a pending statement with its current embedding state.
type pendingRow struct {
	id        string
	content   string
	embedding []byte // nil if not yet computed
}

// nextPendingExcluding returns the oldest pending statement not in the exclude set.
func (kb *KnowledgeBase) nextPendingExcluding(exclude map[string]bool) (*pendingRow, error) {
	rows, err := kb.db.Query(
		`SELECT id, content, embedding FROM statements
		 WHERE status = 'pending'
		 ORDER BY created_at ASC, id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var row pendingRow
		if err := rows.Scan(&row.id, &row.content, &row.embedding); err != nil {
			return nil, err
		}
		if !exclude[row.id] {
			return &row, nil
		}
	}
	return nil, rows.Err()
}

// processOne handles a single pending statement: embed, check dups, promote or flag.
// Returns true if the statement was promoted, false otherwise.
func (kb *KnowledgeBase) processOne(ctx context.Context, row *pendingRow) bool {
	// Step 1: compute embedding if missing
	if row.embedding == nil {
		emb, err := kb.embedText(row.content)
		if err != nil {
			// Ollama unavailable — skip, retry next cycle
			slog.Debug("worker: embedding failed, will retry", "id", row.id, "error", err)
			return false
		}

		blob := embeddingToBlob(emb)
		_, err = kb.db.Exec(
			`UPDATE statements SET embedding = ?, model = ? WHERE id = ?`,
			blob, kb.embeddingModel, row.id,
		)
		if err != nil {
			slog.Warn("worker: failed to store embedding", "id", row.id, "error", err)
			return false
		}
		row.embedding = blob
		slog.Debug("worker: embedding computed", "id", row.id)
	}

	if ctx.Err() != nil {
		return false
	}

	// Step 2: check for duplicates against all statements with embeddings
	// (both active and other pending — avoids blind spots)
	newEmb := blobToEmbedding(row.embedding)
	hasDup := false

	rows, err := kb.db.Query(
		`SELECT id, embedding FROM statements
		 WHERE id != ? AND embedding IS NOT NULL AND model = ?`,
		row.id, kb.embeddingModel,
	)
	if err != nil {
		slog.Warn("worker: failed to query candidates", "id", row.id, "error", err)
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var candID string
		var candBlob []byte
		if err := rows.Scan(&candID, &candBlob); err != nil {
			continue
		}

		candEmb := blobToEmbedding(candBlob)
		score := cosineSimilarity(newEmb, candEmb)

		if score >= kb.dupThreshold {
			// Check if an issue already exists for this pair
			exists, err := kb.issueExists(row.id, candID)
			if err != nil {
				slog.Warn("worker: failed to check existing issue", "error", err)
				continue
			}
			if !exists {
				issueID := newIssueID()
				now := time.Now().Format("2006-01-02T15:04:05Z")
				_, err = kb.db.Exec(
					`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at)
					 VALUES (?, 'duplicate', 'open', ?, ?, ?, ?)`,
					issueID, row.id, candID, score, now,
				)
				if err != nil {
					slog.Warn("worker: failed to create issue", "id", row.id, "candidate", candID, "error", err)
				} else {
					slog.Info("worker: duplicate issue created", "issue", issueID, "a", row.id, "b", candID, "score", fmt.Sprintf("%.3f", score))
				}
			}
			hasDup = true
		}
	}
	rows.Close()

	// Step 3: promote if no issues, keep pending otherwise
	if !hasDup {
		if err := kb.PromoteStatement(row.id); err != nil {
			slog.Warn("worker: failed to promote", "id", row.id, "error", err)
			return false
		}
		return true
	}

	slog.Debug("worker: statement has duplicates, staying pending", "id", row.id)
	return false
}

// issueExists checks if an open issue already exists for this pair (in either order).
func (kb *KnowledgeBase) issueExists(idA, idB string) (bool, error) {
	var count int
	err := kb.db.QueryRow(
		`SELECT COUNT(*) FROM issues
		 WHERE status = 'open'
		   AND ((statement_a = ? AND statement_b = ?) OR (statement_a = ? AND statement_b = ?))`,
		idA, idB, idB, idA,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// newIssueID generates a UUID v4 for issue IDs.
func newIssueID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
