package main

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// AppConfig stores configuration that _new-pane fetches via /api/config.
type AppConfig struct {
	VeePath       string   `json:"vee_path"`
	Port          int      `json:"port"`
	Passthrough   []string `json:"passthrough"`
	ProjectConfig string   `json:"project_config"`
}

// IndexingTask represents a background note indexing operation.
type IndexingTask struct {
	NoteID    int       `json:"note_id"`
	Title     string    `json:"title"`
	StartedAt time.Time `json:"started_at"`
}

// indexingStore is a thread-safe store for active indexing tasks.
type indexingStore struct {
	mu    sync.RWMutex
	tasks map[int]*IndexingTask
}

func newIndexingStore() *indexingStore {
	return &indexingStore{
		tasks: make(map[int]*IndexingTask),
	}
}

func (s *indexingStore) add(noteID int, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[noteID] = &IndexingTask{
		NoteID:    noteID,
		Title:     title,
		StartedAt: time.Now(),
	}
}

func (s *indexingStore) remove(noteID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, noteID)
}

func (s *indexingStore) list() []IndexingTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]IndexingTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		result = append(result, *t)
	}
	// Sort by StartedAt ascending
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].StartedAt.Before(result[j-1].StartedAt); j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

func (s *indexingStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tasks)
}

// App holds the shared application state passed to all subsystems.
type App struct {
	Sessions *sessionStore
	Indexing  *indexingStore

	mu     sync.RWMutex
	config *AppConfig
}

func newApp() *App {
	return &App{
		Sessions: newSessionStore(),
		Indexing:  newIndexingStore(),
	}
}

// SetConfig stores the project configuration for retrieval by _new-pane.
func (a *App) SetConfig(cfg *AppConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config = cfg
}

// Config returns the stored project configuration.
func (a *App) Config() *AppConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config
}

// Session represents a Claude Code session (active or suspended).
type Session struct {
	ID              string    `json:"id"`
	Mode            string    `json:"mode"`
	Indicator       string    `json:"indicator"`
	StartedAt       time.Time `json:"started_at"`
	Preview         string    `json:"preview"`
	Status          string    `json:"status"`          // "active", "suspended", or "completed"
	WindowTarget    string    `json:"window_target"`   // tmux window ID (e.g. "@3")
	Ephemeral       bool      `json:"ephemeral"`
	KBIngest        bool      `json:"kb_ingest"`
	Working         bool      `json:"working"`
	HasNotification bool      `json:"has_notification"`
	PermissionMode  string    `json:"permission_mode"`
}

// sessionStore is an in-memory store of sessions keyed by ID.
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		sessions: make(map[string]*Session),
	}
}

func (s *sessionStore) create(id, mode, indicator, preview, windowTarget string, ephemeral, kbIngest bool) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &Session{
		ID:           id,
		Mode:         mode,
		Indicator:    indicator,
		StartedAt:    time.Now(),
		Preview:      preview,
		Status:       "active",
		WindowTarget: windowTarget,
		Ephemeral:    ephemeral,
		KBIngest:     kbIngest,
	}
	s.sessions[id] = sess
	return sess
}

func (s *sessionStore) get(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

func (s *sessionStore) setStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.Status = status
	}
}

func (s *sessionStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// suspended returns all sessions with status "suspended", ordered by start time.
func (s *sessionStore) suspended() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Session
	for _, sess := range s.sessions {
		if sess.Status == "suspended" {
			result = append(result, sess)
		}
	}
	// Sort by StartedAt ascending
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].StartedAt.Before(result[j-1].StartedAt); j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

// completed returns all sessions with status "completed", ordered by start time.
func (s *sessionStore) completed() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Session
	for _, sess := range s.sessions {
		if sess.Status == "completed" {
			result = append(result, sess)
		}
	}
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].StartedAt.Before(result[j-1].StartedAt); j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

// active returns all currently active sessions.
func (s *sessionStore) active() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Session
	for _, sess := range s.sessions {
		if sess.Status == "active" {
			result = append(result, sess)
		}
	}
	return result
}

// findByWindowTarget returns the first active session matching a tmux window ID.
func (s *sessionStore) findByWindowTarget(target string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.WindowTarget == target {
			return sess
		}
	}
	return nil
}

// setWindowState updates the dynamic window state fields for a session.
// Pointer bools so callers only update the fields they care about.
func (s *sessionStore) setWindowState(id string, working, notif *bool, permMode, preview string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return
	}
	if working != nil {
		sess.Working = *working
	}
	if notif != nil {
		sess.HasNotification = *notif
	}
	if permMode != "" {
		sess.PermissionMode = permMode
	}
	if preview != "" {
		sess.Preview = preview
	}
}

// setPreview updates the preview text for a session.
func (s *sessionStore) setPreview(id, preview string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.Preview = preview
	}
}

// setWindowTarget updates the tmux window ID for a session.
func (s *sessionStore) setWindowTarget(id, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.WindowTarget = target
	}
}

// newUUID generates a v4 UUID using crypto/rand.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 2
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
