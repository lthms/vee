package kb

import (
	"encoding/binary"
	"fmt"
	"math"
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

// embedText computes an embedding for a single text string using the model.
func (kb *KnowledgeBase) embedText(text string) ([]float64, error) {
	embeddings, err := kb.model.Embed([]string{text})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(embeddings) == 0 {
		return nil, fmt.Errorf("embed: empty result")
	}
	return embeddings[0], nil
}
