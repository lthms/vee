package feedback

import (
	"math"
	"math/rand"
	"time"
)

// Sample returns up to n feedback entries for the given profile, with recency bias.
// Entries are selected where profile matches AND (scope='user' OR (scope='project' AND project matches)).
func (s *Store) Sample(profile, project string, n int) ([]Entry, error) {
	rows, err := s.db.Query(
		`SELECT id, profile, kind, statement, scope, project, created_at
		 FROM feedback
		 WHERE profile = ?
		   AND (scope = 'user' OR (scope = 'project' AND project = ?))`,
		profile, project,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Profile, &e.Kind, &e.Statement, &e.Scope, &e.Project, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(entries) <= n {
		return entries, nil
	}

	return weightedSample(entries, n), nil
}

// weightedSample selects n entries using weighted random sampling with
// recency bias: weight = 1.0 / (1.0 + days_since_creation).
func weightedSample(entries []Entry, n int) []Entry {
	now := time.Now()
	weights := make([]float64, len(entries))
	for i, e := range entries {
		created, err := time.Parse("2006-01-02", e.CreatedAt)
		if err != nil {
			weights[i] = 0.1
			continue
		}
		days := math.Max(0, now.Sub(created).Hours()/24)
		weights[i] = 1.0 / (1.0 + days)
	}

	selected := make([]Entry, 0, n)
	used := make([]bool, len(entries))

	for range n {
		// Compute total weight of remaining entries
		var total float64
		for i, w := range weights {
			if !used[i] {
				total += w
			}
		}
		if total == 0 {
			break
		}

		// Pick a random point
		r := rand.Float64() * total
		var cumulative float64
		for i, w := range weights {
			if used[i] {
				continue
			}
			cumulative += w
			if cumulative >= r {
				selected = append(selected, entries[i])
				used[i] = true
				break
			}
		}
	}

	return selected
}
