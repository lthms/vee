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

func TestOpen_DefaultRootExists(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	var id string
	err := kbase.db.QueryRow(`SELECT id FROM roots WHERE id = 'default'`).Scan(&id)
	if err != nil {
		t.Fatalf("default root not found: %v", err)
	}
	if id != "default" {
		t.Errorf("expected 'default', got %q", id)
	}
}

// --- Statement CRUD tests ---

func TestAddStatement_CreatesRow(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	id, err := kbase.AddStatement("Test Statement. Some content", "file.go", "manual")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}

	// Verify DB row — title should be derived as first sentence
	var title, content, status string
	err = kbase.db.QueryRow(`SELECT title, content, status FROM statements WHERE id = ?`, id).Scan(&title, &content, &status)
	if err != nil {
		t.Fatalf("query DB: %v", err)
	}
	if title != "Test Statement." {
		t.Errorf("expected title 'Test Statement.', got %q", title)
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

	id, err := kbase.AddStatement("Embedded content", "src", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}

	// Verify embedding stored
	var embBlob []byte
	err = kbase.db.QueryRow(`SELECT embedding FROM statements WHERE id = ?`, id).Scan(&embBlob)
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

	id, err := kbase.AddStatement("Default ST content", "src", "")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}

	var sourceType string
	kbase.db.QueryRow(`SELECT source_type FROM statements WHERE id = ?`, id).Scan(&sourceType)
	if sourceType != "manual" {
		t.Errorf("expected source_type 'manual', got %q", sourceType)
	}
}

func TestAddStatement_EnqueuesBackgroundTasks(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	id, err := kbase.AddStatement("Queue Test content", "src", "manual")
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}

	// Check that cluster_assign and contradiction_check were enqueued
	var clusterCount, contradictionCount int
	kbase.db.QueryRow(
		`SELECT COUNT(*) FROM processing_queue WHERE task_type = 'cluster_assign' AND payload = ?`, id,
	).Scan(&clusterCount)
	kbase.db.QueryRow(
		`SELECT COUNT(*) FROM processing_queue WHERE task_type = 'contradiction_check' AND payload = ?`, id,
	).Scan(&contradictionCount)

	if clusterCount != 1 {
		t.Errorf("expected 1 cluster_assign task, got %d", clusterCount)
	}
	if contradictionCount != 1 {
		t.Errorf("expected 1 contradiction_check task, got %d", contradictionCount)
	}
}

func TestGetStatement(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	id, _ := kbase.AddStatement("Get Test. Content here", "file.go", "manual")

	s, err := kbase.GetStatement(id)
	if err != nil {
		t.Fatalf("GetStatement: %v", err)
	}
	if s.Title != "Get Test." {
		t.Errorf("expected title 'Get Test.', got %q", s.Title)
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

	id, _ := kbase.AddStatement("Fetch Test. Detailed content", "src.go", "manual")

	content, err := kbase.FetchStatement(id)
	if err != nil {
		t.Fatalf("FetchStatement: %v", err)
	}
	if !strings.Contains(content, "# Fetch Test.") {
		t.Error("expected formatted title")
	}
	if !strings.Contains(content, "Detailed content") {
		t.Error("expected content body")
	}
	if !strings.Contains(content, "Source: src.go") {
		t.Error("expected source info")
	}
}

func TestTouchStatement(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	id, _ := kbase.AddStatement("Touch Test body", "src", "manual")

	err := kbase.TouchStatement(id)
	if err != nil {
		t.Fatalf("TouchStatement: %v", err)
	}

	var lv string
	kbase.db.QueryRow(`SELECT last_verified FROM statements WHERE id = ?`, id).Scan(&lv)
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
	// Embedding function: "go" topics get [0.9,0.1,0], others get [0,0.1,0.9]
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

	// Should contain Go statement, sorted by score
	foundGo := false
	for _, r := range results {
		if r.Title == "Go Pointers." {
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
		t.Error("expected 'Go Pointers.' in results")
	}
}

func TestQuery_RespectsThreshold(t *testing.T) {
	stub := newStub("ok")
	callIdx := 0
	stub.embedFn = func(texts []string) ([][]float64, error) {
		callIdx++
		results := make([][]float64, len(texts))
		for i := range texts {
			// Query gets [1,0,0], statement gets [0,1,0] (orthogonal = score 0)
			if callIdx == 1 {
				// This is AddStatement's call
				results[i] = []float64{0, 1, 0}
			} else {
				// This is Query's call
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
				// Query embedding
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

	// Verify descending order
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

	// Content should be truncated in results
	if len(results[0].Content) > 210 {
		t.Errorf("expected truncated content, got %d chars", len(results[0].Content))
	}
}

// --- Queue tests ---

func TestEnqueueDequeue(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	kbase.Enqueue("test_task", "payload1", 5)
	kbase.Enqueue("test_task", "payload2", 10)

	// Higher priority should come first
	task, err := kbase.Dequeue("test_task")
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if task == nil {
		t.Fatal("expected a task")
	}
	if task.Payload != "payload2" {
		t.Errorf("expected payload2 (higher priority), got %q", task.Payload)
	}
	if task.Status != "processing" {
		t.Errorf("expected status 'processing', got %q", task.Status)
	}

	kbase.CompleteTask(task.ID)

	// Next one
	task2, _ := kbase.Dequeue("test_task")
	if task2 == nil {
		t.Fatal("expected second task")
	}
	if task2.Payload != "payload1" {
		t.Errorf("expected payload1, got %q", task2.Payload)
	}
}

func TestDequeue_ReturnsNilWhenEmpty(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	task, err := kbase.Dequeue("nonexistent")
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if task != nil {
		t.Errorf("expected nil task, got %+v", task)
	}
}

func TestFailTask_RetriesWhenUnderMax(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	kbase.Enqueue("retry_test", "payload", 5)

	task, _ := kbase.Dequeue("retry_test")
	kbase.FailTask(task.ID, fmt.Errorf("temporary error"))

	// Should be back to pending (attempt 1 < max_attempts 3)
	var status string
	kbase.db.QueryRow(`SELECT status FROM processing_queue WHERE id = ?`, task.ID).Scan(&status)
	if status != "pending" {
		t.Errorf("expected 'pending' after failure, got %q", status)
	}
}

// --- Cluster tests ---

func TestAssignToCluster_CreatesNewCluster(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	id, _ := kbase.AddStatement("Cluster Test content", "src", "manual")

	err := kbase.AssignToCluster(id)
	if err != nil {
		t.Fatalf("AssignToCluster: %v", err)
	}

	// Should have created a cluster and a membership
	var clusterCount, memberCount int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM clusters`).Scan(&clusterCount)
	kbase.db.QueryRow(`SELECT COUNT(*) FROM cluster_members WHERE statement_id = ?`, id).Scan(&memberCount)

	if clusterCount == 0 {
		t.Error("expected at least one cluster")
	}
	if memberCount == 0 {
		t.Error("expected cluster membership")
	}
}

func TestAssignToCluster_JoinsExistingCluster(t *testing.T) {
	stub := newStub("ok")
	// All embeddings are identical, so similarity = 1.0
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.9, 0.1, 0}
		}
		return results, nil
	}
	kbase := openTestKB(t, stub)

	id1, _ := kbase.AddStatement("First content", "src", "manual")
	kbase.AssignToCluster(id1)

	id2, _ := kbase.AddStatement("Second content", "src", "manual")
	kbase.AssignToCluster(id2)

	// Both should be in the same cluster
	var clusterCount int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM clusters`).Scan(&clusterCount)
	if clusterCount != 1 {
		t.Errorf("expected 1 cluster (joined existing), got %d", clusterCount)
	}

	var memberCount int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM cluster_members`).Scan(&memberCount)
	if memberCount != 2 {
		t.Errorf("expected 2 members, got %d", memberCount)
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
		{ID: "abc123", Title: "A", Content: "content", Score: 0.95, LastVerified: "2025-01-01"},
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

// --- deriveTitle tests ---

func TestDeriveTitle_FirstSentence(t *testing.T) {
	got := deriveTitle("Sessions move through three statuses. Active, suspended, completed.")
	if got != "Sessions move through three statuses." {
		t.Errorf("expected first sentence, got %q", got)
	}
}

func TestDeriveTitle_NewlineBoundary(t *testing.T) {
	got := deriveTitle("First line\nSecond line")
	if got != "First line" {
		t.Errorf("expected first line, got %q", got)
	}
}

func TestDeriveTitle_NewlineBeforeDot(t *testing.T) {
	got := deriveTitle("Short line\nThen a sentence. More text.")
	if got != "Short line" {
		t.Errorf("expected newline to win over dot, got %q", got)
	}
}

func TestDeriveTitle_LongNoBoundary(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := deriveTitle(long)
	if len(got) != 121 { // 120 + "…" (3 bytes in UTF-8, but len counts runes... actually len counts bytes)
		// "…" is 3 bytes in UTF-8
		if got != long[:120]+"…" {
			t.Errorf("expected truncated with ellipsis, got %q (len %d)", got, len(got))
		}
	}
}

func TestDeriveTitle_ShortString(t *testing.T) {
	got := deriveTitle("Hello world")
	if got != "Hello world" {
		t.Errorf("expected full string, got %q", got)
	}
}

func TestDeriveTitle_Empty(t *testing.T) {
	got := deriveTitle("")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
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
	id, err := kbase.AddStatement(exact, "src", "manual")
	if err != nil {
		t.Fatalf("expected no error at exact limit, got %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}
}

// --- Audit test ---

func TestLogModelCall(t *testing.T) {
	stub := newStub("ok")
	kbase := openTestKB(t, stub)

	kbase.LogModelCall("test-model", "embed", "statement", "abc123", 42)

	var count int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM model_audit WHERE operation = 'embed'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 audit entry, got %d", count)
	}
}
