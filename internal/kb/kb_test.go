package kb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubModel implements Model for testing. It returns responses in order,
// cycling when exhausted, and records all prompts received.
type stubModel struct {
	mu        sync.Mutex
	responses []string
	errors    []error // if non-nil and same index, return error instead
	calls     []string
	idx       int

	// Embedding support
	embedFn func(texts []string) ([][]float64, error)
}

func newStub(responses ...string) *stubModel {
	return &stubModel{
		responses: responses,
		embedFn:   func(texts []string) ([][]float64, error) { return nil, fmt.Errorf("embed not configured") },
	}
}

func (s *stubModel) Generate(prompt string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, prompt)
	if len(s.responses) == 0 {
		return "", fmt.Errorf("stub: no responses configured")
	}
	i := s.idx % len(s.responses)
	s.idx++
	if s.errors != nil && i < len(s.errors) && s.errors[i] != nil {
		return "", s.errors[i]
	}
	return s.responses[i], nil
}

func (s *stubModel) Embed(texts []string) ([][]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.embedFn != nil {
		return s.embedFn(texts)
	}
	return nil, fmt.Errorf("embed not configured")
}

func (s *stubModel) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// openTestKB creates a temporary KB and returns it along with a cleanup function.
func openTestKB(t *testing.T, stub *stubModel) *KnowledgeBase {
	t.Helper()
	dir := t.TempDir()
	kbase, err := Open(Config{
		DBPath:   filepath.Join(dir, "kb.db"),
		VaultDir: filepath.Join(dir, "vault"),
		Model:    stub,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { kbase.Close() })
	return kbase
}

// --- Schema & migration tests ---

func TestOpen_CreatesDBAndVault(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "kb.db")
	vaultDir := filepath.Join(dir, "sub", "vault")

	kbase, err := Open(Config{
		DBPath:   dbPath,
		VaultDir: vaultDir,
		Model:    newStub("ok"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer kbase.Close()

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("DB file not created")
	}
	if info, err := os.Stat(vaultDir); os.IsNotExist(err) || !info.IsDir() {
		t.Error("vault dir not created")
	}
}

func TestOpen_MigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kb.db")
	vaultDir := filepath.Join(dir, "vault")
	cfg := Config{DBPath: dbPath, VaultDir: vaultDir, Model: newStub("ok")}

	kb1, err := Open(cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	kb1.Close()

	kb2, err := Open(cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	kb2.Close()
}

func TestOpen_NilModel(t *testing.T) {
	dir := t.TempDir()
	_, err := Open(Config{
		DBPath:   filepath.Join(dir, "kb.db"),
		VaultDir: filepath.Join(dir, "vault"),
		Model:    nil,
	})
	if err == nil {
		t.Fatal("expected error for nil model")
	}
}

// --- Notes CRUD tests ---

func TestAddNote_CreatesFileAndRow(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	noteID, relPath, err := kbase.AddNote("Test Note", "Some content", []string{"file.go"})
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}
	if noteID <= 0 {
		t.Errorf("expected positive noteID, got %d", noteID)
	}
	if relPath == "" {
		t.Error("expected non-empty relPath")
	}

	// Verify vault file exists
	absPath := filepath.Join(kbase.vaultDir, relPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("read vault file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, `title: "Test Note"`) {
		t.Error("vault file missing title in frontmatter")
	}
	if !strings.Contains(content, "Some content") {
		t.Error("vault file missing content")
	}
	if !strings.Contains(content, `"file.go"`) {
		t.Error("vault file missing source")
	}

	// Verify DB row
	var title string
	err = kbase.db.QueryRow(`SELECT title FROM notes WHERE id = ?`, noteID).Scan(&title)
	if err != nil {
		t.Fatalf("query DB: %v", err)
	}
	if title != "Test Note" {
		t.Errorf("expected title 'Test Note', got %q", title)
	}
}

func TestAddNote_DuplicateTitle(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	id1, _, err := kbase.AddNote("Dup", "content 1", nil)
	if err != nil {
		t.Fatalf("first AddNote: %v", err)
	}

	id2, _, err := kbase.AddNote("Dup", "content 2", nil)
	if err != nil {
		t.Fatalf("second AddNote: %v", err)
	}

	// INSERT OR REPLACE should produce a new ID (the old row is replaced)
	if id1 == id2 {
		// Could be the same row replaced, that's acceptable too
		// Just verify only one row exists
		var count int
		kbase.db.QueryRow(`SELECT COUNT(*) FROM notes WHERE title = 'Dup'`).Scan(&count)
		if count != 1 {
			t.Errorf("expected 1 row, got %d", count)
		}
	}

	// Verify file has updated content
	content, err := kbase.FetchNote("Dup.md")
	if err != nil {
		t.Fatalf("FetchNote: %v", err)
	}
	if !strings.Contains(content, "content 2") {
		t.Error("expected updated content")
	}
}

func TestFetchNote_ReturnsContent(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	_, relPath, err := kbase.AddNote("Fetch Me", "Hello world", nil)
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	content, err := kbase.FetchNote(relPath)
	if err != nil {
		t.Fatalf("FetchNote: %v", err)
	}
	if !strings.Contains(content, "Hello world") {
		t.Error("FetchNote missing content")
	}
}

func TestFetchNote_NotFound(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	_, err := kbase.FetchNote("nonexistent.md")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestGetNoteByPath(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	noteID, relPath, err := kbase.AddNote("Path Test", "content", []string{"src"})
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	info, err := kbase.GetNoteByPath(relPath)
	if err != nil {
		t.Fatalf("GetNoteByPath: %v", err)
	}
	if info.ID != noteID {
		t.Errorf("expected ID %d, got %d", noteID, info.ID)
	}
	if info.Path != relPath {
		t.Errorf("expected path %q, got %q", relPath, info.Path)
	}
	if info.Title != "Path Test" {
		t.Errorf("expected title 'Path Test', got %q", info.Title)
	}
}

func TestTouchNote_UpdatesTimestamp(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	noteID, relPath, err := kbase.AddNote("Touch Test", "body", nil)
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	err = kbase.TouchNote(noteID)
	if err != nil {
		t.Fatalf("TouchNote: %v", err)
	}

	// Verify DB updated
	var lv string
	kbase.db.QueryRow(`SELECT last_verified FROM notes WHERE id = ?`, noteID).Scan(&lv)
	if lv == "" {
		t.Error("last_verified not updated in DB")
	}

	// Verify vault file updated
	content, err := kbase.FetchNote(relPath)
	if err != nil {
		t.Fatalf("FetchNote: %v", err)
	}
	if !strings.Contains(content, "last_verified: "+lv) {
		t.Error("last_verified not updated in vault file")
	}
}

// --- Indexing tests ---

func TestIndexNote_SummarizesAndTags(t *testing.T) {
	// Responses: 1=summarize, 2=tag picking, 3=find related (none), 4=tree leaf summary, 5=propagate (root internal) ...
	stub := newStub(
		"A concise summary of the note.",                           // summarize
		"golang, testing",                                          // tags
		"none",                                                     // related notes
		"Covers Go programming topics.",                            // leaf summary for "golang"
		"Covers testing-related topics.",                            // leaf summary for "testing"
		"Covers Go programming topics.",                            // propagate (root is leaf, so updateLeafSummary may be called)
		"Covers testing-related topics.",                            // propagate
	)
	kbase := openTestKB(t, stub)

	noteID, _, err := kbase.AddNote("Go Testing Guide", "How to test in Go", []string{"docs"})
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	err = kbase.IndexNote(noteID)
	if err != nil {
		t.Fatalf("IndexNote: %v", err)
	}

	// Verify summary updated in DB
	var summary string
	kbase.db.QueryRow(`SELECT summary FROM notes WHERE id = ?`, noteID).Scan(&summary)
	if summary != "A concise summary of the note." {
		t.Errorf("unexpected summary: %q", summary)
	}

	// Verify tags updated in DB
	var tags string
	kbase.db.QueryRow(`SELECT tags FROM notes WHERE id = ?`, noteID).Scan(&tags)
	if !strings.Contains(tags, "golang") || !strings.Contains(tags, "testing") {
		t.Errorf("unexpected tags: %q", tags)
	}

	// Verify indexed flag
	var indexed int
	kbase.db.QueryRow(`SELECT indexed FROM notes WHERE id = ?`, noteID).Scan(&indexed)
	if indexed != 1 {
		t.Error("note not marked as indexed")
	}
}

func TestIndexNote_CreatesTreeNodes(t *testing.T) {
	stub := newStub(
		"Summary of test note.",
		"architecture",
		"none",
		"Covers software architecture topics.",
	)
	kbase := openTestKB(t, stub)

	noteID, _, err := kbase.AddNote("Arch Overview", "Architecture overview", nil)
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	err = kbase.IndexNote(noteID)
	if err != nil {
		t.Fatalf("IndexNote: %v", err)
	}

	// Verify tree_nodes exist
	var nodeCount int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM tree_nodes`).Scan(&nodeCount)
	if nodeCount == 0 {
		t.Error("no tree nodes created")
	}

	// Verify leaf_entries exist
	var entryCount int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM leaf_entries`).Scan(&entryCount)
	if entryCount == 0 {
		t.Error("no leaf entries created")
	}
}

func TestIndexNote_ModelFailure(t *testing.T) {
	stub := &stubModel{
		responses: []string{""},
		errors:    []error{fmt.Errorf("model unavailable")},
	}
	kbase := openTestKB(t, stub)

	noteID, _, err := kbase.AddNote("Fail Note", "content", nil)
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	err = kbase.IndexNote(noteID)
	if err == nil {
		t.Fatal("expected error from IndexNote when model fails")
	}

	// Verify note is NOT marked as indexed
	var indexed int
	kbase.db.QueryRow(`SELECT indexed FROM notes WHERE id = ?`, noteID).Scan(&indexed)
	if indexed != 0 {
		t.Error("note should not be marked indexed after model failure")
	}
}

// --- Tree operation tests ---

func TestInsertIntoTree_CreatesRoot(t *testing.T) {
	stub := newStub("Covers Go-related topics.") // for updateLeafSummary
	kbase := openTestKB(t, stub)

	// Insert a note manually so the tree can reference it
	noteID, _, err := kbase.AddNote("Root Test", "content", nil)
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}
	// Set a summary so the tree insertion works
	kbase.db.Exec(`UPDATE notes SET summary = 'test summary' WHERE id = ?`, noteID)

	err = kbase.insertIntoTree(noteID, "golang", "test summary")
	if err != nil {
		t.Fatalf("insertIntoTree: %v", err)
	}

	// Verify root node created
	var label string
	err = kbase.db.QueryRow(`SELECT label FROM tree_nodes WHERE parent_id IS NULL AND label = 'golang'`).Scan(&label)
	if err != nil {
		t.Fatalf("root node not found: %v", err)
	}
	if label != "golang" {
		t.Errorf("expected label 'golang', got %q", label)
	}
}

func TestInsertIntoTree_SplitsLeaf(t *testing.T) {
	// Build the exact response sequence:
	// - Inserts 1-20: each calls updateLeafSummary (1 Generate) + propagateSummaryUp (root is leaf, no call) = 20 calls
	// - Insert 21: triggers split → splitLeaf calls Generate for clustering JSON (1 call)
	//   then propagateSummaryUp on now-internal root → updateInternalSummary (1 call)
	// - Insert 22: descendAndInsert calls Generate to pick child (1 call)
	//   then insertIntoLeaf calls updateLeafSummary (1 call)
	//   then propagateSummaryUp → updateInternalSummary for root (1 call)
	var responses []string
	// 20 updateLeafSummary calls
	for i := 0; i < 20; i++ {
		responses = append(responses, "Covers various topics.")
	}
	// splitLeaf clustering JSON
	responses = append(responses, `[{"label":"group-a","summary":"First group","notes":[0,1,2,3,4,5,6,7,8,9]},{"label":"group-b","summary":"Second group","notes":[10,11,12,13,14,15,16,17,18,19,20]}]`)
	// updateInternalSummary for root after split
	responses = append(responses, "Updated internal summary.")
	// descendAndInsert child selection for insert 22
	responses = append(responses, "group-a")
	// updateLeafSummary for child
	responses = append(responses, "Updated child summary.")
	// updateInternalSummary for root during propagate
	responses = append(responses, "Updated root summary.")

	stub := newStub(responses...)
	kbase := openTestKB(t, stub)

	// Insert 22 notes under the same tag to trigger a split
	for i := 0; i < 22; i++ {
		title := fmt.Sprintf("Note %d", i)
		noteID, _, err := kbase.AddNote(title, fmt.Sprintf("content %d", i), nil)
		if err != nil {
			t.Fatalf("AddNote %d: %v", i, err)
		}
		kbase.db.Exec(`UPDATE notes SET summary = ? WHERE id = ?`, fmt.Sprintf("Summary of note %d", i), noteID)
		err = kbase.insertIntoTree(noteID, "split-test", fmt.Sprintf("Summary of note %d", i))
		if err != nil {
			t.Fatalf("insertIntoTree %d: %v", i, err)
		}
	}

	// Verify the root node is no longer a leaf (it got split)
	var isLeaf int
	err := kbase.db.QueryRow(`SELECT is_leaf FROM tree_nodes WHERE parent_id IS NULL AND label = 'split-test'`).Scan(&isLeaf)
	if err != nil {
		t.Fatalf("query root: %v", err)
	}
	if isLeaf != 0 {
		t.Error("expected root to become internal after split")
	}

	// Verify children exist
	var childCount int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM tree_nodes WHERE parent_id = (SELECT id FROM tree_nodes WHERE parent_id IS NULL AND label = 'split-test')`).Scan(&childCount)
	if childCount < 2 {
		t.Errorf("expected at least 2 children after split, got %d", childCount)
	}
}

func TestInsertIntoTree_MultiTag(t *testing.T) {
	stub := newStub("Covers topics.") // summary responses
	kbase := openTestKB(t, stub)

	noteID, _, err := kbase.AddNote("Multi Tag", "content", nil)
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}
	kbase.db.Exec(`UPDATE notes SET summary = 'multi tag summary' WHERE id = ?`, noteID)

	// Insert under two different tags
	err = kbase.insertIntoTree(noteID, "tag-a", "multi tag summary")
	if err != nil {
		t.Fatalf("insertIntoTree tag-a: %v", err)
	}
	err = kbase.insertIntoTree(noteID, "tag-b", "multi tag summary")
	if err != nil {
		t.Fatalf("insertIntoTree tag-b: %v", err)
	}

	// Verify note appears under both roots
	var countA, countB int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM leaf_entries le JOIN tree_nodes tn ON tn.id = le.node_id WHERE tn.label = 'tag-a' AND le.note_id = ?`, noteID).Scan(&countA)
	kbase.db.QueryRow(`SELECT COUNT(*) FROM leaf_entries le JOIN tree_nodes tn ON tn.id = le.node_id WHERE tn.label = 'tag-b' AND le.note_id = ?`, noteID).Scan(&countB)

	if countA != 1 {
		t.Errorf("expected 1 entry under tag-a, got %d", countA)
	}
	if countB != 1 {
		t.Errorf("expected 1 entry under tag-b, got %d", countB)
	}
}

func TestPropagateSummaryUp(t *testing.T) {
	stub := newStub(
		"Updated internal summary.",   // for updateInternalSummary
		"Updated root summary.",       // for root update
	)
	kbase := openTestKB(t, stub)

	// Build a small tree manually: root (internal) -> child (leaf)
	res, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'root', 'old root summary', 0)`)
	rootID, _ := res.LastInsertId()

	_, err := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (?, 'child', 'child summary', 1)`, rootID)
	if err != nil {
		t.Fatalf("insert child: %v", err)
	}

	childID := int(rootID) + 1 // approximate

	err = kbase.propagateSummaryUp(childID)
	if err != nil {
		t.Fatalf("propagateSummaryUp: %v", err)
	}

	// Verify root summary was updated
	var summary string
	kbase.db.QueryRow(`SELECT summary FROM tree_nodes WHERE id = ?`, rootID).Scan(&summary)
	if summary == "old root summary" {
		t.Error("root summary should have been updated")
	}
}

// --- Query tests ---

func TestQuery_EmptyDB(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	results, err := kbase.Query("anything")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestQuery_SelectsCorrectRoots(t *testing.T) {
	stub := newStub(
		"Summary for leaf.",           // updateLeafSummary for golang
		"Summary for leaf.",           // updateLeafSummary for python
	)
	// Embedding: query and golang are similar, cooking is dissimilar
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0} // similar to Go topic
		}
		return results, nil
	}

	dir := t.TempDir()
	kbase, err := Open(Config{
		DBPath:         filepath.Join(dir, "kb.db"),
		VaultDir:       filepath.Join(dir, "vault"),
		Model:          stub,
		EmbeddingModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer kbase.Close()

	// Add notes and tree structure
	noteID1, _, _ := kbase.AddNote("Go Basics", "Go content", nil)
	kbase.db.Exec(`UPDATE notes SET summary = 'Go basics summary' WHERE id = ?`, noteID1)
	kbase.insertIntoTree(noteID1, "golang", "Go basics summary")

	noteID2, _, _ := kbase.AddNote("Python Basics", "Python content", nil)
	kbase.db.Exec(`UPDATE notes SET summary = 'Python basics summary' WHERE id = ?`, noteID2)
	kbase.insertIntoTree(noteID2, "python", "Python basics summary")

	// Store distinct embeddings for roots: golang is similar to query, python is not
	var goNodeID, pyNodeID int
	kbase.db.QueryRow(`SELECT id FROM tree_nodes WHERE label = 'golang'`).Scan(&goNodeID)
	kbase.db.QueryRow(`SELECT id FROM tree_nodes WHERE label = 'python'`).Scan(&pyNodeID)
	kbase.storeNodeEmbedding(goNodeID, []float64{0.9, 0.1, 0})  // similar to query
	kbase.storeNodeEmbedding(pyNodeID, []float64{0, 0.1, 0.9})  // dissimilar

	results, err := kbase.Query("how to write Go tests")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}

	// Should contain Go note but not Python
	foundGo := false
	foundPython := false
	for _, r := range results {
		if r.Title == "Go Basics" {
			foundGo = true
		}
		if r.Title == "Python Basics" {
			foundPython = true
		}
	}
	if !foundGo {
		t.Error("expected Go Basics in results")
	}
	if foundPython {
		t.Error("did not expect Python Basics in results")
	}
}

func TestQuery_FallbackOnModelError(t *testing.T) {
	kbase := openTestKB(t, newStub("ok"))

	// Set up a simple tree with one root and one note
	noteID, _, _ := kbase.AddNote("Fallback Note", "content", nil)
	kbase.db.Exec(`UPDATE notes SET summary = 'fallback summary' WHERE id = ?`, noteID)

	// Manually insert tree structure
	res, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'test-root', 'test root summary', 0)`)
	rootID, _ := res.LastInsertId()

	childRes, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (?, 'test-child', 'test child summary', 1)`, rootID)
	childID, _ := childRes.LastInsertId()
	kbase.db.Exec(`INSERT INTO leaf_entries (node_id, note_id) VALUES (?, ?)`, childID, noteID)

	// Set up a failing stub for query: first call succeeds (root selection),
	// second call fails (descent selection inside descendTree)
	failStub := &stubModel{
		responses: []string{"test-root", ""},
		errors:    []error{nil, fmt.Errorf("model error")},
	}
	kbase.model = failStub

	results, err := kbase.Query("test query")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Should still find the note because fallback searches all children
	if len(results) == 0 {
		t.Error("expected results from fallback, got none")
	}
}

// --- BackfillSummaries tests ---

func TestBackfillSummaries_UpdatesStaleLeaves(t *testing.T) {
	stub := newStub("Fresh summary for this category.")
	kbase := openTestKB(t, stub)

	// Create a leaf node with summary == label (stale)
	res, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'stale-tag', 'stale-tag', 1)`)
	nodeID, _ := res.LastInsertId()

	// Add a note in the leaf
	noteID, _, _ := kbase.AddNote("Stale Note", "content", nil)
	kbase.db.Exec(`UPDATE notes SET summary = 'note summary' WHERE id = ?`, noteID)
	kbase.db.Exec(`INSERT INTO leaf_entries (node_id, note_id) VALUES (?, ?)`, nodeID, noteID)

	kbase.BackfillSummaries()

	// Verify summary updated
	var summary string
	kbase.db.QueryRow(`SELECT summary FROM tree_nodes WHERE id = ?`, nodeID).Scan(&summary)
	if summary == "stale-tag" {
		t.Error("summary should have been updated from 'stale-tag'")
	}
}

// --- Utility tests ---

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"hello world", "hello world"},
		{"path/to/file", "path-to-file"},
		{"file:name", "file-name"},
		{"star*quest?", "starquest"},
		{"<tag>", "tag"},
		{`"quoted"`, "quoted"},
		{"pipe|here", "pipehere"},
		{"", "untitled"},
		{"  ", "untitled"},
		{"back\\slash", "back-slash"},
	}
	for _, tt := range tests {
		got := sanitizeFilename(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestStripFrontmatter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "with frontmatter",
			input:    "---\ntitle: test\n---\n\nBody content",
			expected: "Body content",
		},
		{
			name:     "without frontmatter",
			input:    "Just plain text",
			expected: "Just plain text",
		},
		{
			name:     "malformed (no closing)",
			input:    "---\ntitle: test\nno closing fence",
			expected: "---\ntitle: test\nno closing fence",
		},
		{
			name:     "empty after frontmatter",
			input:    "---\ntitle: test\n---\n",
			expected: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripFrontmatter(tt.input)
			if got != tt.expected {
				t.Errorf("stripFrontmatter() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestParseCSV(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"a, b, c", []string{"a", "b", "c"}},
		{"", nil},
		{"  ", nil},
		{"one", []string{"one"}},
		{"a,,b,", []string{"a", "b"}},
		{" spaces , between ", []string{"spaces", "between"}},
	}
	for _, tt := range tests {
		got := parseCSV(tt.input)
		if len(got) != len(tt.expected) {
			t.Errorf("parseCSV(%q) = %v, want %v", tt.input, got, tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("parseCSV(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.expected[i])
			}
		}
	}
}

// --- QueryResultsJSON ---

func TestQueryResultsJSON_Empty(t *testing.T) {
	got := QueryResultsJSON(nil)
	if got != "[]" {
		t.Errorf("QueryResultsJSON(nil) = %q, want %q", got, "[]")
	}
}

func TestQueryResultsJSON_WithResults(t *testing.T) {
	results := []QueryResult{
		{Path: "a.md", Title: "A", Summary: "sum", LastVerified: "2025-01-01"},
	}
	got := QueryResultsJSON(results)
	if !strings.Contains(got, `"path":"a.md"`) {
		t.Errorf("QueryResultsJSON missing path: %s", got)
	}
}

// --- CallModel ---

func TestCallModel_DelegatesToModel(t *testing.T) {
	stub := newStub("model response")
	kbase := openTestKB(t, stub)

	resp, err := kbase.CallModel("test prompt")
	if err != nil {
		t.Fatalf("CallModel: %v", err)
	}
	if resp != "model response" {
		t.Errorf("expected 'model response', got %q", resp)
	}
	if stub.callCount() != 1 {
		t.Errorf("expected 1 call, got %d", stub.callCount())
	}
}

// --- NoteInfo/noteRow via direct DB ---

func TestGetNoteByPath_NotFound(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	_, err := kbase.GetNoteByPath("does-not-exist.md")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}
