package kb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

const (
	maxLeafSize     = 20
	maxNodeChildren = 10
)

// Model abstracts the LLM backend used for judgment calls and embeddings.
type Model interface {
	Generate(prompt string) (string, error)
	Embed(texts []string) ([][]float64, error)
}

// Config holds KB initialization parameters.
type Config struct {
	DBPath         string // path to SQLite file
	VaultDir       string // path to markdown vault directory
	Model          Model  // LLM backend for indexing/judgment/embeddings
	EmbeddingModel string // model name for stale detection in DB (e.g. "nomic-embed-text")
}

// KnowledgeBase provides persistent note storage backed by markdown files
// and a SQLite tree-based semantic index for retrieval.
type KnowledgeBase struct {
	db             *sql.DB
	vaultDir       string
	model          Model
	embeddingModel string // stored in node_embeddings.model for stale detection
}

// QueryResult is a single search hit from the tree index.
type QueryResult struct {
	Path         string `json:"path"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	LastVerified string `json:"last_verified"`
}

// NoteInfo is minimal note metadata (for touch/lookup).
type NoteInfo struct {
	ID    int
	Path  string
	Title string
}

// Open opens (or creates) the knowledge base at the configured paths.
// The vault directory and SQLite database are created on first use.
func Open(cfg Config) (*KnowledgeBase, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("kb: Model must not be nil")
	}

	if err := os.MkdirAll(cfg.VaultDir, 0700); err != nil {
		return nil, fmt.Errorf("create vault dir: %w", err)
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

	return &KnowledgeBase{
		db:             db,
		vaultDir:       cfg.VaultDir,
		model:          cfg.Model,
		embeddingModel: cfg.EmbeddingModel,
	}, nil
}

// Close closes the underlying database connection.
func (kb *KnowledgeBase) Close() error {
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
