package feedback

import (
	"database/sql"
	"fmt"
)

func migrate(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS feedback (
		id         TEXT PRIMARY KEY,
		profile    TEXT NOT NULL,
		kind       TEXT NOT NULL CHECK(kind IN ('good','bad')),
		statement  TEXT NOT NULL,
		scope      TEXT NOT NULL CHECK(scope IN ('user','project')),
		project    TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create feedback table: %w", err)
	}

	// Migrate legacy "mode" column to "profile" if the old schema exists.
	var colName string
	row := db.QueryRow(`SELECT name FROM pragma_table_info('feedback') WHERE name = 'mode'`)
	if err := row.Scan(&colName); err == nil {
		_, err := db.Exec(`ALTER TABLE feedback RENAME COLUMN mode TO profile`)
		if err != nil {
			return fmt.Errorf("rename mode→profile column: %w", err)
		}
	}

	return nil
}
