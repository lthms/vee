package kb

import (
	"fmt"
	"log/slog"
	"sort"
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

	// Load all active statement embeddings matching the current model
	rows, err := kb.db.Query(
		`SELECT id, content, source, last_verified, embedding
		 FROM statements
		 WHERE status = 'active' AND embedding IS NOT NULL AND model = ?`,
		kb.embeddingModel,
	)
	if err != nil {
		return nil, fmt.Errorf("query statements: %w", err)
	}
	defer rows.Close()

	var candidates []QueryResult

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

		candidates = append(candidates, QueryResult{
			ID:           id,
			Content:      truncateRunes(content, 200),
			Source:       source,
			Score:        score,
			LastVerified: lastVerified,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate statements: %w", err)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	// Cap at maxResults
	if len(candidates) > kb.maxResults {
		candidates = candidates[:kb.maxResults]
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	return candidates, nil
}
