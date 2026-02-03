package kb

import (
	"math"
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

func TestEmbedText(t *testing.T) {
	stub := newStub()
	stub.embedFn = func(texts []string) ([][]float64, error) {
		return [][]float64{{0.1, 0.2, 0.3}}, nil
	}
	kbase := openTestKB(t, stub)

	emb, err := kbase.embedText("hello world")
	if err != nil {
		t.Fatalf("embedText: %v", err)
	}
	if len(emb) != 3 {
		t.Errorf("expected 3-dimensional embedding, got %d", len(emb))
	}
}
