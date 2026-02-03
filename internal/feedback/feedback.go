package feedback

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Entry represents a single feedback record.
type Entry struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	Kind      string `json:"kind"`
	Statement string `json:"statement"`
	Scope     string `json:"scope"`
	Project   string `json:"project"`
	CreatedAt string `json:"created_at"`
}

// Store provides persistent feedback storage backed by SQLite.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the feedback database at the given path.
func Open(dbPath string) (*Store, error) {
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open feedback db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate feedback db: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}
