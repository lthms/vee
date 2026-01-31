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
	Zettelkasten  bool     `json:"zettelkasten"`
	Passthrough   []string `json:"passthrough"`
	ProjectConfig string   `json:"project_config"`
}

// App holds the shared application state passed to all subsystems.
type App struct {
	Sessions *sessionStore

	mu     sync.RWMutex
	config *AppConfig
}

func newApp() *App {
	return &App{
		Sessions: newSessionStore(),
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
	ID           string    `json:"id"`
	Mode         string    `json:"mode"`
	Indicator    string    `json:"indicator"`
	StartedAt    time.Time `json:"started_at"`
	Preview      string    `json:"preview"`
	Status       string    `json:"status"`        // "active", "suspended", or "completed"
	WindowTarget string    `json:"window_target"` // tmux window ID (e.g. "@3")
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

func (s *sessionStore) create(id, mode, indicator, preview, windowTarget string) *Session {
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
