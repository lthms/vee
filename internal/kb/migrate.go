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
			status        TEXT NOT NULL DEFAULT 'pending',
			embedding     BLOB,
			model         TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL,
			last_verified TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS issues (
			id          TEXT PRIMARY KEY,
			type        TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'open',
			statement_a TEXT NOT NULL,
			statement_b TEXT NOT NULL,
			score       REAL NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL,
			resolved_at TEXT NOT NULL DEFAULT ''
		)`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", truncate(s, 60), err)
		}
	}

	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
