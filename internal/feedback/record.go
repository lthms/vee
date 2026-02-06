package feedback

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Record inserts a new feedback entry and returns its ID.
func (s *Store) Record(profile, kind, statement, scope, project string) (string, error) {
	id := newUUID()
	createdAt := time.Now().Format("2006-01-02")

	_, err := s.db.Exec(
		`INSERT INTO feedback (id, profile, kind, statement, scope, project, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, profile, kind, statement, scope, project, createdAt,
	)
	if err != nil {
		return "", fmt.Errorf("insert feedback: %w", err)
	}

	return id, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
