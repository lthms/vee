package kb

import (
	"database/sql"
	"fmt"
)

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS statements (
			id            TEXT PRIMARY KEY,
			content       TEXT NOT NULL,
			source        TEXT NOT NULL DEFAULT '',
			source_type   TEXT NOT NULL DEFAULT 'manual',
			status        TEXT NOT NULL DEFAULT 'active',
			embedding     BLOB,
			model         TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL,
			last_verified TEXT NOT NULL DEFAULT ''
		)`,

		`CREATE TABLE IF NOT EXISTS nogoods (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			stmt_a_id    TEXT NOT NULL REFERENCES statements(id),
			stmt_b_id    TEXT NOT NULL REFERENCES statements(id),
			explanation  TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL DEFAULT 'pending',
			detected_at  TEXT NOT NULL,
			UNIQUE(stmt_a_id, stmt_b_id)
		)`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", truncate(s, 60), err)
		}
	}

	// Drop title column from statements (added pre-v2, now redundant).
	db.Exec(`ALTER TABLE statements DROP COLUMN title`)

	// Add status column to nogoods (idempotent for existing DBs).
	db.Exec(`ALTER TABLE nogoods ADD COLUMN status TEXT NOT NULL DEFAULT 'pending'`)

	// Drop removed tables (safe on fresh DBs via IF EXISTS)
	dropStmts := []string{
		`DROP TABLE IF EXISTS root_exclusions`,
		`DROP TABLE IF EXISTS cluster_members`,
		`DROP TABLE IF EXISTS hierarchy`,
		`DROP TABLE IF EXISTS roots`,
		`DROP TABLE IF EXISTS clusters`,
		`DROP TABLE IF EXISTS processing_queue`,
		`DROP TABLE IF EXISTS model_audit`,
		// Legacy tables from older schemas
		`DROP TABLE IF EXISTS leaf_entries`,
		`DROP TABLE IF EXISTS node_embeddings`,
		`DROP TABLE IF EXISTS tree_nodes`,
		`DROP TABLE IF EXISTS notes`,
		`DROP TABLE IF EXISTS notes_fts`,
		`DROP TRIGGER IF EXISTS notes_ai`,
		`DROP TRIGGER IF EXISTS notes_ad`,
		`DROP TRIGGER IF EXISTS notes_au`,
	}
	for _, s := range dropStmts {
		db.Exec(s)
	}

	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
