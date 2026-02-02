package kb

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubModel implements Model for testing.
type stubModel struct {
	mu        sync.Mutex
	responses []string
	errors    []error
	calls     []string
	idx       int

	embedFn func(texts []string) ([][]float64, error)
}

func newStub(responses ...string) *stubModel {
	return &stubModel{
		responses: responses,
		embedFn: func(texts []string) ([][]float64, error) {
			results := make([][]float64, len(texts))
			for i := range texts {
				results[i] = []float64{0.5, 0.5, 0}
			}
			return results, nil
		},
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

// --- Schema & migration tests ---

func TestOpen_CreatesDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kb.db")

	kbase, err := Open(Config{
		DBPath: dbPath,
		Model:  newStub("ok"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer kbase.Close()
}

func TestOpen_MigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kb.db")
	cfg := Config{DBPath: dbPath, Model: newStub("ok")}

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
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Test Statement. Some content", "file.go", "manual")
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
	if status != "active" {
		t.Errorf("expected status 'active', got %q", status)
	}
}

func TestAddStatement_StoresEmbedding(t *testing.T) {
	stub := newStub("ok")
	stub.embedFn = func(texts []string) ([][]float64, error) {
		return [][]float64{{0.1, 0.2, 0.3}}, nil
	}
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Embedded content", "src", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}

	var embBlob []byte
	err = kbase.db.QueryRow(`SELECT embedding FROM statements WHERE id = ?`, result.ID).Scan(&embBlob)
	if err != nil {
		t.Fatalf("query embedding: %v", err)
	}
	if embBlob == nil {
		t.Error("expected embedding to be stored")
	}
	emb := blobToEmbedding(embBlob)
	if len(emb) != 3 {
		t.Errorf("expected 3-dimensional embedding, got %d", len(emb))
	}
}

func TestAddStatement_DefaultSourceType(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	result, err := kbase.AddStatement("Default ST content", "src", "")
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
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Get Test. Content here", "file.go", "manual")

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
	if s.Status != "active" {
		t.Errorf("expected status 'active', got %q", s.Status)
	}
}

func TestGetStatement_NotFound(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	_, err := kbase.GetStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
	}
}

func TestFetchStatement(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Fetch Test. Detailed content", "src.go", "manual")

	content, err := kbase.FetchStatement(result.ID)
	if err != nil {
		t.Fatalf("FetchStatement: %v", err)
	}
	if !strings.Contains(content, "Fetch Test. Detailed content") {
		t.Error("expected content body")
	}
	if !strings.Contains(content, "Source: src.go") {
		t.Error("expected source info")
	}
}

func TestTouchStatement(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	result, _ := kbase.AddStatement("Touch Test body", "src", "manual")

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
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	err := kbase.TouchStatement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent statement")
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

func TestQuery_FindsSimilarStatements(t *testing.T) {
	stub := newStub("ok")
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

	kbase.AddStatement("Go Pointers. How Go pointers work", "docs", "manual")
	kbase.AddStatement("Pasta Recipe. How to cook pasta", "cookbook", "manual")

	results, err := kbase.Query("Go programming")
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
	stub := newStub("ok")
	callIdx := 0
	stub.embedFn = func(texts []string) ([][]float64, error) {
		callIdx++
		results := make([][]float64, len(texts))
		for i := range texts {
			if callIdx == 1 {
				results[i] = []float64{0, 1, 0}
			} else {
				results[i] = []float64{1, 0, 0}
			}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	kbase.AddStatement("Orthogonal content", "src", "manual")

	results, err := kbase.Query("search query")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if results != nil {
		t.Errorf("expected no results (below threshold), got %d", len(results))
	}
}

func TestQuery_ResultsSortedByScore(t *testing.T) {
	stub := newStub("ok")
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

	kbase.AddStatement("Low Relevance low", "src", "manual")
	kbase.AddStatement("High Relevance high", "src", "manual")
	kbase.AddStatement("Medium Relevance medium", "src", "manual")

	results, err := kbase.Query("search")
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
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	longContent := strings.Repeat("x", 300)
	kbase.AddStatement("Long Content. "+longContent, "src", "manual")

	results, err := kbase.Query("Long Content")
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

// --- Near-duplicate detection tests ---

func TestAddStatement_DetectsNearDuplicates(t *testing.T) {
	stub := newStub("ok")
	// All embeddings identical -> cosine similarity = 1.0
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	r1, _ := kbase.AddStatement("First content", "src", "manual")
	r2, _ := kbase.AddStatement("Second content", "src", "manual")

	if len(r2.NearDuplicates) == 0 {
		t.Fatal("expected near-duplicates for second statement")
	}
	found := false
	for _, id := range r2.NearDuplicates {
		if id == r1.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected first statement %s in near-duplicates, got %v", r1.ID, r2.NearDuplicates)
	}

	// Verify a pending nogood was queued
	var count int
	var status string
	kbase.db.QueryRow(`SELECT COUNT(*) FROM nogoods WHERE status = 'pending'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 pending nogood, got %d", count)
	}
	kbase.db.QueryRow(
		`SELECT status FROM nogoods WHERE (stmt_a_id = ? AND stmt_b_id = ?) OR (stmt_a_id = ? AND stmt_b_id = ?)`,
		r2.ID, r1.ID, r1.ID, r2.ID,
	).Scan(&status)
	if status != "pending" {
		t.Errorf("expected nogood status 'pending', got %q", status)
	}

	// No model.Generate calls should have been made during ingestion
	if stub.callCount() > 0 {
		t.Errorf("expected no model.Generate calls, got %d", stub.callCount())
	}
}

func TestAddStatement_NoCandidatesBelowThreshold(t *testing.T) {
	stub := newStub("ok")
	callIdx := 0
	stub.embedFn = func(texts []string) ([][]float64, error) {
		callIdx++
		results := make([][]float64, len(texts))
		for i := range texts {
			if callIdx == 1 {
				results[i] = []float64{1, 0, 0}
			} else {
				results[i] = []float64{0, 0, 1}
			}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	kbase.AddStatement("First orthogonal", "src", "manual")
	r2, _ := kbase.AddStatement("Second orthogonal", "src", "manual")

	if len(r2.NearDuplicates) > 0 {
		t.Errorf("expected no near-duplicates for orthogonal statements, got %v", r2.NearDuplicates)
	}
	if r2.CandidatePairs > 0 {
		t.Errorf("expected no candidate pairs, got %d", r2.CandidatePairs)
	}

	var count int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM nogoods`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 nogoods, got %d", count)
	}
}

func TestAddStatement_QueuesCandidatePairs(t *testing.T) {
	stub := newStub("ok")
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	r1, _ := kbase.AddStatement("Statement A", "src", "manual")
	r2, _ := kbase.AddStatement("Statement B", "src", "manual")
	r3, _ := kbase.AddStatement("Statement C", "src", "manual")

	// r2 should have queued 1 pair (with r1)
	if r2.CandidatePairs != 1 {
		t.Errorf("expected 1 candidate pair for r2, got %d", r2.CandidatePairs)
	}

	// r3 should have queued 2 pairs (with r1 and r2)
	if r3.CandidatePairs != 2 {
		t.Errorf("expected 2 candidate pairs for r3, got %d", r3.CandidatePairs)
	}

	// Total nogoods should be 3: (r2,r1), (r3,r1), (r3,r2)
	var count int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM nogoods`).Scan(&count)
	if count != 3 {
		t.Errorf("expected 3 nogoods total, got %d", count)
	}

	// All should be pending
	kbase.db.QueryRow(`SELECT COUNT(*) FROM nogoods WHERE status = 'pending'`).Scan(&count)
	if count != 3 {
		t.Errorf("expected 3 pending nogoods, got %d", count)
	}

	// No model.Generate calls
	if stub.callCount() > 0 {
		t.Errorf("expected no model.Generate calls, got %d", stub.callCount())
	}

	_ = r1
}

func TestAddStatement_SkipsExistingNogoods(t *testing.T) {
	stub := newStub("ok")
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	r1, _ := kbase.AddStatement("First content", "src", "manual")

	// Pre-insert a nogood for (r1, placeholder) — won't match the real pair
	kbase.db.Exec(
		`INSERT INTO nogoods (stmt_a_id, stmt_b_id, status, detected_at) VALUES (?, 'placeholder', 'pending', '2025-01-01')`,
		r1.ID,
	)

	r2, _ := kbase.AddStatement("Second content", "src", "manual")

	// r2 should still queue a pair with r1 since the existing nogood is for a different pair
	if r2.CandidatePairs != 1 {
		t.Errorf("expected 1 candidate pair, got %d", r2.CandidatePairs)
	}

	// Now add r3 — the pair (r3, r1) and (r3, r2) should be new, but (r2, r1) already exists
	r3, _ := kbase.AddStatement("Third content", "src", "manual")
	if r3.CandidatePairs != 2 {
		t.Errorf("expected 2 candidate pairs for r3, got %d", r3.CandidatePairs)
	}

	// Total: 1 pre-inserted + 1 from r2 + 2 from r3 = 4
	var count int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM nogoods`).Scan(&count)
	if count != 4 {
		t.Errorf("expected 4 nogoods total, got %d", count)
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

// --- Size validation tests ---

func TestAddStatement_RejectsTooLarge(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	large := strings.Repeat("x", MaxStatementSize+1)
	_, err := kbase.AddStatement(large, "src", "manual")
	if err == nil {
		t.Fatal("expected error for oversized statement")
	}
	if err != ErrStatementTooLarge {
		t.Errorf("expected ErrStatementTooLarge, got %v", err)
	}
}

func TestAddStatement_AcceptsExactLimit(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	exact := strings.Repeat("x", MaxStatementSize)
	result, err := kbase.AddStatement(exact, "src", "manual")
	if err != nil {
		t.Fatalf("expected no error at exact limit, got %v", err)
	}
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
}
