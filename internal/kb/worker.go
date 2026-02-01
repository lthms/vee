package kb

import (
	"log/slog"
	"time"
)

// StartWorkers launches background worker goroutines that poll the processing queue.
// Workers respect kb.stopCh for graceful shutdown.
func (kb *KnowledgeBase) StartWorkers() {
	taskTypes := []struct {
		name     string
		handler  func(payload string) error
		interval time.Duration
	}{
		{"cluster_assign", kb.handleClusterAssign, 2 * time.Second},
		{"contradiction_check", kb.handleContradictionCheck, 5 * time.Second},
	}

	for _, tt := range taskTypes {
		tt := tt
		kb.wg.Add(1)
		go func() {
			defer kb.wg.Done()
			kb.workerLoop(tt.name, tt.handler, tt.interval)
		}()
	}

	slog.Info("kb: workers started", "count", len(taskTypes))
}

func (kb *KnowledgeBase) workerLoop(taskType string, handler func(string) error, pollInterval time.Duration) {
	for {
		select {
		case <-kb.stopCh:
			slog.Debug("kb: worker stopping", "type", taskType)
			return
		default:
		}

		task, err := kb.Dequeue(taskType)
		if err != nil {
			slog.Warn("kb: dequeue error", "type", taskType, "error", err)
			kb.sleep(pollInterval)
			continue
		}

		if task == nil {
			// No tasks available, wait before polling again
			kb.sleep(pollInterval)
			continue
		}

		slog.Debug("kb: processing task", "type", taskType, "id", task.ID, "payload", task.Payload)

		if err := handler(task.Payload); err != nil {
			slog.Warn("kb: task failed", "type", taskType, "id", task.ID, "error", err)
			kb.FailTask(task.ID, err)
		} else {
			kb.CompleteTask(task.ID)
		}
	}
}

// sleep waits for the given duration or until stopCh is closed.
func (kb *KnowledgeBase) sleep(d time.Duration) {
	select {
	case <-kb.stopCh:
	case <-time.After(d):
	}
}

func (kb *KnowledgeBase) handleClusterAssign(payload string) error {
	return kb.AssignToCluster(payload)
}

func (kb *KnowledgeBase) handleContradictionCheck(payload string) error {
	return kb.DetectContradictions(payload)
}
