package kb

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubModel implements Model for testing.
type stubModel struct {
	mu      sync.Mutex
	embedFn func(texts []string) ([][]float64, error)
}

func newStub() *stubModel {
	return &stubModel{
		embedFn: func(texts []string) ([][]float64, error) {
			results := make([][]float64, len(texts))
			for i := range texts {
				results[i] = []float64{0.5, 0.5, 0}
			}
			return results, nil
		},
	}
}

func (s *stubModel) Embed(texts []string) ([][]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.embedFn != nil {
		return s.embedFn(texts)
	}
	return nil, fmt.Errorf("embed not configured")
}

// openTestKB creates a temporary KB for testing.
func openTestKB(t *testing.T, stub *stubModel) *KnowledgeBase {
	t.Helper()
	dir := t.TempDir()
	kbase, err := Open(Config{
		DBPath:         filepath.Join(dir, "kb.db"),
		Model:          stub,
		EmbeddingModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { kbase.Close() })
	return kbase
}

// addAndPromote is a test helper that adds a statement and manually promotes it
// with an embedding, simulating what the worker would do.
func addAndPromote(t *testing.T, kbase *KnowledgeBase, content, source, sourceType string, emb []float64) string {
	t.Helper()
	result, err := kbase.AddStatement(content, source, sourceType, "user", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}
	if emb != nil {
		blob := embeddingToBlob(emb)
		_, err = kbase.db.Exec(
			`UPDATE statements SET embedding = ?, model = ?, status = 'active' WHERE id = ?`,
			blob, kbase.embeddingModel, result.ID,
		)
		if err != nil {
			t.Fatalf("manual promote: %v", err)
		}
	}
	return result.ID
}

// --- Schema & migration tests ---

func TestOpen_CreatesDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kb.db")

	kbase, err := Open(Config{
		DBPath: dbPath,
		Model:  newStub(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer kbase.Close()
}

func TestOpen_MigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kb.db")
	cfg := Config{DBPath: dbPath, Model: newStub()}

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
		DBPath: filepath.Join(dir, "kb.db"),
		Model:  nil,
	})
	if err == nil {
		t.Fatal("expected error for nil model")
	}
}

// --- Statement CRUD tests ---

func TestAddStatement_CreatesRow(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Test Statement. Some content", "file.go", "manual", "", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}

	var content, status string
	err = kbase.db.QueryRow(`SELECT content, status FROM statements WHERE id = ?`, result.ID).Scan(&content, &status)
	if err != nil {
		t.Fatalf("query DB: %v", err)
	}
	if content != "Test Statement. Some content" {
		t.Errorf("expected content 'Test Statement. Some content', got %q", content)
	}
	if status != "pending" {
		t.Errorf("expected status 'pending', got %q", status)
	}
}

func TestAddStatement_NoEmbeddingAtInsert(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		return [][]float64{{0.1, 0.2, 0.3}}, nil
	}
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Embedded content", "src", "", "", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}

	var embBlob []byte
	err = kbase.db.QueryRow(`SELECT embedding FROM statements WHERE id = ?`, result.ID).Scan(&embBlob)
	if err != nil {
		t.Fatalf("query embedding: %v", err)
	}
	// No embedding at insert time — worker does it async
	if embBlob != nil {
		t.Error("expected NULL embedding at insert time")
	}
}

func TestAddStatement_DefaultSourceType(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Default ST content", "src", "", "", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}

	var sourceType string
	kbase.db.QueryRow(`SELECT source_type FROM statements WHERE id = ?`, result.ID).Scan(&sourceType)
	if sourceType != "manual" {
		t.Errorf("expected source_type 'manual', got %q", sourceType)
	}
}

func TestGetStatement(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Get Test. Content here", "file.go", "manual", "", "")

	s, err := kbase.GetStatement(result.ID)
	if err != nil {
		t.Fatalf("GetStatement: %v", err)
	}
	if s.Content != "Get Test. Content here" {
		t.Errorf("expected content 'Get Test. Content here', got %q", s.Content)
	}
	if s.Source != "file.go" {
		t.Errorf("expected source 'file.go', got %q", s.Source)
	}
	if s.Status != "pending" {
		t.Errorf("expected status 'pending', got %q", s.Status)
	}
}

func TestGetStatement_NotFound(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	_, err := kbase.GetStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
	}
}

func TestTouchStatement(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Touch Test body", "src", "manual", "", "")

	err := kbase.TouchStatement(result.ID)
	if err != nil {
		t.Fatalf("TouchStatement: %v", err)
	}

	var lv string
	kbase.db.QueryRow(`SELECT last_verified FROM statements WHERE id = ?`, result.ID).Scan(&lv)
	if lv == "" {
		t.Error("last_verified not updated")
	}
}

func TestTouchStatement_NotFound(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	err := kbase.TouchStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
	}
}

// --- Promote & Delete tests ---

func TestPromoteStatement(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Promote me", "src", "manual", "", "")

	var status string
	kbase.db.QueryRow(`SELECT status FROM statements WHERE id = ?`, result.ID).Scan(&status)
	if status != "pending" {
		t.Fatalf("expected 'pending' before promote, got %q", status)
	}

	err := kbase.PromoteStatement(result.ID)
	if err != nil {
		t.Fatalf("PromoteStatement: %v", err)
	}

	kbase.db.QueryRow(`SELECT status FROM statements WHERE id = ?`, result.ID).Scan(&status)
	if status != "active" {
		t.Errorf("expected 'active' after promote, got %q", status)
	}
}

func TestDeleteStatement(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Delete me", "src", "manual", "", "")

	err := kbase.DeleteStatement(result.ID)
	if err != nil {
		t.Fatalf("DeleteStatement: %v", err)
	}

	_, err = kbase.GetStatement(result.ID)
	if err == nil {
		t.Error("expected error after deletion")
	}
}

// --- Query tests ---

func TestQuery_EmptyDB(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	results, err := kbase.Query("anything", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestQuery_FindsSimilarStatements(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i, text := range texts {
			if strings.Contains(strings.ToLower(text), "go") {
				results[i] = []float64{0.9, 0.1, 0}
			} else {
				results[i] = []float64{0, 0.1, 0.9}
			}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	addAndPromote(t, kbase, "Go Pointers. How Go pointers work", "docs", "manual", []float64{0.9, 0.1, 0})
	addAndPromote(t, kbase, "Pasta Recipe. How to cook pasta", "cookbook", "manual", []float64{0, 0.1, 0.9})

	results, err := kbase.Query("Go programming", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}

	foundGo := false
	for _, r := range results {
		if strings.Contains(r.Content, "Go pointers") {
			foundGo = true
			if r.Score <= 0 {
				t.Error("expected positive score")
			}
			if r.ID == "" {
				t.Error("expected non-empty ID")
			}
		}
	}
	if !foundGo {
		t.Error("expected Go pointers statement in results")
	}
}

func TestQuery_RespectsThreshold(t *testing.T) {
	stub := newStub()
	// Query embedding is orthogonal to the stored one
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{1, 0, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	// Add with orthogonal embedding (dot product with query ≈ 0)
	addAndPromote(t, kbase, "Orthogonal content", "src", "manual", []float64{0, 1, 0})

	results, err := kbase.Query("search query", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if results != nil {
		t.Errorf("expected no results (below threshold), got %d", len(results))
	}
}

func TestQuery_ResultsSortedByScore(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i, text := range texts {
			if strings.Contains(text, "High") {
				results[i] = []float64{0.95, 0.05, 0}
			} else if strings.Contains(text, "Medium") {
				results[i] = []float64{0.7, 0.3, 0}
			} else if strings.Contains(text, "Low") {
				results[i] = []float64{0.4, 0.4, 0.2}
			} else {
				results[i] = []float64{1, 0, 0}
			}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	addAndPromote(t, kbase, "Low Relevance low", "src", "manual", []float64{0.4, 0.4, 0.2})
	addAndPromote(t, kbase, "High Relevance high", "src", "manual", []float64{0.95, 0.05, 0})
	addAndPromote(t, kbase, "Medium Relevance medium", "src", "manual", []float64{0.7, 0.3, 0})

	results, err := kbase.Query("search", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not sorted descending: [%d] score %f > [%d] score %f",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

func TestQuery_ContentTruncated(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	longContent := strings.Repeat("x", 300)
	addAndPromote(t, kbase, "Long Content. "+longContent, "src", "manual", []float64{0.5, 0.5, 0})

	results, err := kbase.Query("Long Content", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected results")
	}

	if len(results[0].Content) > 210 {
		t.Errorf("expected truncated content, got %d chars", len(results[0].Content))
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
		{ID: "abc123", Content: "content", Score: 0.95, LastVerified: "2025-01-01"},
	}
	got := QueryResultsJSON(results)
	if !strings.Contains(got, `"id":"abc123"`) {
		t.Errorf("QueryResultsJSON missing id: %s", got)
	}
	if !strings.Contains(got, `"score":0.95`) {
		t.Errorf("QueryResultsJSON missing score: %s", got)
	}
}

// --- Size validation tests ---

func TestAddStatement_RejectsTooLarge(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	large := strings.Repeat("x", MaxStatementSize+1)
	_, err := kbase.AddStatement(large, "src", "manual", "", "")
	if err == nil {
		t.Fatal("expected error for oversized statement")
	}
	if err != ErrStatementTooLarge {
		t.Errorf("expected ErrStatementTooLarge, got %v", err)
	}
}

func TestAddStatement_AcceptsExactLimit(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	exact := strings.Repeat("x", MaxStatementSize)
	result, err := kbase.AddStatement(exact, "src", "manual", "", "")
	if err != nil {
		t.Fatalf("expected no error at exact limit, got %v", err)
	}
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
}

// --- Embedding failure degradation ---

func TestAddStatement_EmbeddingFailure(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		return nil, fmt.Errorf("embedding service unavailable")
	}
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Survives embedding failure", "src", "manual", "", "")
	if err != nil {
		t.Fatalf("AddStatement should succeed even when embedding fails: %v", err)
	}
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}

	// Verify the row exists with NULL embedding
	var embBlob []byte
	err = kbase.db.QueryRow(`SELECT embedding FROM statements WHERE id = ?`, result.ID).Scan(&embBlob)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}
	if embBlob != nil {
		t.Error("expected NULL embedding when embed fails")
	}

	// Query should not crash on an empty/NULL-embedding DB
	results, err := kbase.Query("anything", "")
	if err != nil {
		t.Fatalf("Query should not error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results (embed fails for query too), got %v", results)
	}
}

// --- Worker tests ---

func TestWorker_PromotesPendingStatement(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.1, 0.2, 0.3}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Worker test content", "src", "manual", "", "")

	// Run one processing cycle
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kbase.processPending(ctx)

	// Check that the statement was promoted
	s, err := kbase.GetStatement(result.ID)
	if err != nil {
		t.Fatalf("GetStatement: %v", err)
	}
	if s.Status != "active" {
		t.Errorf("expected 'active' after worker, got %q", s.Status)
	}

	// Check embedding was stored
	var embBlob []byte
	kbase.db.QueryRow(`SELECT embedding FROM statements WHERE id = ?`, result.ID).Scan(&embBlob)
	if embBlob == nil {
		t.Error("expected embedding to be stored by worker")
	}
}

func TestWorker_CreatesIssueForDuplicates(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		// All texts get the same embedding → guaranteed duplicate
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{1.0, 0.0, 0.0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	// Add two near-identical statements
	r1, _ := kbase.AddStatement("Statement one", "src", "manual", "", "")
	r2, _ := kbase.AddStatement("Statement two", "src", "manual", "", "")

	// Process both
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kbase.processPending(ctx)

	// One should be promoted (whichever FIFO picks first), the other stays pending
	s1, _ := kbase.GetStatement(r1.ID)
	s2, _ := kbase.GetStatement(r2.ID)

	activeCount := 0
	pendingCount := 0
	if s1.Status == "active" {
		activeCount++
	}
	if s1.Status == "pending" {
		pendingCount++
	}
	if s2.Status == "active" {
		activeCount++
	}
	if s2.Status == "pending" {
		pendingCount++
	}

	if activeCount != 1 || pendingCount != 1 {
		t.Errorf("expected 1 active + 1 pending, got active=%d pending=%d (s1=%s s2=%s)",
			activeCount, pendingCount, s1.Status, s2.Status)
	}

	// Check issue was created
	issues, err := kbase.ListOpenIssues()
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("expected at least one issue")
	}
	if issues[0].Type != "duplicate" {
		t.Errorf("expected type 'duplicate', got %q", issues[0].Type)
	}
}

func TestWorker_SkipsOnEmbeddingFailure(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		return nil, fmt.Errorf("ollama down")
	}
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Stuck without embedding", "src", "manual", "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kbase.processPending(ctx)

	// Statement should still be pending
	s, _ := kbase.GetStatement(result.ID)
	if s.Status != "pending" {
		t.Errorf("expected 'pending' when embedding fails, got %q", s.Status)
	}
}

// --- Issue tests ---

func TestIssue_ResolveKeepA(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	// Manually create a controlled issue scenario
	now := time.Now().Format("2006-01-02")
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "keep-a", "Keep this one", now)
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "del-b", "Delete this one", now)

	issueNow := time.Now().Format("2006-01-02T15:04:05Z")
	kbase.db.Exec(`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at) VALUES (?, 'duplicate', 'open', ?, ?, 0.95, ?)`, "iss-ka", "keep-a", "del-b", issueNow)

	err := kbase.ResolveIssue("iss-ka", "keep_a")
	if err != nil {
		t.Fatalf("ResolveIssue: %v", err)
	}

	// Statement A (keep-a) should be active
	s1, _ := kbase.GetStatement("keep-a")
	if s1.Status != "active" {
		t.Errorf("expected A 'active', got %q", s1.Status)
	}

	// Statement B (del-b) should be deleted
	_, err = kbase.GetStatement("del-b")
	if err == nil {
		t.Error("expected B to be deleted")
	}

	// Issue should be resolved
	count, _ := kbase.OpenIssueCount()
	if count != 0 {
		t.Errorf("expected 0 open issues, got %d", count)
	}
}

func TestIssue_CascadeClose(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	// Manually create statements and issues for cascading test
	now := time.Now().Format("2006-01-02")
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-a", "A content", now)
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-b", "B content", now)
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-c", "C content", now)

	issueNow := time.Now().Format("2006-01-02T15:04:05Z")
	kbase.db.Exec(`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at) VALUES (?, 'duplicate', 'open', ?, ?, 0.9, ?)`, "iss-1", "stmt-a", "stmt-b", issueNow)
	kbase.db.Exec(`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at) VALUES (?, 'duplicate', 'open', ?, ?, 0.85, ?)`, "iss-2", "stmt-a", "stmt-c", issueNow)

	// Resolve issue 1 by deleting A → issue 2 should cascade close
	err := kbase.ResolveIssue("iss-1", "keep_b")
	if err != nil {
		t.Fatalf("ResolveIssue: %v", err)
	}

	count, _ := kbase.OpenIssueCount()
	if count != 0 {
		t.Errorf("expected 0 open issues after cascade, got %d", count)
	}
}

func TestIssue_KeepBoth(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	now := time.Now().Format("2006-01-02")
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-x", "X content", now)
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-y", "Y content", now)

	issueNow := time.Now().Format("2006-01-02T15:04:05Z")
	kbase.db.Exec(`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at) VALUES (?, 'duplicate', 'open', ?, ?, 0.9, ?)`, "iss-kb", "stmt-x", "stmt-y", issueNow)

	err := kbase.ResolveIssue("iss-kb", "keep_both")
	if err != nil {
		t.Fatalf("ResolveIssue: %v", err)
	}

	// Both should be active
	sx, _ := kbase.GetStatement("stmt-x")
	sy, _ := kbase.GetStatement("stmt-y")
	if sx.Status != "active" {
		t.Errorf("expected X 'active', got %q", sx.Status)
	}
	if sy.Status != "active" {
		t.Errorf("expected Y 'active', got %q", sy.Status)
	}
}

func TestIssue_DeleteBoth(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	now := time.Now().Format("2006-01-02")
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-d1", "D1 content", now)
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-d2", "D2 content", now)

	issueNow := time.Now().Format("2006-01-02T15:04:05Z")
	kbase.db.Exec(`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at) VALUES (?, 'duplicate', 'open', ?, ?, 0.9, ?)`, "iss-db", "stmt-d1", "stmt-d2", issueNow)

	err := kbase.ResolveIssue("iss-db", "delete_both")
	if err != nil {
		t.Fatalf("ResolveIssue: %v", err)
	}

	_, err = kbase.GetStatement("stmt-d1")
	if err == nil {
		t.Error("expected D1 to be deleted")
	}
	_, err = kbase.GetStatement("stmt-d2")
	if err == nil {
		t.Error("expected D2 to be deleted")
	}
}

// --- Flag/Restore tests ---

func TestFlagStatement(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Flag me", "src", "manual", "", "")

	err := kbase.FlagStatement(result.ID)
	if err != nil {
		t.Fatalf("FlagStatement: %v", err)
	}

	var flaggedAt string
	kbase.db.QueryRow(`SELECT flagged_at FROM statements WHERE id = ?`, result.ID).Scan(&flaggedAt)
	if flaggedAt == "" {
		t.Error("expected flagged_at to be set")
	}
}

func TestFlagStatement_NotFound(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	err := kbase.FlagStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
	}
}

func TestFlagStatement_AlreadyFlagged(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Double flag me", "src", "manual", "", "")
	kbase.FlagStatement(result.ID)

	err := kbase.FlagStatement(result.ID)
	if err == nil {
		t.Fatal("expected error for already flagged statement")
	}
}

func TestRestoreStatement(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Restore me", "src", "manual", "", "")
	kbase.FlagStatement(result.ID)

	err := kbase.RestoreStatement(result.ID)
	if err != nil {
		t.Fatalf("RestoreStatement: %v", err)
	}

	var flaggedAt string
	kbase.db.QueryRow(`SELECT flagged_at FROM statements WHERE id = ?`, result.ID).Scan(&flaggedAt)
	if flaggedAt != "" {
		t.Errorf("expected flagged_at to be cleared, got %q", flaggedAt)
	}
}

func TestRestoreStatement_NotFlagged(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Not flagged", "src", "manual", "", "")

	err := kbase.RestoreStatement(result.ID)
	if err == nil {
		t.Fatal("expected error for non-flagged statement")
	}
}

func TestRestoreStatement_NotFound(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	err := kbase.RestoreStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
	}
}

// --- ListFlaggedStatements tests ---

func TestListFlaggedStatements_Empty(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	flagged, err := kbase.ListFlaggedStatements()
	if err != nil {
		t.Fatalf("ListFlaggedStatements: %v", err)
	}
	if len(flagged) != 0 {
		t.Errorf("expected no flagged statements, got %d", len(flagged))
	}
}

func TestListFlaggedStatements_ReturnsFlagged(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	r1, _ := kbase.AddStatement("Flagged one", "src1", "manual", "", "")
	kbase.AddStatement("Not flagged", "src2", "manual", "", "")
	r3, _ := kbase.AddStatement("Flagged two", "src3", "manual", "", "")

	kbase.FlagStatement(r1.ID)
	kbase.FlagStatement(r3.ID)

	flagged, err := kbase.ListFlaggedStatements()
	if err != nil {
		t.Fatalf("ListFlaggedStatements: %v", err)
	}
	if len(flagged) != 2 {
		t.Errorf("expected 2 flagged statements, got %d", len(flagged))
	}

	for _, f := range flagged {
		if f.ID == "" {
			t.Error("expected non-empty ID")
		}
		if f.Content == "" {
			t.Error("expected non-empty Content")
		}
		if f.FlaggedAt == "" {
			t.Error("expected non-empty FlaggedAt")
		}
	}
}

// --- Scope tests ---

func TestAddStatement_WithScope(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	r1, err := kbase.AddStatement("User statement", "src", "manual", "user", "")
	if err != nil {
		t.Fatalf("AddStatement(user): %v", err)
	}

	r2, err := kbase.AddStatement("Project statement", "src", "manual", "project", "my-project")
	if err != nil {
		t.Fatalf("AddStatement(project): %v", err)
	}

	var scope1, project1 string
	kbase.db.QueryRow(`SELECT scope, project FROM statements WHERE id = ?`, r1.ID).Scan(&scope1, &project1)
	if scope1 != "user" {
		t.Errorf("expected scope 'user', got %q", scope1)
	}
	if project1 != "" {
		t.Errorf("expected empty project, got %q", project1)
	}

	var scope2, project2 string
	kbase.db.QueryRow(`SELECT scope, project FROM statements WHERE id = ?`, r2.ID).Scan(&scope2, &project2)
	if scope2 != "project" {
		t.Errorf("expected scope 'project', got %q", scope2)
	}
	if project2 != "my-project" {
		t.Errorf("expected project 'my-project', got %q", project2)
	}
}

func TestAddStatement_DefaultScope(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Default scope", "src", "manual", "", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}

	var scope string
	kbase.db.QueryRow(`SELECT scope FROM statements WHERE id = ?`, result.ID).Scan(&scope)
	if scope != "user" {
		t.Errorf("expected default scope 'user', got %q", scope)
	}
}

// --- Query with scope tests ---

func TestQuery_FiltersProjectScope(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	addAndPromoteWithScope(t, kbase, "User visible", "src", "manual", []float64{0.9, 0.1, 0}, "user", "")
	addAndPromoteWithScope(t, kbase, "Project A only", "src", "manual", []float64{0.9, 0.1, 0}, "project", "project-a")
	addAndPromoteWithScope(t, kbase, "Project B only", "src", "manual", []float64{0.9, 0.1, 0}, "project", "project-b")

	results, err := kbase.Query("search", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result (user-scoped only), got %d", len(results))
	}

	results, err = kbase.Query("search", "project-a")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results (user + project-a), got %d", len(results))
	}
}

func addAndPromoteWithScope(t *testing.T, kbase *KnowledgeBase, content, source, sourceType string, emb []float64, scope, project string) string {
	t.Helper()
	result, err := kbase.AddStatement(content, source, sourceType, scope, project)
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}
	if emb != nil {
		blob := embeddingToBlob(emb)
		_, err = kbase.db.Exec(
			`UPDATE statements SET embedding = ?, model = ?, status = 'active' WHERE id = ?`,
			blob, kbase.embeddingModel, result.ID,
		)
		if err != nil {
			t.Fatalf("manual promote: %v", err)
		}
	}
	return result.ID
}

// --- Soft-delete workflow tests (flag → hidden → restore → visible) ---

func TestFlaggedStatementIsHiddenFromQueries(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	id := addAndPromote(t, kbase, "Visible statement", "src", "manual", []float64{0.9, 0.1, 0})

	results, _ := kbase.Query("Visible", "")
	if len(results) != 1 {
		t.Fatalf("precondition: expected statement to be queryable")
	}

	kbase.FlagStatement(id)
	results, _ = kbase.Query("Visible", "")
	if len(results) != 0 {
		t.Errorf("flagged statement should be hidden from queries, got %d results", len(results))
	}
}

func TestRestoredStatementBecomesQueryableAgain(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	id := addAndPromote(t, kbase, "Will restore", "src", "manual", []float64{0.9, 0.1, 0})
	kbase.FlagStatement(id)

	results, _ := kbase.Query("restore", "")
	if len(results) != 0 {
		t.Fatalf("precondition: flagged statement should be hidden")
	}

	kbase.RestoreStatement(id)
	results, _ = kbase.Query("restore", "")
	if len(results) != 1 {
		t.Errorf("restored statement should be queryable, got %d results", len(results))
	}
}

func TestConfirmDeleteRemovesStatementPermanently(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("To be deleted", "src", "manual", "", "")
	kbase.FlagStatement(result.ID)

	flagged, _ := kbase.ListFlaggedStatements()
	if len(flagged) != 1 {
		t.Fatalf("precondition: expected 1 flagged statement")
	}

	err := kbase.DeleteStatement(result.ID)
	if err != nil {
		t.Fatalf("DeleteStatement: %v", err)
	}

	flagged, _ = kbase.ListFlaggedStatements()
	if len(flagged) != 0 {
		t.Errorf("deleted statement should not appear in flagged list")
	}

	_, err = kbase.GetStatement(result.ID)
	if err == nil {
		t.Error("statement should be permanently deleted")
	}
}

// --- truncateRunes tests ---

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		input    string
		limit    int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "hel..."},
		{"", 5, ""},
		{"日本語テスト", 3, "日本語..."},
		{"日本語テスト", 10, "日本語テスト"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncateRunes(tt.input, tt.limit)
			if got != tt.expected {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.input, tt.limit, got, tt.expected)
			}
		})
	}
}

// --- ResolveIssue error cases ---

func TestResolveIssue_NotFound(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	err := kbase.ResolveIssue("nonexistent-id", "keep_a")
	if err == nil {
		t.Fatal("expected error for nonexistent issue")
	}
}

func TestResolveIssue_AlreadyResolved(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	now := time.Now().Format("2006-01-02")
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'active', ?)`, "stmt-ar-a", "A", now)
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'active', ?)`, "stmt-ar-b", "B", now)

	issueNow := time.Now().Format("2006-01-02T15:04:05Z")
	kbase.db.Exec(`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at, resolved_at) VALUES (?, 'duplicate', 'resolved', ?, ?, 0.9, ?, ?)`, "iss-ar", "stmt-ar-a", "stmt-ar-b", issueNow, issueNow)

	err := kbase.ResolveIssue("iss-ar", "keep_a")
	if err == nil {
		t.Fatal("expected error for already resolved issue")
	}
}

func TestResolveIssue_UnknownAction(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	now := time.Now().Format("2006-01-02")
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-ua-a", "A", now)
	kbase.db.Exec(`INSERT INTO statements (id, content, source, source_type, status, created_at) VALUES (?, ?, '', 'manual', 'pending', ?)`, "stmt-ua-b", "B", now)

	issueNow := time.Now().Format("2006-01-02T15:04:05Z")
	kbase.db.Exec(`INSERT INTO issues (id, type, status, statement_a, statement_b, score, created_at) VALUES (?, 'duplicate', 'open', ?, ?, 0.9, ?)`, "iss-ua", "stmt-ua-a", "stmt-ua-b", issueNow)

	err := kbase.ResolveIssue("iss-ua", "invalid_action")
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

// --- PromoteStatement and DeleteStatement error cases ---

func TestPromoteStatement_NotFound(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	err := kbase.PromoteStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
	}
}

func TestDeleteStatement_NotFound(t *testing.T) {
	stub := newStub()
	kbase := openTestKB(t, stub)

	err := kbase.DeleteStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
	}
}
