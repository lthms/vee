package kb

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
)

// AssignToCluster assigns a statement to its nearest cluster, or creates a new
// cluster if no existing cluster is similar enough.
func (kb *KnowledgeBase) AssignToCluster(stmtID string) error {
	// Load statement embedding
	var embBlob []byte
	err := kb.db.QueryRow(
		`SELECT embedding FROM statements WHERE id = ? AND embedding IS NOT NULL`, stmtID,
	).Scan(&embBlob)
	if err != nil {
		return fmt.Errorf("load statement embedding: %w", err)
	}
	stmtEmb := blobToEmbedding(embBlob)

	// Load all cluster embeddings
	rows, err := kb.db.Query(`SELECT id, embedding FROM clusters WHERE embedding IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("load clusters: %w", err)
	}
	defer rows.Close()

	var bestClusterID string
	var bestScore float64

	for rows.Next() {
		var cID string
		var cBlob []byte
		if err := rows.Scan(&cID, &cBlob); err != nil {
			continue
		}
		score := cosineSimilarity(stmtEmb, blobToEmbedding(cBlob))
		if score > bestScore {
			bestScore = score
			bestClusterID = cID
		}
	}

	// If best cluster exceeds threshold, join it; otherwise create new cluster
	if bestClusterID != "" && bestScore >= kb.threshold {
		_, err := kb.db.Exec(
			`INSERT OR IGNORE INTO cluster_members (cluster_id, statement_id) VALUES (?, ?)`,
			bestClusterID, stmtID,
		)
		if err != nil {
			return fmt.Errorf("join cluster: %w", err)
		}
		slog.Debug("cluster: assigned to existing", "stmt", stmtID, "cluster", bestClusterID, "score", bestScore)
		return nil
	}

	// Create a new cluster with this statement as founding member
	clusterID := contentAddressedClusterID(stmtID)

	// Get statement content for initial cluster label
	var content string
	kb.db.QueryRow(`SELECT content FROM statements WHERE id = ?`, stmtID).Scan(&content)
	label := content
	if len(label) > 120 {
		label = label[:120] + "..."
	}

	_, err = kb.db.Exec(
		`INSERT INTO clusters (id, label, summary, embedding, model) VALUES (?, ?, '', ?, ?)`,
		clusterID, label, embBlob, kb.embeddingModel,
	)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	_, err = kb.db.Exec(
		`INSERT INTO cluster_members (cluster_id, statement_id) VALUES (?, ?)`,
		clusterID, stmtID,
	)
	if err != nil {
		return fmt.Errorf("add founding member: %w", err)
	}

	slog.Debug("cluster: created new", "stmt", stmtID, "cluster", clusterID)
	return nil
}

// ReclusterDirty is a placeholder for re-clustering operations.
// In a full implementation, this would merge similar clusters, split large ones, etc.
func (kb *KnowledgeBase) ReclusterDirty() error {
	// Stub: no-op for initial implementation
	return nil
}

// contentAddressedClusterID derives a cluster ID from founding member IDs.
func contentAddressedClusterID(memberIDs ...string) string {
	h := sha256.New()
	for _, id := range memberIDs {
		h.Write([]byte(id))
	}
	sum := h.Sum(nil)
	return fmt.Sprintf("c-%x", sum[:8])
}
