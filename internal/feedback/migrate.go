package feedback

import (
	"database/sql"
	"fmt"
)

func migrate(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS feedback (
		id         TEXT PRIMARY KEY,
		mode       TEXT NOT NULL,
		kind       TEXT NOT NULL CHECK(kind IN ('good','bad')),
		statement  TEXT NOT NULL,
		scope      TEXT NOT NULL CHECK(scope IN ('user','project')),
		project    TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create feedback table: %w", err)
	}
	return nil
}
