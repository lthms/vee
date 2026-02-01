package kb

import (
	"database/sql"
	"fmt"
)

func migrate(db *sql.DB) error {
	stmts := []string{
		// Atomic facts with inline embedding
		`CREATE TABLE IF NOT EXISTS statements (
			id            TEXT PRIMARY KEY,
			title         TEXT NOT NULL,
			content       TEXT NOT NULL,
			source        TEXT NOT NULL DEFAULT '',
			source_type   TEXT NOT NULL DEFAULT 'manual',
			status        TEXT NOT NULL DEFAULT 'active',
			embedding     BLOB,
			model         TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL,
			last_verified TEXT NOT NULL DEFAULT ''
		)`,

		// Consistency worlds
		`CREATE TABLE IF NOT EXISTS roots (
			id          TEXT PRIMARY KEY,
			description TEXT NOT NULL DEFAULT ''
		)`,

		// Statements excluded from specific roots
		`CREATE TABLE IF NOT EXISTS root_exclusions (
			root_id      TEXT NOT NULL REFERENCES roots(id),
			statement_id TEXT NOT NULL REFERENCES statements(id),
			reason       TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (root_id, statement_id)
		)`,

		// Detected contradictions between statement pairs
		`CREATE TABLE IF NOT EXISTS nogoods (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			stmt_a_id    TEXT NOT NULL REFERENCES statements(id),
			stmt_b_id    TEXT NOT NULL REFERENCES statements(id),
			explanation  TEXT NOT NULL DEFAULT '',
			detected_at  TEXT NOT NULL,
			UNIQUE(stmt_a_id, stmt_b_id)
		)`,

		// Groups of related statements
		`CREATE TABLE IF NOT EXISTS clusters (
			id        TEXT PRIMARY KEY,
			label     TEXT NOT NULL DEFAULT '',
			summary   TEXT NOT NULL DEFAULT '',
			embedding BLOB,
			model     TEXT NOT NULL DEFAULT ''
		)`,

		// Many-to-many: clusters <-> statements
		`CREATE TABLE IF NOT EXISTS cluster_members (
			cluster_id   TEXT NOT NULL REFERENCES clusters(id),
			statement_id TEXT NOT NULL REFERENCES statements(id),
			PRIMARY KEY (cluster_id, statement_id)
		)`,

		// Per-root cluster hierarchy (materialized path)
		`CREATE TABLE IF NOT EXISTS hierarchy (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			root_id    TEXT NOT NULL REFERENCES roots(id),
			cluster_id TEXT NOT NULL REFERENCES clusters(id),
			parent_id  INTEGER REFERENCES hierarchy(id),
			path       TEXT NOT NULL DEFAULT '',
			UNIQUE(root_id, cluster_id)
		)`,

		// Background task queue
		`CREATE TABLE IF NOT EXISTS processing_queue (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			task_type    TEXT NOT NULL,
			payload      TEXT NOT NULL DEFAULT '',
			priority     INTEGER NOT NULL DEFAULT 0,
			status       TEXT NOT NULL DEFAULT 'pending',
			attempts     INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 3,
			error        TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL,
			updated_at   TEXT NOT NULL
		)`,

		// Provenance tracking for all LLM calls
		`CREATE TABLE IF NOT EXISTS model_audit (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			model       TEXT NOT NULL,
			operation   TEXT NOT NULL,
			target_type TEXT NOT NULL DEFAULT '',
			target_id   TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL
		)`,

		// Insert default root if not present
		`INSERT OR IGNORE INTO roots (id, description) VALUES ('default', 'Default consistency world')`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", truncate(s, 60), err)
		}
	}

	// Drop old tables from previous schema (safe to ignore errors)
	dropStmts := []string{
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
