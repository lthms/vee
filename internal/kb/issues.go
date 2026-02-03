package kb

import (
	"fmt"
	"log/slog"
	"time"
)

// Issue represents a detected issue between two statements.
type Issue struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	StatementA string `json:"statement_a"`
	StatementB string `json:"statement_b"`
	Score      float64 `json:"score"`
	CreatedAt  string `json:"created_at"`
	ResolvedAt string `json:"resolved_at"`

	// Inlined statement content for display
	ContentA string `json:"content_a,omitempty"`
	ContentB string `json:"content_b,omitempty"`
	SourceA  string `json:"source_a,omitempty"`
	SourceB  string `json:"source_b,omitempty"`
}

// ListOpenIssues returns all open issues with both statements' content inlined.
func (kb *KnowledgeBase) ListOpenIssues() ([]Issue, error) {
	rows, err := kb.db.Query(
		`SELECT i.id, i.type, i.status, i.statement_a, i.statement_b, i.score, i.created_at,
		        COALESCE(sa.content, '[deleted]'), COALESCE(sb.content, '[deleted]'),
		        COALESCE(sa.source, ''), COALESCE(sb.source, '')
		 FROM issues i
		 LEFT JOIN statements sa ON sa.id = i.statement_a
		 LEFT JOIN statements sb ON sb.id = i.statement_b
		 WHERE i.status = 'open'
		 ORDER BY i.score DESC, i.created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list open issues: %w", err)
	}
	defer rows.Close()

	var issues []Issue
	for rows.Next() {
		var iss Issue
		if err := rows.Scan(
			&iss.ID, &iss.Type, &iss.Status, &iss.StatementA, &iss.StatementB,
			&iss.Score, &iss.CreatedAt, &iss.ContentA, &iss.ContentB,
			&iss.SourceA, &iss.SourceB,
		); err != nil {
			slog.Warn("list issues: scan row", "error", err)
			continue
		}
		issues = append(issues, iss)
	}
	return issues, rows.Err()
}

// OpenIssueCount returns the number of open issues.
func (kb *KnowledgeBase) OpenIssueCount() (int, error) {
	var count int
	err := kb.db.QueryRow(`SELECT COUNT(*) FROM issues WHERE status = 'open'`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("open issue count: %w", err)
	}
	return count, nil
}

// ResolveIssue resolves an issue with the given action.
// Valid actions: keep_a, keep_b, keep_both, delete_both.
func (kb *KnowledgeBase) ResolveIssue(issueID, action string) error {
	// Fetch the issue
	var stmtA, stmtB string
	var status string
	err := kb.db.QueryRow(
		`SELECT status, statement_a, statement_b FROM issues WHERE id = ?`, issueID,
	).Scan(&status, &stmtA, &stmtB)
	if err != nil {
		return fmt.Errorf("resolve issue: fetch: %w", err)
	}
	if status != "open" {
		return fmt.Errorf("issue %s is not open (status: %s)", issueID, status)
	}

	now := time.Now().Format("2006-01-02T15:04:05Z")

	switch action {
	case "keep_a":
		if err := kb.DeleteStatement(stmtB); err != nil {
			slog.Warn("resolve: failed to delete statement B", "id", stmtB, "error", err)
		}
		if err := kb.PromoteStatement(stmtA); err != nil {
			slog.Warn("resolve: failed to promote statement A", "id", stmtA, "error", err)
		}
		kb.cascadeCloseIssues(stmtB)

	case "keep_b":
		if err := kb.DeleteStatement(stmtA); err != nil {
			slog.Warn("resolve: failed to delete statement A", "id", stmtA, "error", err)
		}
		if err := kb.PromoteStatement(stmtB); err != nil {
			slog.Warn("resolve: failed to promote statement B", "id", stmtB, "error", err)
		}
		kb.cascadeCloseIssues(stmtA)

	case "keep_both":
		if err := kb.PromoteStatement(stmtA); err != nil {
			slog.Warn("resolve: failed to promote statement A", "id", stmtA, "error", err)
		}
		if err := kb.PromoteStatement(stmtB); err != nil {
			slog.Warn("resolve: failed to promote statement B", "id", stmtB, "error", err)
		}

	case "delete_both":
		if err := kb.DeleteStatement(stmtA); err != nil {
			slog.Warn("resolve: failed to delete statement A", "id", stmtA, "error", err)
		}
		if err := kb.DeleteStatement(stmtB); err != nil {
			slog.Warn("resolve: failed to delete statement B", "id", stmtB, "error", err)
		}
		kb.cascadeCloseIssues(stmtA)
		kb.cascadeCloseIssues(stmtB)

	default:
		return fmt.Errorf("unknown action: %s", action)
	}

	// Close this issue
	_, err = kb.db.Exec(
		`UPDATE issues SET status = 'resolved', resolved_at = ? WHERE id = ?`,
		now, issueID,
	)
	if err != nil {
		return fmt.Errorf("resolve issue: close: %w", err)
	}

	slog.Info("issue resolved", "id", issueID, "action", action)
	return nil
}

// cascadeCloseIssues closes all open issues that reference a deleted statement.
func (kb *KnowledgeBase) cascadeCloseIssues(deletedStmtID string) {
	now := time.Now().Format("2006-01-02T15:04:05Z")
	result, err := kb.db.Exec(
		`UPDATE issues SET status = 'resolved', resolved_at = ?
		 WHERE status = 'open' AND (statement_a = ? OR statement_b = ?)`,
		now, deletedStmtID, deletedStmtID,
	)
	if err != nil {
		slog.Warn("cascade close: failed", "statement", deletedStmtID, "error", err)
		return
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		slog.Info("cascade closed stale issues", "statement", deletedStmtID, "count", n)
	}
}
