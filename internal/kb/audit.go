package kb

import (
	"time"
)

// LogModelCall records an LLM invocation for provenance tracking.
func (kb *KnowledgeBase) LogModelCall(model, operation, targetType, targetID string, durationMs int) {
	now := time.Now().Format("2006-01-02T15:04:05Z")
	kb.db.Exec(
		`INSERT INTO model_audit (model, operation, target_type, target_id, duration_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		model, operation, targetType, targetID, durationMs, now,
	)
}
