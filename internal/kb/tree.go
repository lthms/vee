package kb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// treeNode is an internal representation of a tree_nodes row.
type treeNode struct {
	id       int
	parentID *int
	label    string
	summary  string
	isLeaf   bool
}

// insertIntoTree inserts a note into the tree under a given tag.
func (kb *KnowledgeBase) insertIntoTree(noteID int, tag, noteSummary string) error {
	rootID, err := kb.findOrCreateRoot(tag)
	if err != nil {
		return fmt.Errorf("find/create root %q: %w", tag, err)
	}

	var isLeaf bool
	err = kb.db.QueryRow(`SELECT is_leaf FROM tree_nodes WHERE id = ?`, rootID).Scan(&isLeaf)
	if err != nil {
		return fmt.Errorf("get root node: %w", err)
	}

	if isLeaf {
		return kb.insertIntoLeaf(rootID, noteID)
	}

	return kb.descendAndInsert(rootID, noteID, noteSummary)
}

func (kb *KnowledgeBase) findOrCreateRoot(tag string) (int, error) {
	var id int
	err := kb.db.QueryRow(`SELECT id FROM tree_nodes WHERE parent_id IS NULL AND label = ?`, tag).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	result, err := kb.db.Exec(
		`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, ?, ?, 1)`,
		tag, tag,
	)
	if err != nil {
		return 0, err
	}
	newID, err := result.LastInsertId()
	return int(newID), err
}

func (kb *KnowledgeBase) insertIntoLeaf(nodeID, noteID int) error {
	_, err := kb.db.Exec(
		`INSERT OR IGNORE INTO leaf_entries (node_id, note_id) VALUES (?, ?)`,
		nodeID, noteID,
	)
	if err != nil {
		return err
	}

	size, err := kb.leafSize(nodeID)
	if err != nil {
		return err
	}
	if size > maxLeafSize {
		if err := kb.splitLeaf(nodeID); err != nil {
			return err
		}
		if err := kb.propagateSummaryUp(nodeID); err != nil {
			slog.Warn("index: propagate summary after split failed", "nodeID", nodeID, "error", err)
		}
		return nil
	}

	if err := kb.updateLeafSummary(nodeID); err != nil {
		slog.Warn("index: update leaf summary failed", "nodeID", nodeID, "error", err)
	}
	if err := kb.propagateSummaryUp(nodeID); err != nil {
		slog.Warn("index: propagate summary up failed", "nodeID", nodeID, "error", err)
	}
	return nil
}

// updateLeafSummary regenerates the summary for a leaf node based on its notes.
func (kb *KnowledgeBase) updateLeafSummary(nodeID int) error {
	var label string
	if err := kb.db.QueryRow(`SELECT label FROM tree_nodes WHERE id = ?`, nodeID).Scan(&label); err != nil {
		return fmt.Errorf("fetch leaf label: %w", err)
	}

	rows, err := kb.db.Query(
		`SELECT n.title, n.summary FROM notes n
		 JOIN leaf_entries le ON le.note_id = n.id
		 WHERE le.node_id = ?`, nodeID,
	)
	if err != nil {
		return fmt.Errorf("fetch leaf notes: %w", err)
	}
	defer rows.Close()

	var notesList strings.Builder
	count := 0
	for rows.Next() {
		var title, summary string
		if err := rows.Scan(&title, &summary); err != nil {
			return fmt.Errorf("scan leaf note: %w", err)
		}
		notesList.WriteString(fmt.Sprintf("- %s: %s\n", title, summary))
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate leaf notes: %w", err)
	}

	if count == 0 {
		return nil
	}

	summary, err := kb.model.Generate(fmt.Sprintf(
		`This category is named %q. Describe what it covers based on its name and the following notes. The summary should reflect the broad scope implied by the category name, not just the current notes. Reply with ONLY a single concise sentence, nothing else.

Notes:
%s`, label, notesList.String(),
	))
	if err != nil {
		return fmt.Errorf("judgment leaf summary: %w", err)
	}

	summary = strings.TrimSpace(summary)
	if _, err := kb.db.Exec(`UPDATE tree_nodes SET summary = ? WHERE id = ?`, summary, nodeID); err != nil {
		return fmt.Errorf("update leaf summary: %w", err)
	}

	// Update embedding for this node
	kb.embedAndStoreNode(nodeID, summary)

	return nil
}

// propagateSummaryUp walks from nodeID up to the root, updating each ancestor's
// summary based on its children's labels and summaries.
func (kb *KnowledgeBase) propagateSummaryUp(nodeID int) error {
	currentID := nodeID

	for {
		var parentID *int
		err := kb.db.QueryRow(`SELECT parent_id FROM tree_nodes WHERE id = ?`, currentID).Scan(&parentID)
		if err != nil {
			return fmt.Errorf("get parent: %w", err)
		}

		if parentID == nil {
			var isLeaf bool
			err := kb.db.QueryRow(`SELECT is_leaf FROM tree_nodes WHERE id = ?`, currentID).Scan(&isLeaf)
			if err != nil {
				return fmt.Errorf("check root leaf: %w", err)
			}
			if !isLeaf {
				if err := kb.updateInternalSummary(currentID); err != nil {
					return err
				}
			}
			return nil
		}

		if err := kb.updateInternalSummary(*parentID); err != nil {
			return err
		}

		currentID = *parentID
	}
}

// updateInternalSummary regenerates the summary for an internal node based on its children.
func (kb *KnowledgeBase) updateInternalSummary(nodeID int) error {
	var label string
	if err := kb.db.QueryRow(`SELECT label FROM tree_nodes WHERE id = ?`, nodeID).Scan(&label); err != nil {
		return fmt.Errorf("fetch internal label: %w", err)
	}

	children, err := kb.getNodeChildren(nodeID)
	if err != nil {
		return fmt.Errorf("get children for summary: %w", err)
	}

	if len(children) == 0 {
		return nil
	}

	var childList strings.Builder
	for _, c := range children {
		childList.WriteString(fmt.Sprintf("- %s: %s\n", c.label, c.summary))
	}

	summary, err := kb.model.Generate(fmt.Sprintf(
		`This category is named %q. Describe what it covers based on its name and the following subcategories. The summary should reflect the broad scope implied by the category name, not just the current subcategories. Reply with ONLY a single concise sentence, nothing else.

Subcategories:
%s`, label, childList.String(),
	))
	if err != nil {
		return fmt.Errorf("judgment internal summary: %w", err)
	}

	summary = strings.TrimSpace(summary)
	if _, err := kb.db.Exec(`UPDATE tree_nodes SET summary = ? WHERE id = ?`, summary, nodeID); err != nil {
		return fmt.Errorf("update internal summary: %w", err)
	}

	// Update embedding for this node
	kb.embedAndStoreNode(nodeID, summary)

	return nil
}

func (kb *KnowledgeBase) descendAndInsert(nodeID, noteID int, noteSummary string) error {
	children, err := kb.getNodeChildren(nodeID)
	if err != nil {
		return err
	}

	if len(children) == 0 {
		return kb.insertIntoLeaf(nodeID, noteID)
	}

	var childList strings.Builder
	for _, c := range children {
		childList.WriteString(fmt.Sprintf("- %s: %s\n", c.label, c.summary))
	}

	selectedRaw, err := kb.model.Generate(fmt.Sprintf(
		`Given a note summary and a list of subcategories, select ALL subcategories where this note belongs.

Note: %s

Subcategories:
%s
Reply with ONLY a comma-separated list of selected subcategory names (exactly as shown above). If none fit well, reply with the single best match. Nothing else.`,
		noteSummary, childList.String(),
	))
	if err != nil {
		slog.Warn("index: descent selection failed, using first child", "error", err)
		if children[0].isLeaf {
			return kb.insertIntoLeaf(children[0].id, noteID)
		}
		return kb.descendAndInsert(children[0].id, noteID, noteSummary)
	}

	selected := parseCSV(selectedRaw)
	childMap := make(map[string]*treeNode)
	for _, c := range children {
		childMap[c.label] = c
	}

	var wg sync.WaitGroup
	var errs []error
	var errMu sync.Mutex

	for _, name := range selected {
		child, ok := childMap[name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(child *treeNode) {
			defer wg.Done()
			var e error
			if child.isLeaf {
				e = kb.insertIntoLeaf(child.id, noteID)
			} else {
				e = kb.descendAndInsert(child.id, noteID, noteSummary)
			}
			if e != nil {
				errMu.Lock()
				errs = append(errs, e)
				errMu.Unlock()
			}
		}(child)
	}
	wg.Wait()

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// splitLeaf splits a leaf node with > maxLeafSize entries into 2-4 child leaves.
func (kb *KnowledgeBase) splitLeaf(nodeID int) error {
	noteIDs, err := kb.getLeafNoteIDs(nodeID)
	if err != nil {
		return fmt.Errorf("get leaf entries: %w", err)
	}

	type noteSummary struct {
		id      int
		title   string
		summary string
	}
	var notes []noteSummary
	for _, nid := range noteIDs {
		var title, sum string
		err := kb.db.QueryRow(`SELECT title, summary FROM notes WHERE id = ?`, nid).Scan(&title, &sum)
		if err != nil {
			continue
		}
		notes = append(notes, noteSummary{id: nid, title: title, summary: sum})
	}

	if len(notes) < 2 {
		return nil
	}

	var notesList strings.Builder
	for i, n := range notes {
		notesList.WriteString(fmt.Sprintf("%d. %s: %s\n", i, n.title, n.summary))
	}

	clusterJSON, err := kb.model.Generate(fmt.Sprintf(
		`Partition the following notes into 2-4 thematic clusters. A note can appear in multiple clusters if relevant.

Notes:
%s
Reply with ONLY valid JSON in this exact format, nothing else:
[{"label": "cluster-name", "summary": "one sentence description", "notes": [0, 2, 5]}]

Where the numbers are the note indices from the list above.`,
		notesList.String(),
	))
	if err != nil {
		slog.Error("split: judgment clustering failed", "nodeID", nodeID, "error", err)
		return nil // non-fatal: leave the oversized leaf as-is
	}

	type cluster struct {
		Label   string `json:"label"`
		Summary string `json:"summary"`
		Notes   []int  `json:"notes"`
	}
	var clusters []cluster
	clusterJSON = strings.TrimSpace(clusterJSON)
	if err := json.Unmarshal([]byte(clusterJSON), &clusters); err != nil {
		slog.Error("split: failed to parse cluster JSON", "nodeID", nodeID, "raw", clusterJSON, "error", err)
		return nil // non-fatal
	}

	if len(clusters) < 2 {
		return nil
	}

	if _, err := kb.db.Exec(`UPDATE tree_nodes SET is_leaf = 0 WHERE id = ?`, nodeID); err != nil {
		return fmt.Errorf("update parent: %w", err)
	}

	if _, err := kb.db.Exec(`DELETE FROM leaf_entries WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("delete old entries: %w", err)
	}

	for _, c := range clusters {
		result, err := kb.db.Exec(
			`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (?, ?, ?, 1)`,
			nodeID, c.Label, c.Summary,
		)
		if err != nil {
			return fmt.Errorf("create child node: %w", err)
		}
		childID, _ := result.LastInsertId()

		for _, idx := range c.Notes {
			if idx >= 0 && idx < len(notes) {
				kb.db.Exec(
					`INSERT OR IGNORE INTO leaf_entries (node_id, note_id) VALUES (?, ?)`,
					int(childID), notes[idx].id,
				)
			}
		}

		// Compute embedding for new child
		kb.embedAndStoreNode(int(childID), c.Summary)
	}

	count, err := kb.childCount(nodeID)
	if err == nil && count > maxNodeChildren {
		return kb.splitNode(nodeID)
	}

	slog.Info("split: leaf split", "nodeID", nodeID, "clusters", len(clusters))
	return nil
}

// splitNode splits an internal node with > maxNodeChildren into 2-4 meta-clusters.
func (kb *KnowledgeBase) splitNode(nodeID int) error {
	children, err := kb.getNodeChildren(nodeID)
	if err != nil {
		return fmt.Errorf("get children: %w", err)
	}

	if len(children) <= maxNodeChildren {
		return nil
	}

	var childList strings.Builder
	for i, c := range children {
		childList.WriteString(fmt.Sprintf("%d. %s: %s\n", i, c.label, c.summary))
	}

	groupJSON, err := kb.model.Generate(fmt.Sprintf(
		`Group the following subcategories into 2-4 meta-groups.

Subcategories:
%s
Reply with ONLY valid JSON in this exact format, nothing else:
[{"label": "group-name", "summary": "one sentence description", "children": [0, 2, 5]}]

Where the numbers are the subcategory indices from the list above.`,
		childList.String(),
	))
	if err != nil {
		slog.Error("split: judgment node grouping failed", "nodeID", nodeID, "error", err)
		return nil
	}

	type group struct {
		Label    string `json:"label"`
		Summary  string `json:"summary"`
		Children []int  `json:"children"`
	}
	var groups []group
	groupJSON = strings.TrimSpace(groupJSON)
	if err := json.Unmarshal([]byte(groupJSON), &groups); err != nil {
		slog.Error("split: failed to parse group JSON", "nodeID", nodeID, "raw", groupJSON, "error", err)
		return nil
	}

	if len(groups) < 2 {
		return nil
	}

	for _, g := range groups {
		result, err := kb.db.Exec(
			`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (?, ?, ?, 0)`,
			nodeID, g.Label, g.Summary,
		)
		if err != nil {
			return fmt.Errorf("create group node: %w", err)
		}
		groupID, _ := result.LastInsertId()

		for _, idx := range g.Children {
			if idx >= 0 && idx < len(children) {
				kb.db.Exec(`UPDATE tree_nodes SET parent_id = ? WHERE id = ?`, int(groupID), children[idx].id)
			}
		}

		// Compute embedding for new group node
		kb.embedAndStoreNode(int(groupID), g.Summary)
	}

	slog.Info("split: node split", "nodeID", nodeID, "groups", len(groups))
	return nil
}

// --- Tree query helpers ---

func (kb *KnowledgeBase) getRootNodes() ([]*treeNode, error) {
	rows, err := kb.db.Query(`SELECT id, parent_id, label, summary, is_leaf FROM tree_nodes WHERE parent_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []*treeNode
	for rows.Next() {
		var n treeNode
		if err := rows.Scan(&n.id, &n.parentID, &n.label, &n.summary, &n.isLeaf); err != nil {
			return nil, err
		}
		nodes = append(nodes, &n)
	}
	return nodes, rows.Err()
}

func (kb *KnowledgeBase) getNodeChildren(nodeID int) ([]*treeNode, error) {
	rows, err := kb.db.Query(`SELECT id, parent_id, label, summary, is_leaf FROM tree_nodes WHERE parent_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []*treeNode
	for rows.Next() {
		var n treeNode
		if err := rows.Scan(&n.id, &n.parentID, &n.label, &n.summary, &n.isLeaf); err != nil {
			return nil, err
		}
		nodes = append(nodes, &n)
	}
	return nodes, rows.Err()
}

func (kb *KnowledgeBase) getLeafNoteIDs(nodeID int) ([]int, error) {
	rows, err := kb.db.Query(`SELECT note_id FROM leaf_entries WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (kb *KnowledgeBase) leafSize(nodeID int) (int, error) {
	var count int
	err := kb.db.QueryRow(`SELECT COUNT(*) FROM leaf_entries WHERE node_id = ?`, nodeID).Scan(&count)
	return count, err
}

func (kb *KnowledgeBase) childCount(nodeID int) (int, error) {
	var count int
	err := kb.db.QueryRow(`SELECT COUNT(*) FROM tree_nodes WHERE parent_id = ?`, nodeID).Scan(&count)
	return count, err
}

func (kb *KnowledgeBase) allRootLabels() ([]string, error) {
	rows, err := kb.db.Query(`SELECT label FROM tree_nodes WHERE parent_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		labels = append(labels, l)
	}
	return labels, rows.Err()
}
