package kb

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

// Model abstracts the embedding backend used by the knowledge base.
type Model interface {
	Embed(texts []string) ([][]float64, error)
}

// Config holds KB initialization parameters.
type Config struct {
	DBPath         string  // path to SQLite file
	Model          Model   // embedding backend
	EmbeddingModel string  // model name stored alongside embeddings for stale detection
	Threshold      float64 // minimum cosine similarity to include (0 = default 0.3)
	MaxResults     int     // max query results returned (0 = default 10)
	DupThreshold   float64 // cosine similarity above which a pair is flagged as duplicate (0 = default 0.85)
}

// KnowledgeBase provides persistent statement storage backed by SQLite
// with brute-force KNN search and async duplicate detection.
type KnowledgeBase struct {
	db             *sql.DB
	model          Model
	embeddingModel string
	threshold      float64
	maxResults     int
	dupThreshold   float64
	notifyCh       chan struct{} // signals the worker that a new statement was inserted
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
	dupThreshold := cfg.DupThreshold
	if dupThreshold == 0 {
		dupThreshold = 0.85
	}

	return &KnowledgeBase{
		db:             db,
		model:          cfg.Model,
		embeddingModel: cfg.EmbeddingModel,
		threshold:      threshold,
		maxResults:     maxResults,
		dupThreshold:   dupThreshold,
		notifyCh:       make(chan struct{}, 1),
	}, nil
}

// NotifyCh returns the channel that signals new pending statements.
func (kb *KnowledgeBase) NotifyCh() <-chan struct{} {
	return kb.notifyCh
}

// Close closes the database.
func (kb *KnowledgeBase) Close() error {
	return kb.db.Close()
}

// QueryResultsJSON marshals query results to JSON text suitable for MCP responses.
func QueryResultsJSON(results []QueryResult) string {
	if results == nil {
		results = []QueryResult{}
	}
	out, _ := json.Marshal(results)
	return string(out)
}
