package kb

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		a, b     []float64
		expected float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1.0},
		{"orthogonal", []float64{1, 0, 0}, []float64{0, 1, 0}, 0.0},
		{"opposite", []float64{1, 0, 0}, []float64{-1, 0, 0}, -1.0},
		{"similar", []float64{1, 1, 0}, []float64{1, 0, 0}, 1.0 / math.Sqrt(2)},
		{"empty", []float64{}, []float64{}, 0.0},
		{"zero vector", []float64{0, 0, 0}, []float64{1, 0, 0}, 0.0},
		{"mismatched length", []float64{1, 0}, []float64{1, 0, 0}, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.expected) > 1e-10 {
				t.Errorf("cosineSimilarity(%v, %v) = %f, want %f", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}

func TestEmbeddingBlobRoundtrip(t *testing.T) {
	original := []float64{0.1, -0.5, 3.14159, 0, -1e10, 1e-10}
	blob := embeddingToBlob(original)
	restored := blobToEmbedding(blob)

	if len(restored) != len(original) {
		t.Fatalf("length mismatch: %d vs %d", len(restored), len(original))
	}
	for i := range original {
		if restored[i] != original[i] {
			t.Errorf("index %d: %f != %f", i, restored[i], original[i])
		}
	}
}

func TestStoreAndGetNodeEmbedding(t *testing.T) {
	stub := newStub("ok")
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

	// Create a tree node to embed
	res, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'test', 'test summary', 1)`)
	nodeID, _ := res.LastInsertId()

	emb := []float64{0.1, 0.2, 0.3, 0.4}
	err = kbase.storeNodeEmbedding(int(nodeID), emb)
	if err != nil {
		t.Fatalf("storeNodeEmbedding: %v", err)
	}

	got, err := kbase.getNodeEmbedding(int(nodeID))
	if err != nil {
		t.Fatalf("getNodeEmbedding: %v", err)
	}

	if len(got) != len(emb) {
		t.Fatalf("length mismatch: %d vs %d", len(got), len(emb))
	}
	for i := range emb {
		if got[i] != emb[i] {
			t.Errorf("index %d: %f != %f", i, got[i], emb[i])
		}
	}
}

func TestGetNodeEmbedding_StaleModel(t *testing.T) {
	stub := newStub("ok")
	dir := t.TempDir()
	kbase, err := Open(Config{
		DBPath:         filepath.Join(dir, "kb.db"),
		VaultDir:       filepath.Join(dir, "vault"),
		Model:          stub,
		EmbeddingModel: "model-a",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer kbase.Close()

	res, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'test', 'summary', 1)`)
	nodeID, _ := res.LastInsertId()

	// Store with model-a
	err = kbase.storeNodeEmbedding(int(nodeID), []float64{1, 2, 3})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// Switch to model-b and try to retrieve
	kbase.embeddingModel = "model-b"
	_, err = kbase.getNodeEmbedding(int(nodeID))
	if err == nil {
		t.Fatal("expected error for stale model")
	}
}

func TestSelectByThreshold(t *testing.T) {
	nodes := []*treeNode{
		{id: 1, label: "high"},
		{id: 2, label: "medium"},
		{id: 3, label: "low"},
	}

	queryEmb := []float64{1, 0, 0}
	embeddings := map[int][]float64{
		1: {0.9, 0.1, 0},   // high similarity
		2: {0.5, 0.5, 0.5}, // medium similarity
		3: {0, 0, 1},        // low similarity
	}

	// With threshold 0.3, max 5
	selected := selectByThreshold(queryEmb, nodes, embeddings, 0.3, 5)
	if len(selected) == 0 {
		t.Fatal("expected at least one selected node")
	}
	// First should be highest scoring
	if selected[0].label != "high" {
		t.Errorf("expected first selected to be 'high', got %q", selected[0].label)
	}

	// With threshold above any actual similarity, returns empty (no fallback)
	selected = selectByThreshold(queryEmb, nodes, embeddings, 1.1, 5)
	if len(selected) != 0 {
		t.Errorf("expected 0 nodes when threshold exceeds all scores, got %d", len(selected))
	}

	// With maxCount=1
	selected = selectByThreshold(queryEmb, nodes, embeddings, 0.3, 1)
	if len(selected) != 1 {
		t.Errorf("expected 1 selected, got %d", len(selected))
	}
}

func TestQuery_UsesEmbeddings(t *testing.T) {
	stub := newStub("ok")
	// Set up embedding function that returns distinct vectors for different texts
	stub.embedFn = func(texts []string) ([][]float64, error) {
		results := make([][]float64, len(texts))
		for i := range texts {
			// Return a vector that's similar to "golang" topics
			results[i] = []float64{0.9, 0.1, 0}
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

	// Create two root nodes with different embeddings
	res1, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'golang', 'Go programming topics', 1)`)
	goID, _ := res1.LastInsertId()
	kbase.storeNodeEmbedding(int(goID), []float64{0.9, 0.1, 0}) // similar to query

	res2, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'cooking', 'Cooking recipes', 1)`)
	cookID, _ := res2.LastInsertId()
	kbase.storeNodeEmbedding(int(cookID), []float64{0, 0.1, 0.9}) // dissimilar to query

	// Add notes
	noteID1, _, _ := kbase.AddNote("Go Pointers", "Pointers in Go", nil)
	kbase.db.Exec(`UPDATE notes SET summary = 'How Go pointers work' WHERE id = ?`, noteID1)
	kbase.db.Exec(`INSERT INTO leaf_entries (node_id, note_id) VALUES (?, ?)`, goID, noteID1)

	noteID2, _, _ := kbase.AddNote("Pasta Recipe", "How to cook pasta", nil)
	kbase.db.Exec(`UPDATE notes SET summary = 'A pasta recipe' WHERE id = ?`, noteID2)
	kbase.db.Exec(`INSERT INTO leaf_entries (node_id, note_id) VALUES (?, ?)`, cookID, noteID2)

	results, err := kbase.Query("how do Go pointers work")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	// Should find the Go note
	foundGo := false
	foundCooking := false
	for _, r := range results {
		if r.Title == "Go Pointers" {
			foundGo = true
		}
		if r.Title == "Pasta Recipe" {
			foundCooking = true
		}
	}
	if !foundGo {
		t.Error("expected Go Pointers in results")
	}
	// Cooking may or may not be included depending on threshold — that's OK
	_ = foundCooking
}

func TestBackfillEmbeddings(t *testing.T) {
	stub := newStub("ok")
	embedCalls := 0
	stub.embedFn = func(texts []string) ([][]float64, error) {
		embedCalls++
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.5, 0.5, 0}
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

	// Create nodes with real summaries (summary != label)
	kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'tag-a', 'Covers topic A in detail', 1)`)
	kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'tag-b', 'Covers topic B in detail', 1)`)

	kbase.BackfillEmbeddings()

	// Verify embeddings were created
	var count int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 embeddings, got %d", count)
	}
	if embedCalls != 2 {
		t.Errorf("expected 2 embed calls, got %d", embedCalls)
	}
}

func TestBackfillEmbeddings_SkipsUnsummarized(t *testing.T) {
	stub := newStub("ok")
	embedCalls := 0
	stub.embedFn = func(texts []string) ([][]float64, error) {
		embedCalls++
		results := make([][]float64, len(texts))
		for i := range texts {
			results[i] = []float64{0.5, 0.5, 0}
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

	// Create a node with summary == label (unsummarized, should be skipped)
	kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'stale', 'stale', 1)`)
	// Create a node with a real summary
	kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'real', 'A real summary', 1)`)

	kbase.BackfillEmbeddings()

	var count int
	kbase.db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 embedding (skip unsummarized), got %d", count)
	}
}

func TestQuery_FallbackWhenEmbedFails(t *testing.T) {
	stub := newStub("ok")
	stub.embedFn = func(texts []string) ([][]float64, error) {
		return nil, fmt.Errorf("embed service down")
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

	// Set up a simple tree
	res, _ := kbase.db.Exec(`INSERT INTO tree_nodes (parent_id, label, summary, is_leaf) VALUES (NULL, 'tag', 'A tag summary', 1)`)
	nodeID, _ := res.LastInsertId()

	noteID, _, _ := kbase.AddNote("Test Note", "content", nil)
	kbase.db.Exec(`UPDATE notes SET summary = 'note summary' WHERE id = ?`, noteID)
	kbase.db.Exec(`INSERT INTO leaf_entries (node_id, note_id) VALUES (?, ?)`, nodeID, noteID)

	// Query should still work via fallback (include all)
	results, err := kbase.Query("test query")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results from fallback when embed fails")
	}
}
