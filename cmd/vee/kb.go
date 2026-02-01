package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxLeafSize     = 20
	maxNodeChildren = 10
)

// KnowledgeBase provides persistent note storage backed by markdown files
// and a SQLite tree-based semantic index for retrieval.
type KnowledgeBase struct {
	db       *sql.DB
	vaultDir string
}

// QueryResult is a single search hit from the tree index.
type QueryResult struct {
	Path         string `json:"path"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	LastVerified string `json:"last_verified"`
}

// OpenKnowledgeBase opens (or creates) the knowledge base at
// ~/.local/state/vee/. The vault directory and SQLite database are
// created on first use.
func OpenKnowledgeBase() (*KnowledgeBase, error) {
	stateDir, err := stateDir()
	if err != nil {
		return nil, err
	}

	vaultDir := filepath.Join(stateDir, "vault")
	if err := os.MkdirAll(vaultDir, 0700); err != nil {
		return nil, fmt.Errorf("create vault dir: %w", err)
	}

	dbPath := filepath.Join(stateDir, "kb.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &KnowledgeBase{db: db, vaultDir: vaultDir}, nil
}

// AddNote writes a markdown file to the vault and inserts it into the
// notes table. Returns the note ID and absolute file path.
func (kb *KnowledgeBase) AddNote(title, content string, sources []string) (int, string, error) {
	filename := sanitizeFilename(title) + ".md"
	relPath := filename
	absPath := filepath.Join(kb.vaultDir, filename)

	now := time.Now().Format("2006-01-02")

	var sourcesYAML string
	if len(sources) > 0 {
		var sb strings.Builder
		sb.WriteString("sources:\n")
		for _, s := range sources {
			sb.WriteString(fmt.Sprintf("  - %q\n", s))
		}
		sourcesYAML = sb.String()
	} else {
		sourcesYAML = "sources: []\n"
	}

	md := fmt.Sprintf("---\ntitle: %q\ntags: []\n%screated: %s\nlast_verified: %s\n---\n\n%s\n", title, sourcesYAML, now, now, content)

	if err := os.WriteFile(absPath, []byte(md), 0600); err != nil {
		return 0, "", fmt.Errorf("write note: %w", err)
	}

	result, err := kb.db.Exec(
		`INSERT OR REPLACE INTO notes (path, title, tags, created_at, last_verified) VALUES (?, ?, '', ?, ?)`,
		relPath, title, now, now,
	)
	if err != nil {
		return 0, "", fmt.Errorf("insert note: %w", err)
	}

	noteID, err := result.LastInsertId()
	if err != nil {
		return 0, "", fmt.Errorf("last insert id: %w", err)
	}

	return int(noteID), absPath, nil
}

// IndexNote performs background semantic indexing of a note.
// It summarizes the note, picks tags, finds related notes, updates the vault
// file, and inserts the note into the tree index.
func (kb *KnowledgeBase) IndexNote(noteID int, app *App) {
	defer app.Indexing.remove(noteID)

	note, err := kb.getNote(noteID)
	if err != nil {
		slog.Error("index: failed to get note", "noteID", noteID, "error", err)
		return
	}

	rawContent, err := kb.ReadNoteContent(note.path)
	if err != nil {
		slog.Error("index: failed to read note content", "noteID", noteID, "error", err)
		return
	}
	content := stripFrontmatter(rawContent)

	// Step 1: Summarize
	summary, err := callHaiku(fmt.Sprintf(
		"Summarize the following note in one concise sentence.\n\nTitle: %s\n\nContent:\n%s\n\nReply with ONLY the summary sentence, nothing else.",
		note.title, content,
	))
	if err != nil {
		slog.Error("index: summarize failed", "noteID", noteID, "error", err)
		return
	}
	summary = strings.TrimSpace(summary)

	if _, err := kb.db.Exec(`UPDATE notes SET summary = ? WHERE id = ?`, summary, noteID); err != nil {
		slog.Error("index: update summary failed", "noteID", noteID, "error", err)
		return
	}

	// Step 2: Pick tags
	rootLabels, err := kb.allRootLabels()
	if err != nil {
		slog.Error("index: failed to get root labels", "error", err)
		return
	}

	var existingSection string
	if len(rootLabels) > 0 {
		existingSection = fmt.Sprintf("Existing categories in the knowledge base:\n%s\n\n", strings.Join(rootLabels, ", "))
	}

	tagsRaw, err := callHaiku(fmt.Sprintf(
		`Pick categories/tags for the following note. Prefer reusing existing categories when they fit. Only create a new category if none of the existing ones apply. Keep categories as single lowercase words or short hyphenated phrases.

%sNote summary: %s

Title: %s

Reply with ONLY a comma-separated list of tags, nothing else.`,
		existingSection, summary, note.title,
	))
	if err != nil {
		slog.Error("index: tag picking failed", "noteID", noteID, "error", err)
		return
	}

	var tags []string
	for _, t := range strings.Split(tagsRaw, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}

	// Step 3: Find related notes
	recentNotes, err := kb.recentNoteSummaries(noteID, 20)
	if err != nil {
		slog.Warn("index: failed to get recent notes for linking", "error", err)
	}

	var relatedTitles []string
	if len(recentNotes) > 0 {
		var notesList strings.Builder
		for _, rn := range recentNotes {
			notesList.WriteString(fmt.Sprintf("- %s: %s\n", rn.title, rn.summary))
		}

		relatedRaw, err := callHaiku(fmt.Sprintf(
			`Given this note and a list of other notes, identify which notes are related.

Note: %s — %s

Other notes:
%s
Reply with ONLY a comma-separated list of related note titles (exactly as shown above), or "none" if no notes are related. Nothing else.`,
			note.title, summary, notesList.String(),
		))
		if err != nil {
			slog.Warn("index: find related failed", "error", err)
		} else {
			relatedRaw = strings.TrimSpace(relatedRaw)
			if relatedRaw != "none" && relatedRaw != "" {
				for _, t := range strings.Split(relatedRaw, ",") {
					t = strings.TrimSpace(t)
					if t != "" {
						relatedTitles = append(relatedTitles, t)
					}
				}
			}
		}
	}

	// Step 4: Update vault file with tags and related links
	kb.rewriteVaultFile(note, content, tags, relatedTitles)

	// Update tags in DB
	tagStr := strings.Join(tags, ",")
	if _, err := kb.db.Exec(`UPDATE notes SET tags = ? WHERE id = ?`, tagStr, noteID); err != nil {
		slog.Error("index: update tags failed", "noteID", noteID, "error", err)
	}

	// Step 5: Tree insertion (fan out per tag)
	var wg sync.WaitGroup
	for _, tag := range tags {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			if err := kb.insertIntoTree(noteID, tag, summary); err != nil {
				slog.Error("index: tree insertion failed", "noteID", noteID, "tag", tag, "error", err)
			}
		}(tag)
	}
	wg.Wait()

	// Step 6: Mark as indexed
	if _, err := kb.db.Exec(`UPDATE notes SET indexed = 1 WHERE id = ?`, noteID); err != nil {
		slog.Error("index: mark indexed failed", "noteID", noteID, "error", err)
	}

	slog.Info("index: note indexed", "noteID", noteID, "title", note.title, "tags", tags)
}

// TouchNote updates the last_verified timestamp for a note in both the DB
// and the vault file. Intended to be run as a goroutine.
func (kb *KnowledgeBase) TouchNote(noteID int, app *App) {
	defer app.Indexing.remove(noteID)

	now := time.Now().Format("2006-01-02")

	if _, err := kb.db.Exec(`UPDATE notes SET last_verified = ? WHERE id = ?`, now, noteID); err != nil {
		slog.Error("touch: update db failed", "noteID", noteID, "error", err)
		return
	}

	note, err := kb.getNote(noteID)
	if err != nil {
		slog.Error("touch: get note failed", "noteID", noteID, "error", err)
		return
	}

	absPath := filepath.Join(kb.vaultDir, note.path)
	data, err := os.ReadFile(absPath)
	if err != nil {
		slog.Error("touch: read vault file failed", "path", absPath, "error", err)
		return
	}

	content := string(data)
	if strings.Contains(content, "last_verified:") {
		// Replace existing last_verified line
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "last_verified:") {
				lines[i] = "last_verified: " + now
				break
			}
		}
		content = strings.Join(lines, "\n")
	} else if strings.Contains(content, "created:") {
		// Insert after created: line
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "created:") {
				rest := append([]string{"last_verified: " + now}, lines[i+1:]...)
				lines = append(lines[:i+1], rest...)
				break
			}
		}
		content = strings.Join(lines, "\n")
	}

	if err := os.WriteFile(absPath, []byte(content), 0600); err != nil {
		slog.Error("touch: write vault file failed", "path", absPath, "error", err)
		return
	}

	slog.Info("touch: note verified", "noteID", noteID, "title", note.title)
}

// Query performs a tree traversal to find relevant notes for a query.
func (kb *KnowledgeBase) Query(query string) ([]QueryResult, error) {
	roots, err := kb.getRootNodes()
	if err != nil {
		return nil, fmt.Errorf("get roots: %w", err)
	}

	if len(roots) == 0 {
		return nil, nil
	}

	// Ask haiku which roots are relevant
	var rootList strings.Builder
	for _, r := range roots {
		rootList.WriteString(fmt.Sprintf("- %s: %s\n", r.label, r.summary))
	}

	selectedRaw, err := callHaiku(fmt.Sprintf(
		`Given a search query and a list of categories, select ALL categories that could contain relevant results.

Query: %s

Categories:
%s
Reply with ONLY a comma-separated list of selected category names (exactly as shown above), or "none" if no categories are relevant. Nothing else.`,
		query, rootList.String(),
	))
	if err != nil {
		return nil, fmt.Errorf("select roots: %w", err)
	}

	selectedRaw = strings.TrimSpace(selectedRaw)
	if selectedRaw == "none" || selectedRaw == "" {
		return nil, nil
	}

	selectedRoots := parseCSV(selectedRaw)
	rootMap := make(map[string]*treeNode)
	for _, r := range roots {
		rootMap[r.label] = r
	}

	// Fan out across selected roots
	var mu sync.Mutex
	noteIDs := make(map[int]struct{})
	var wg sync.WaitGroup

	for _, name := range selectedRoots {
		node, ok := rootMap[name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(node *treeNode) {
			defer wg.Done()
			ids, err := kb.descendTree(node.id, query)
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

	// Fetch note details
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

// Close closes the underlying database connection.
func (kb *KnowledgeBase) Close() error {
	return kb.db.Close()
}

// BackfillSummaries updates summaries for all leaf nodes whose summary still
// equals their label (i.e., never been updated), then propagates up to ancestors.
func (kb *KnowledgeBase) BackfillSummaries() {
	rows, err := kb.db.Query(`SELECT id FROM tree_nodes WHERE is_leaf = 1 AND summary = label`)
	if err != nil {
		slog.Error("backfill: query stale leaves", "error", err)
		return
	}
	defer rows.Close()

	var leafIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			slog.Error("backfill: scan leaf id", "error", err)
			continue
		}
		leafIDs = append(leafIDs, id)
	}
	if err := rows.Err(); err != nil {
		slog.Error("backfill: iterate leaves", "error", err)
		return
	}

	if len(leafIDs) == 0 {
		return
	}

	slog.Info("backfill: updating stale leaf summaries", "count", len(leafIDs))

	for _, id := range leafIDs {
		if err := kb.updateLeafSummary(id); err != nil {
			slog.Warn("backfill: update leaf summary failed", "nodeID", id, "error", err)
			continue
		}
		if err := kb.propagateSummaryUp(id); err != nil {
			slog.Warn("backfill: propagate summary failed", "nodeID", id, "error", err)
		}
	}

	slog.Info("backfill: done")
}

// QueryResultsJSON marshals query results to JSON text suitable for MCP responses.
func QueryResultsJSON(results []QueryResult) string {
	if results == nil {
		results = []QueryResult{}
	}
	out, _ := json.Marshal(results)
	return string(out)
}

// ReadNoteContent reads a vault markdown file by its relative path and returns
// the raw markdown content.
func (kb *KnowledgeBase) ReadNoteContent(relPath string) (string, error) {
	absPath := filepath.Join(kb.vaultDir, relPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read vault file: %w", err)
	}
	return string(data), nil
}

// stripFrontmatter removes YAML frontmatter delimited by "---" lines from markdown.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	// Find the closing "---"
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return s
	}
	// Skip past the closing "---\n"
	body := s[3+end+4:]
	return strings.TrimLeft(body, "\n")
}

// --- Internal types ---

type noteRow struct {
	id           int
	path         string
	title        string
	tags         string
	summary      string
	lastVerified string
}

type treeNode struct {
	id       int
	parentID *int
	label    string
	summary  string
	isLeaf   bool
}

// --- Internal helpers ---

func (kb *KnowledgeBase) getNote(id int) (*noteRow, error) {
	var n noteRow
	err := kb.db.QueryRow(
		`SELECT id, path, title, tags, summary, last_verified FROM notes WHERE id = ?`, id,
	).Scan(&n.id, &n.path, &n.title, &n.tags, &n.summary, &n.lastVerified)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (kb *KnowledgeBase) getNoteByPath(relPath string) (*noteRow, error) {
	var n noteRow
	err := kb.db.QueryRow(
		`SELECT id, path, title, tags, summary, last_verified FROM notes WHERE path = ?`, relPath,
	).Scan(&n.id, &n.path, &n.title, &n.tags, &n.summary, &n.lastVerified)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (kb *KnowledgeBase) recentNoteSummaries(excludeID, limit int) ([]noteRow, error) {
	rows, err := kb.db.Query(
		`SELECT id, path, title, tags, summary FROM notes WHERE id != ? AND summary != '' ORDER BY id DESC LIMIT ?`,
		excludeID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []noteRow
	for rows.Next() {
		var n noteRow
		if err := rows.Scan(&n.id, &n.path, &n.title, &n.tags, &n.summary); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
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

// insertIntoTree inserts a note into the tree under a given tag.
func (kb *KnowledgeBase) insertIntoTree(noteID int, tag, noteSummary string) error {
	// Find or create root node for this tag
	rootID, err := kb.findOrCreateRoot(tag)
	if err != nil {
		return fmt.Errorf("find/create root %q: %w", tag, err)
	}

	// Get root node
	var isLeaf bool
	err = kb.db.QueryRow(`SELECT is_leaf FROM tree_nodes WHERE id = ?`, rootID).Scan(&isLeaf)
	if err != nil {
		return fmt.Errorf("get root node: %w", err)
	}

	if isLeaf {
		// Insert directly into this leaf
		return kb.insertIntoLeaf(rootID, noteID)
	}

	// Descend to find all relevant leaves
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

	// Create new root leaf node
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

	// Check if we need to split
	size, err := kb.leafSize(nodeID)
	if err != nil {
		return err
	}
	if size > maxLeafSize {
		if err := kb.splitLeaf(nodeID); err != nil {
			return err
		}
		// After split, nodeID is now internal — propagate its summary up
		if err := kb.propagateSummaryUp(nodeID); err != nil {
			slog.Warn("index: propagate summary after split failed", "nodeID", nodeID, "error", err)
		}
		return nil
	}

	// No split — update the leaf summary and propagate up
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

	summary, err := callHaiku(fmt.Sprintf(
		`This category is named %q. Describe what it covers based on its name and the following notes. The summary should reflect the broad scope implied by the category name, not just the current notes. Reply with ONLY a single concise sentence, nothing else.

Notes:
%s`, label, notesList.String(),
	))
	if err != nil {
		return fmt.Errorf("haiku leaf summary: %w", err)
	}

	summary = strings.TrimSpace(summary)
	if _, err := kb.db.Exec(`UPDATE tree_nodes SET summary = ? WHERE id = ?`, summary, nodeID); err != nil {
		return fmt.Errorf("update leaf summary: %w", err)
	}

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
			// Reached the root — update it too if it's internal
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

		// Update the parent's summary based on its children
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

	summary, err := callHaiku(fmt.Sprintf(
		`This category is named %q. Describe what it covers based on its name and the following subcategories. The summary should reflect the broad scope implied by the category name, not just the current subcategories. Reply with ONLY a single concise sentence, nothing else.

Subcategories:
%s`, label, childList.String(),
	))
	if err != nil {
		return fmt.Errorf("haiku internal summary: %w", err)
	}

	summary = strings.TrimSpace(summary)
	if _, err := kb.db.Exec(`UPDATE tree_nodes SET summary = ? WHERE id = ?`, summary, nodeID); err != nil {
		return fmt.Errorf("update internal summary: %w", err)
	}

	return nil
}

func (kb *KnowledgeBase) descendAndInsert(nodeID, noteID int, noteSummary string) error {
	children, err := kb.getNodeChildren(nodeID)
	if err != nil {
		return err
	}

	if len(children) == 0 {
		// Shouldn't happen for an internal node, but handle gracefully
		return kb.insertIntoLeaf(nodeID, noteID)
	}

	// Ask haiku which children are relevant
	var childList strings.Builder
	for _, c := range children {
		childList.WriteString(fmt.Sprintf("- %s: %s\n", c.label, c.summary))
	}

	selectedRaw, err := callHaiku(fmt.Sprintf(
		`Given a note summary and a list of subcategories, select ALL subcategories where this note belongs.

Note: %s

Subcategories:
%s
Reply with ONLY a comma-separated list of selected subcategory names (exactly as shown above). If none fit well, reply with the single best match. Nothing else.`,
		noteSummary, childList.String(),
	))
	if err != nil {
		// Fallback: insert into the first child
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

	// Build a list of note summaries for haiku to cluster
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
		return nil // nothing to split
	}

	var notesList strings.Builder
	for i, n := range notes {
		notesList.WriteString(fmt.Sprintf("%d. %s: %s\n", i, n.title, n.summary))
	}

	// Ask haiku to partition into clusters
	clusterJSON, err := callHaiku(fmt.Sprintf(
		`Partition the following notes into 2-4 thematic clusters. A note can appear in multiple clusters if relevant.

Notes:
%s
Reply with ONLY valid JSON in this exact format, nothing else:
[{"label": "cluster-name", "summary": "one sentence description", "notes": [0, 2, 5]}]

Where the numbers are the note indices from the list above.`,
		notesList.String(),
	))
	if err != nil {
		slog.Error("split: haiku clustering failed", "nodeID", nodeID, "error", err)
		return nil // non-fatal: leave the oversized leaf as-is
	}

	// Parse the JSON response
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

	// Convert parent from leaf to internal
	if _, err := kb.db.Exec(`UPDATE tree_nodes SET is_leaf = 0 WHERE id = ?`, nodeID); err != nil {
		return fmt.Errorf("update parent: %w", err)
	}

	// Remove old leaf entries
	if _, err := kb.db.Exec(`DELETE FROM leaf_entries WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("delete old entries: %w", err)
	}

	// Create child leaves
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
	}

	// Check if parent now has too many children
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

	groupJSON, err := callHaiku(fmt.Sprintf(
		`Group the following subcategories into 2-4 meta-groups.

Subcategories:
%s
Reply with ONLY valid JSON in this exact format, nothing else:
[{"label": "group-name", "summary": "one sentence description", "children": [0, 2, 5]}]

Where the numbers are the subcategory indices from the list above.`,
		childList.String(),
	))
	if err != nil {
		slog.Error("split: haiku node grouping failed", "nodeID", nodeID, "error", err)
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

	// Create new internal nodes and reparent children
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
	}

	slog.Info("split: node split", "nodeID", nodeID, "groups", len(groups))
	return nil
}

// descendTree recursively traverses the tree to collect note IDs matching a query.
func (kb *KnowledgeBase) descendTree(nodeID int, query string) ([]int, error) {
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

	// Ask haiku which children are relevant
	var childList strings.Builder
	for _, c := range children {
		childList.WriteString(fmt.Sprintf("- %s: %s\n", c.label, c.summary))
	}

	selectedRaw, err := callHaiku(fmt.Sprintf(
		`Given a search query and a list of subcategories, select ALL subcategories that could contain relevant results.

Query: %s

Subcategories:
%s
Reply with ONLY a comma-separated list of selected subcategory names (exactly as shown above), or "none" if none are relevant. Nothing else.`,
		query, childList.String(),
	))
	if err != nil {
		// On failure, search all children
		slog.Warn("query: descent selection failed, searching all", "error", err)
		selectedRaw = ""
		for _, c := range children {
			if selectedRaw != "" {
				selectedRaw += ", "
			}
			selectedRaw += c.label
		}
	}

	selected := parseCSV(selectedRaw)
	if len(selected) == 0 || (len(selected) == 1 && selected[0] == "none") {
		return nil, nil
	}

	childMap := make(map[string]*treeNode)
	for _, c := range children {
		childMap[c.label] = c
	}

	var mu sync.Mutex
	var allIDs []int
	var wg sync.WaitGroup

	for _, name := range selected {
		child, ok := childMap[name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(child *treeNode) {
			defer wg.Done()
			ids, err := kb.descendTree(child.id, query)
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

func (kb *KnowledgeBase) rewriteVaultFile(note *noteRow, content string, tags, relatedTitles []string) {
	absPath := filepath.Join(kb.vaultDir, note.path)

	tagsYAML := "[]"
	if len(tags) > 0 {
		quoted := make([]string, len(tags))
		for i, t := range tags {
			quoted[i] = fmt.Sprintf("%q", t)
		}
		tagsYAML = "[" + strings.Join(quoted, ", ") + "]"
	}

	now := time.Now().Format("2006-01-02")
	lastVerified := note.lastVerified
	if lastVerified == "" {
		lastVerified = now
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("---\ntitle: %q\ntags: %s\ncreated: %s\nlast_verified: %s\n---\n\n%s\n", note.title, tagsYAML, now, lastVerified, content))

	if len(relatedTitles) > 0 {
		sb.WriteString("\n## Related\n\n")
		for _, t := range relatedTitles {
			sb.WriteString(fmt.Sprintf("- [[%s]]\n", t))
		}
	}

	if err := os.WriteFile(absPath, []byte(sb.String()), 0600); err != nil {
		slog.Error("index: failed to rewrite vault file", "path", absPath, "error", err)
	}
}

// --- Utility functions ---

func callHaiku(prompt string) (string, error) {
	cmd := exec.Command("claude", "-p", "--model", "haiku", prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p: %w (stderr: %s)", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

func parseCSV(s string) []string {
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".local", "state", "vee")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

func migrate(db *sql.DB) error {
	// Core notes table
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notes (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			path       TEXT UNIQUE NOT NULL,
			title      TEXT NOT NULL,
			tags       TEXT NOT NULL DEFAULT '',
			summary    TEXT NOT NULL DEFAULT '',
			indexed    INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tree_nodes (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			parent_id INTEGER REFERENCES tree_nodes(id),
			label     TEXT NOT NULL,
			summary   TEXT NOT NULL,
			is_leaf   INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS leaf_entries (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES tree_nodes(id) ON DELETE CASCADE,
			note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
			UNIQUE(node_id, note_id)
		)`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", s[:40], err)
		}
	}

	// Migration: add columns if missing (for existing DBs)
	alterStmts := []string{
		`ALTER TABLE notes ADD COLUMN summary TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE notes ADD COLUMN indexed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE notes ADD COLUMN last_verified TEXT NOT NULL DEFAULT ''`,
	}
	for _, s := range alterStmts {
		db.Exec(s) // ignore errors (column already exists)
	}

	// Backfill: set last_verified to created_at for existing notes
	db.Exec(`UPDATE notes SET last_verified = created_at WHERE last_verified = ''`)

	// Drop FTS5 table and triggers if they exist (migrating from old schema)
	dropStmts := []string{
		`DROP TRIGGER IF EXISTS notes_ai`,
		`DROP TRIGGER IF EXISTS notes_ad`,
		`DROP TRIGGER IF EXISTS notes_au`,
		`DROP TABLE IF EXISTS notes_fts`,
	}
	for _, s := range dropStmts {
		db.Exec(s) // ignore errors
	}

	return nil
}

// sanitizeFilename turns a title into a safe filename (no path separators, etc).
func sanitizeFilename(title string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	name := replacer.Replace(title)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "untitled"
	}
	return name
}
