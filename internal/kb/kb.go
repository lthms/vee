package kb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Model abstracts the LLM backend used for judgment calls and embeddings.
type Model interface {
	Generate(prompt string) (string, error)
	Embed(texts []string) ([][]float64, error)
}

// Config holds KB initialization parameters.
type Config struct {
	DBPath         string  // path to SQLite file
	Model          Model   // LLM backend for embeddings/judgment
	EmbeddingModel string  // model name stored alongside embeddings for stale detection
	Threshold      float64 // minimum cosine similarity to include (0 = default 0.3)
	MaxResults     int     // max query results returned (0 = default 10)
}

// KnowledgeBase provides persistent statement storage backed by SQLite
// with brute-force KNN search and background clustering.
type KnowledgeBase struct {
	db             *sql.DB
	model          Model
	embeddingModel string
	threshold      float64
	maxResults     int

	// Worker lifecycle
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// QueryResult is a single search hit from KNN search.
type QueryResult struct {
	ID           string  `json:"id"`
	Content      string  `json:"content"`
	Source       string  `json:"source"`
	Score        float64 `json:"score"`
	LastVerified string  `json:"last_verified"`
}

// Open opens (or creates) the knowledge base at the configured path.
func Open(cfg Config) (*KnowledgeBase, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("kb: Model must not be nil")
	}

	dsn := cfg.DBPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	threshold := cfg.Threshold
	if threshold == 0 {
		threshold = 0.3
	}
	maxResults := cfg.MaxResults
	if maxResults == 0 {
		maxResults = 10
	}

	return &KnowledgeBase{
		db:             db,
		model:          cfg.Model,
		embeddingModel: cfg.EmbeddingModel,
		threshold:      threshold,
		maxResults:     maxResults,
		stopCh:         make(chan struct{}),
	}, nil
}

// Close stops background workers and closes the database.
// Workers get 5 seconds to finish; after that, the DB is closed regardless.
func (kb *KnowledgeBase) Close() error {
	close(kb.stopCh)
	done := make(chan struct{})
	go func() { kb.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Warn("kb: workers did not stop in time, forcing close")
	}
	return kb.db.Close()
}

// CallModel exposes the underlying model for callers that need
// judgment calls outside the KB package (e.g. ingest evaluation).
func (kb *KnowledgeBase) CallModel(prompt string) (string, error) {
	return kb.model.Generate(prompt)
}

// QueryResultsJSON marshals query results to JSON text suitable for MCP responses.
func QueryResultsJSON(results []QueryResult) string {
	if results == nil {
		results = []QueryResult{}
	}
	out, _ := json.Marshal(results)
	return string(out)
}
