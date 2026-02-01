package kb

import (
	"database/sql"
	"fmt"
)

func migrate(db *sql.DB) error {
	// Core tables
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notes (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			path       TEXT UNIQUE NOT NULL,
			title      TEXT NOT NULL,
			tags       TEXT NOT NULL DEFAULT '',
			summary    TEXT NOT NULL DEFAULT '',
			indexed    INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tree_nodes (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			parent_id INTEGER REFERENCES tree_nodes(id),
			label     TEXT NOT NULL,
			summary   TEXT NOT NULL,
			is_leaf   INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS leaf_entries (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES tree_nodes(id) ON DELETE CASCADE,
			note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
			UNIQUE(node_id, note_id)
		)`,
		`CREATE TABLE IF NOT EXISTS node_embeddings (
			node_id    INTEGER PRIMARY KEY,
			model      TEXT NOT NULL,
			embedding  BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", s[:40], err)
		}
	}

	// Migration: add columns if missing (for existing DBs)
	alterStmts := []string{
		`ALTER TABLE notes ADD COLUMN summary TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE notes ADD COLUMN indexed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE notes ADD COLUMN last_verified TEXT NOT NULL DEFAULT ''`,
	}
	for _, s := range alterStmts {
		db.Exec(s) // ignore errors (column already exists)
	}

	// Backfill: set last_verified to created_at for existing notes
	db.Exec(`UPDATE notes SET last_verified = created_at WHERE last_verified = ''`)

	// Drop FTS5 table and triggers if they exist (migrating from old schema)
	dropStmts := []string{
		`DROP TRIGGER IF EXISTS notes_ai`,
		`DROP TRIGGER IF EXISTS notes_ad`,
		`DROP TRIGGER IF EXISTS notes_au`,
		`DROP TABLE IF EXISTS notes_fts`,
	}
	for _, s := range dropStmts {
		db.Exec(s) // ignore errors
	}

	return nil
}
