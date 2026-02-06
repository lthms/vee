package feedback

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertWithDate(t *testing.T, s *Store, profile, kind, statement, scope, project, date string) {
	t.Helper()
	id := newUUID()
	_, err := s.db.Exec(
		`INSERT INTO feedback (id, profile, kind, statement, scope, project, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, profile, kind, statement, scope, project, date,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSampleReturnsAllWhenUnderLimit(t *testing.T) {
	s := openTestStore(t)

	s.Record("vibe", "good", "Be concise", "user", "")
	s.Record("vibe", "bad", "No emojis", "user", "")

	entries, err := s.Sample("vibe", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestSampleRespectsMaxCount(t *testing.T) {
	s := openTestStore(t)

	for i := range 10 {
		_ = i
		s.Record("vibe", "good", "Statement", "user", "")
	}

	entries, err := s.Sample("vibe", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
}

func TestSampleFiltersMode(t *testing.T) {
	s := openTestStore(t)

	s.Record("vibe", "good", "Vibe feedback", "user", "")
	s.Record("normal", "good", "Normal feedback", "user", "")

	entries, err := s.Sample("vibe", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Statement != "Vibe feedback" {
		t.Fatalf("unexpected statement: %s", entries[0].Statement)
	}
}

func TestSampleScopeFiltering(t *testing.T) {
	s := openTestStore(t)

	s.Record("vibe", "good", "User-scoped", "user", "")
	s.Record("vibe", "good", "Project-scoped matching", "project", "/my/project")
	s.Record("vibe", "good", "Project-scoped other", "project", "/other/project")

	entries, err := s.Sample("vibe", "/my/project", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (user + matching project), got %d", len(entries))
	}

	// Verify the "other project" entry is not included
	for _, e := range entries {
		if e.Statement == "Project-scoped other" {
			t.Fatal("should not include feedback from other project")
		}
	}
}

func TestSampleRecencyBias(t *testing.T) {
	s := openTestStore(t)

	// Insert one old entry and many recent entries
	today := time.Now().Format("2006-01-02")
	old := time.Now().AddDate(-1, 0, 0).Format("2006-01-02")

	insertWithDate(t, s, "vibe", "good", "Old statement", "user", "", old)
	for range 9 {
		insertWithDate(t, s, "vibe", "good", "Recent statement", "user", "", today)
	}

	// Sample 5 out of 10 — recent entries should dominate
	recentCount := 0
	iterations := 100
	for range iterations {
		entries, err := s.Sample("vibe", "", 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Statement == "Recent statement" {
				recentCount++
			}
		}
	}

	// With strong recency bias, recent entries should make up the vast majority
	totalSampled := iterations * 5
	recentRatio := float64(recentCount) / float64(totalSampled)
	if recentRatio < 0.85 {
		t.Fatalf("expected recent entries to dominate (>85%%), got %.1f%%", recentRatio*100)
	}
}

func TestRecord(t *testing.T) {
	s := openTestStore(t)

	id, err := s.Record("vibe", "good", "Test statement", "user", "")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	// Verify it was stored
	entries, err := s.Sample("vibe", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Kind != "good" {
		t.Fatalf("expected kind=good, got %s", entries[0].Kind)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
