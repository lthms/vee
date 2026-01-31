package main

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// App holds the shared application state passed to all subsystems.
type App struct {
	Sessions *sessionStore
	Control  *sessionControl
}

func newApp() *App {
	return &App{
		Sessions: newSessionStore(),
		Control:  newSessionControl(),
	}
}

// Session represents a Claude Code session (active or suspended).
type Session struct {
	ID        string    `json:"id"`
	Mode      string    `json:"mode"`
	Indicator string    `json:"indicator"`
	StartedAt time.Time `json:"started_at"`
	Preview   string    `json:"preview"`
	Status    string    `json:"status"` // "active" or "suspended"
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

func (s *sessionStore) create(id, mode, indicator, preview string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &Session{
		ID:        id,
		Mode:      mode,
		Indicator: indicator,
		StartedAt: time.Now(),
		Preview:   preview,
		Status:    "active",
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

// active returns the currently active session, if any.
func (s *sessionStore) active() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.Status == "active" {
			return sess
		}
	}
	return nil
}

// sessionControl manages suspend and self-drop signaling for the active session.
type sessionControl struct {
	mu         sync.Mutex
	suspendCh  chan struct{}
	selfDropCh chan struct{}
}

func newSessionControl() *sessionControl {
	return &sessionControl{}
}

// newSession creates buffered(1) channels for the active session and returns them.
func (c *sessionControl) newSession() (suspend, selfDrop chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suspendCh = make(chan struct{}, 1)
	c.selfDropCh = make(chan struct{}, 1)
	return c.suspendCh, c.selfDropCh
}

// requestSuspend sends on the suspend channel (non-blocking). Returns true if sent.
func (c *sessionControl) requestSuspend() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.suspendCh == nil {
		return false
	}
	select {
	case c.suspendCh <- struct{}{}:
		return true
	default:
		return false // already requested
	}
}

// requestSelfDrop sends on the self-drop channel (non-blocking). Returns true if sent.
func (c *sessionControl) requestSelfDrop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selfDropCh == nil {
		return false
	}
	select {
	case c.selfDropCh <- struct{}{}:
		return true
	default:
		return false
	}
}

// clearSession nils out both channels.
func (c *sessionControl) clearSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suspendCh = nil
	c.selfDropCh = nil
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
