package kb

import (
	"fmt"
	"log/slog"
	"time"
)

// QueueTask represents a row from the processing_queue table.
type QueueTask struct {
	ID          int
	TaskType    string
	Payload     string
	Priority    int
	Status      string
	Attempts    int
	MaxAttempts int
	Error       string
	CreatedAt   string
	UpdatedAt   string
}

// Enqueue adds a task to the processing queue.
func (kb *KnowledgeBase) Enqueue(taskType, payload string, priority int) {
	now := time.Now().Format("2006-01-02T15:04:05Z")
	_, err := kb.db.Exec(
		`INSERT INTO processing_queue (task_type, payload, priority, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'pending', ?, ?)`,
		taskType, payload, priority, now, now,
	)
	if err != nil {
		// Non-fatal: log but don't propagate
		fmt.Printf("kb: enqueue %s failed: %v\n", taskType, err)
	}
}

// Dequeue retrieves and claims the highest-priority pending task of the given type.
// Returns nil if no tasks are available.
func (kb *KnowledgeBase) Dequeue(taskType string) (*QueueTask, error) {
	now := time.Now().Format("2006-01-02T15:04:05Z")

	tx, err := kb.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var t QueueTask
	err = tx.QueryRow(
		`SELECT id, task_type, payload, priority, status, attempts, max_attempts, error, created_at, updated_at
		 FROM processing_queue
		 WHERE task_type = ? AND status = 'pending' AND attempts < max_attempts
		 ORDER BY priority DESC, id ASC
		 LIMIT 1`,
		taskType,
	).Scan(&t.ID, &t.TaskType, &t.Payload, &t.Priority, &t.Status, &t.Attempts, &t.MaxAttempts, &t.Error, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, nil // no rows = no tasks
	}

	_, err = tx.Exec(
		`UPDATE processing_queue SET status = 'processing', attempts = attempts + 1, updated_at = ? WHERE id = ?`,
		now, t.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("claim task: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	t.Status = "processing"
	t.Attempts++
	return &t, nil
}

// CompleteTask marks a task as completed.
func (kb *KnowledgeBase) CompleteTask(id int) {
	now := time.Now().Format("2006-01-02T15:04:05Z")
	kb.db.Exec(`UPDATE processing_queue SET status = 'completed', updated_at = ? WHERE id = ?`, now, id)
}

// FailTask marks a task as failed with an error message.
// If attempts < max_attempts, the task returns to pending for retry.
func (kb *KnowledgeBase) FailTask(id int, taskErr error) {
	now := time.Now().Format("2006-01-02T15:04:05Z")
	errMsg := ""
	if taskErr != nil {
		errMsg = taskErr.Error()
	}

	// Check if retryable
	var attempts, maxAttempts int
	kb.db.QueryRow(`SELECT attempts, max_attempts FROM processing_queue WHERE id = ?`, id).Scan(&attempts, &maxAttempts)

	newStatus := "failed"
	if attempts < maxAttempts {
		newStatus = "pending"
	}

	kb.db.Exec(
		`UPDATE processing_queue SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		newStatus, errMsg, now, id,
	)
}

// RecoverStaleTasks resets any tasks left in 'processing' status back to 'pending'.
// This handles the case where workers were killed mid-flight (e.g. daemon crash).
func (kb *KnowledgeBase) RecoverStaleTasks() int {
	result, err := kb.db.Exec(
		`UPDATE processing_queue SET status = 'pending' WHERE status = 'processing'`,
	)
	if err != nil {
		slog.Warn("kb: failed to recover stale tasks", "error", err)
		return 0
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		slog.Info("kb: recovered stale tasks", "count", n)
	}
	return int(n)
}

// ProcessingTasks returns all tasks currently in 'processing' status.
func (kb *KnowledgeBase) ProcessingTasks() []QueueTask {
	rows, err := kb.db.Query(
		`SELECT id, task_type, payload, priority, status, attempts, max_attempts, error, created_at, updated_at
		 FROM processing_queue
		 WHERE status = 'processing'
		 ORDER BY id ASC`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var tasks []QueueTask
	for rows.Next() {
		var t QueueTask
		if err := rows.Scan(&t.ID, &t.TaskType, &t.Payload, &t.Priority, &t.Status, &t.Attempts, &t.MaxAttempts, &t.Error, &t.CreatedAt, &t.UpdatedAt); err != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks
}
