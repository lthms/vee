package kb

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// cosineSimilarity computes the cosine similarity between two vectors.
// Returns 0 if either vector has zero magnitude.
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		dot += a[i] * b[i]
		magA += a[i] * a[i]
		magB += b[i] * b[i]
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / (math.Sqrt(magA) * math.Sqrt(magB))
}

// embeddingToBlob serializes a float64 slice into a binary blob (little-endian).
func embeddingToBlob(emb []float64) []byte {
	buf := make([]byte, len(emb)*8)
	for i, v := range emb {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}
	return buf
}

// blobToEmbedding deserializes a binary blob back to a float64 slice.
func blobToEmbedding(blob []byte) []float64 {
	n := len(blob) / 8
	emb := make([]float64, n)
	for i := 0; i < n; i++ {
		emb[i] = math.Float64frombits(binary.LittleEndian.Uint64(blob[i*8:]))
	}
	return emb
}

// storeNodeEmbedding stores (or replaces) an embedding for a tree node.
func (kb *KnowledgeBase) storeNodeEmbedding(nodeID int, emb []float64) error {
	now := time.Now().Format("2006-01-02T15:04:05Z")
	blob := embeddingToBlob(emb)
	_, err := kb.db.Exec(
		`INSERT OR REPLACE INTO node_embeddings (node_id, model, embedding, updated_at) VALUES (?, ?, ?, ?)`,
		nodeID, kb.embeddingModel, blob, now,
	)
	return err
}

// getNodeEmbedding retrieves the embedding for a node. Returns nil if not found
// or if the stored model differs from the current embedding model.
func (kb *KnowledgeBase) getNodeEmbedding(nodeID int) ([]float64, error) {
	var model string
	var blob []byte
	err := kb.db.QueryRow(
		`SELECT model, embedding FROM node_embeddings WHERE node_id = ?`, nodeID,
	).Scan(&model, &blob)
	if err != nil {
		return nil, err
	}
	if model != kb.embeddingModel {
		return nil, fmt.Errorf("stale embedding model: stored %q, current %q", model, kb.embeddingModel)
	}
	return blobToEmbedding(blob), nil
}

// embedAndStoreNode computes and stores an embedding for a node's summary.
// Returns the embedding, or nil on failure.
func (kb *KnowledgeBase) embedAndStoreNode(nodeID int, summary string) []float64 {
	embeddings, err := kb.model.Embed([]string{summary})
	if err != nil || len(embeddings) == 0 {
		slog.Warn("embed: failed to compute embedding", "nodeID", nodeID, "error", err)
		return nil
	}
	emb := embeddings[0]
	if err := kb.storeNodeEmbedding(nodeID, emb); err != nil {
		slog.Warn("embed: failed to store embedding", "nodeID", nodeID, "error", err)
	}
	return emb
}

// selectByThreshold selects nodes whose cosine similarity to the query embedding
// exceeds the threshold, up to maxCount. Returns all node IDs if none exceed
// the threshold (fallback behavior).
func selectByThreshold(queryEmb []float64, nodes []*treeNode, embeddings map[int][]float64, threshold float64, maxCount int) []*treeNode {
	type scored struct {
		node  *treeNode
		score float64
	}

	var candidates []scored
	for _, n := range nodes {
		emb, ok := embeddings[n.id]
		if !ok {
			continue
		}
		score := cosineSimilarity(queryEmb, emb)
		if score >= threshold {
			candidates = append(candidates, scored{node: n, score: score})
		}
	}

	if len(candidates) == 0 {
		// Fallback: return all nodes
		return nodes
	}

	// Sort by score descending (insertion sort is fine for small N)
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// Cap at maxCount
	if maxCount > 0 && len(candidates) > maxCount {
		candidates = candidates[:maxCount]
	}

	selected := make([]*treeNode, len(candidates))
	for i, c := range candidates {
		selected[i] = c.node
	}
	return selected
}

// loadNodeEmbeddings loads embeddings for a list of nodes, computing on-the-fly
// for any nodes missing a cached embedding.
func (kb *KnowledgeBase) loadNodeEmbeddings(nodes []*treeNode) map[int][]float64 {
	embeddings := make(map[int][]float64, len(nodes))
	for _, n := range nodes {
		emb, err := kb.getNodeEmbedding(n.id)
		if err != nil {
			// Compute on-the-fly
			emb = kb.embedAndStoreNode(n.id, n.summary)
		}
		if emb != nil {
			embeddings[n.id] = emb
		}
	}
	return embeddings
}

// BackfillEmbeddings computes and stores embeddings for all tree nodes that
// have a real summary (summary != label) but no embedding for the current model.
func (kb *KnowledgeBase) BackfillEmbeddings() {
	rows, err := kb.db.Query(`
		SELECT tn.id, tn.summary FROM tree_nodes tn
		LEFT JOIN node_embeddings ne ON ne.node_id = tn.id AND ne.model = ?
		WHERE tn.summary != tn.label AND ne.node_id IS NULL`,
		kb.embeddingModel,
	)
	if err != nil {
		slog.Error("backfill-embeddings: query nodes", "error", err)
		return
	}
	defer rows.Close()

	type nodeEntry struct {
		id      int
		summary string
	}
	var nodes []nodeEntry
	for rows.Next() {
		var n nodeEntry
		if err := rows.Scan(&n.id, &n.summary); err != nil {
			slog.Error("backfill-embeddings: scan", "error", err)
			continue
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		slog.Error("backfill-embeddings: iterate", "error", err)
		return
	}

	if len(nodes) == 0 {
		return
	}

	slog.Info("backfill-embeddings: embedding nodes", "count", len(nodes))

	for _, n := range nodes {
		kb.embedAndStoreNode(n.id, n.summary)
	}

	// Clean up orphaned embeddings (node no longer exists)
	kb.db.Exec(`DELETE FROM node_embeddings WHERE node_id NOT IN (SELECT id FROM tree_nodes)`)

	slog.Info("backfill-embeddings: done")
}
