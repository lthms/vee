package kb

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// DetectContradictions finds the top-N similar statements to stmtID via KNN,
// then uses the model to judge whether any pair is contradictory.
func (kb *KnowledgeBase) DetectContradictions(stmtID string) error {
	// Load the target statement
	stmt, err := kb.GetStatement(stmtID)
	if err != nil {
		return fmt.Errorf("get statement: %w", err)
	}

	// Load its embedding
	var embBlob []byte
	err = kb.db.QueryRow(
		`SELECT embedding FROM statements WHERE id = ? AND embedding IS NOT NULL`, stmtID,
	).Scan(&embBlob)
	if err != nil {
		return fmt.Errorf("load embedding: %w", err)
	}
	stmtEmb := blobToEmbedding(embBlob)

	// Find top-5 most similar active statements (excluding self)
	rows, err := kb.db.Query(
		`SELECT id, content, embedding FROM statements
		 WHERE status = 'active' AND id != ? AND embedding IS NOT NULL`,
		stmtID,
	)
	if err != nil {
		return fmt.Errorf("load candidates: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id      string
		content string
		score   float64
	}
	var candidates []candidate

	for rows.Next() {
		var id, content string
		var cBlob []byte
		if err := rows.Scan(&id, &content, &cBlob); err != nil {
			continue
		}
		score := cosineSimilarity(stmtEmb, blobToEmbedding(cBlob))
		candidates = append(candidates, candidate{id: id, content: content, score: score})
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

	// Check each candidate for contradiction
	for _, c := range candidates {
		if c.score < kb.threshold {
			continue
		}

		// Skip if we already have a nogood for this pair
		var exists int
		kb.db.QueryRow(
			`SELECT COUNT(*) FROM nogoods WHERE (stmt_a_id = ? AND stmt_b_id = ?) OR (stmt_a_id = ? AND stmt_b_id = ?)`,
			stmtID, c.id, c.id, stmtID,
		).Scan(&exists)
		if exists > 0 {
			continue
		}

		// Ask model to judge contradiction
		startTime := time.Now()
		response, err := kb.model.Generate(fmt.Sprintf(
			`Do these two statements contradict each other? Answer ONLY "yes" or "no", followed by a brief explanation.

Statement A:
%s

Statement B:
%s`,
			stmt.Content, c.content,
		))
		durationMs := int(time.Since(startTime).Milliseconds())
		kb.LogModelCall(kb.embeddingModel, "contradiction_check", "statement", stmtID, durationMs)

		if err != nil {
			slog.Warn("nogood: model judgment failed", "stmt", stmtID, "candidate", c.id, "error", err)
			continue
		}

		response = strings.TrimSpace(response)
		if strings.HasPrefix(strings.ToLower(response), "yes") {
			now := time.Now().Format("2006-01-02T15:04:05Z")
			_, err := kb.db.Exec(
				`INSERT OR IGNORE INTO nogoods (stmt_a_id, stmt_b_id, explanation, detected_at)
				 VALUES (?, ?, ?, ?)`,
				stmtID, c.id, response, now,
			)
			if err != nil {
				slog.Warn("nogood: insert failed", "error", err)
			} else {
				slog.Info("nogood: contradiction detected", "a", stmtID, "b", c.id)
			}
		}
	}

	return nil
}
