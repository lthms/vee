package kb

import (
	"fmt"
	"log/slog"
	"sync"
)

// Query performs a tree traversal to find relevant notes for a query.
// Uses embedding-based routing when embeddings are available, falling back
// to including all children when embeddings fail.
func (kb *KnowledgeBase) Query(query string) ([]QueryResult, error) {
	roots, err := kb.getRootNodes()
	if err != nil {
		return nil, fmt.Errorf("get roots: %w", err)
	}

	if len(roots) == 0 {
		return nil, nil
	}

	// Embed the query once for the entire traversal
	queryEmb, err := kb.embedQuery(query)
	if err != nil {
		slog.Warn("query: failed to embed query, will include all roots", "error", err)
	}

	// Select relevant roots via embeddings
	selectedRoots := kb.selectNodes(roots, queryEmb)
	if len(selectedRoots) == 0 {
		return nil, nil
	}

	var mu sync.Mutex
	noteIDs := make(map[int]struct{})
	var wg sync.WaitGroup

	for _, node := range selectedRoots {
		wg.Add(1)
		go func(node *treeNode) {
			defer wg.Done()
			ids, err := kb.descendTree(node.id, queryEmb)
			if err != nil {
				slog.Warn("query: descent failed", "node", node.label, "error", err)
				return
			}
			mu.Lock()
			for _, id := range ids {
				noteIDs[id] = struct{}{}
			}
			mu.Unlock()
		}(node)
	}
	wg.Wait()

	if len(noteIDs) == 0 {
		return nil, nil
	}

	var results []QueryResult
	for id := range noteIDs {
		var r QueryResult
		err := kb.db.QueryRow(
			`SELECT path, title, summary, last_verified FROM notes WHERE id = ?`, id,
		).Scan(&r.Path, &r.Title, &r.Summary, &r.LastVerified)
		if err != nil {
			continue
		}
		results = append(results, r)
	}

	return results, nil
}

// descendTree recursively traverses the tree to collect note IDs matching a query.
// queryEmb is the pre-computed query embedding (may be nil if embedding failed).
func (kb *KnowledgeBase) descendTree(nodeID int, queryEmb []float64) ([]int, error) {
	var isLeaf bool
	err := kb.db.QueryRow(`SELECT is_leaf FROM tree_nodes WHERE id = ?`, nodeID).Scan(&isLeaf)
	if err != nil {
		return nil, err
	}

	if isLeaf {
		return kb.getLeafNoteIDs(nodeID)
	}

	children, err := kb.getNodeChildren(nodeID)
	if err != nil {
		return nil, err
	}

	if len(children) == 0 {
		return nil, nil
	}

	selected := kb.selectNodes(children, queryEmb)
	if len(selected) == 0 {
		return nil, nil
	}

	var mu sync.Mutex
	var allIDs []int
	var wg sync.WaitGroup

	for _, child := range selected {
		wg.Add(1)
		go func(child *treeNode) {
			defer wg.Done()
			ids, err := kb.descendTree(child.id, queryEmb)
			if err != nil {
				slog.Warn("query: recursive descent failed", "node", child.label, "error", err)
				return
			}
			mu.Lock()
			allIDs = append(allIDs, ids...)
			mu.Unlock()
		}(child)
	}
	wg.Wait()

	return allIDs, nil
}

// embedQuery embeds the query text. Returns nil on failure.
func (kb *KnowledgeBase) embedQuery(query string) ([]float64, error) {
	embeddings, err := kb.model.Embed([]string{query})
	if err != nil {
		return nil, err
	}
	if len(embeddings) == 0 {
		return nil, fmt.Errorf("empty embedding result")
	}
	return embeddings[0], nil
}

// selectNodes picks the most relevant nodes using embedding similarity.
// Falls back to returning all nodes if queryEmb is nil or no embeddings available.
func (kb *KnowledgeBase) selectNodes(nodes []*treeNode, queryEmb []float64) []*treeNode {
	if queryEmb == nil {
		// No query embedding → include everything
		return nodes
	}

	embeddings := kb.loadNodeEmbeddings(nodes)
	if len(embeddings) == 0 {
		// No embeddings available → include everything
		return nodes
	}

	return selectByThreshold(queryEmb, nodes, embeddings, kb.threshold, kb.maxSelect)
}
