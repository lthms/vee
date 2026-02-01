package kb

import (
	"fmt"
	"log/slog"
)

// Query performs brute-force KNN search over active statement embeddings.
// Returns results sorted by cosine similarity descending, filtered by threshold.
func (kb *KnowledgeBase) Query(query string) ([]QueryResult, error) {
	// Embed the query
	queryEmb, err := kb.embedText(query)
	if err != nil {
		slog.Warn("query: failed to embed query, returning empty", "error", err)
		return nil, nil
	}

	// Load all active statement embeddings
	rows, err := kb.db.Query(
		`SELECT id, content, source, last_verified, embedding
		 FROM statements
		 WHERE status = 'active' AND embedding IS NOT NULL`,
	)
	if err != nil {
		return nil, fmt.Errorf("query statements: %w", err)
	}
	defer rows.Close()

	type scored struct {
		result QueryResult
		score  float64
	}
	var candidates []scored

	for rows.Next() {
		var id, content, source, lastVerified string
		var embBlob []byte
		if err := rows.Scan(&id, &content, &source, &lastVerified, &embBlob); err != nil {
			slog.Warn("query: scan row", "error", err)
			continue
		}

		emb := blobToEmbedding(embBlob)
		score := cosineSimilarity(queryEmb, emb)

		if score < kb.threshold {
			continue
		}

		// Truncate content for result preview
		preview := content
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}

		candidates = append(candidates, scored{
			result: QueryResult{
				ID:           id,
				Content:      preview,
				Source:       source,
				Score:        score,
				LastVerified: lastVerified,
			},
			score: score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate statements: %w", err)
	}

	// Sort by score descending (insertion sort, fine for small N)
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// Cap at maxResults
	if len(candidates) > kb.maxResults {
		candidates = candidates[:kb.maxResults]
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	results := make([]QueryResult, len(candidates))
	for i, c := range candidates {
		results[i] = c.result
	}
	return results, nil
}
