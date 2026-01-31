package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ringBuffer stores the last N lines of log output.
type ringBuffer struct {
	mu    sync.RWMutex
	lines []string
	cap   int
	count int // total lines ever written (for change detection)
}

func newRingBuffer(cap int) *ringBuffer {
	return &ringBuffer{
		lines: make([]string, 0, cap),
		cap:   cap,
	}
}

func (b *ringBuffer) Write(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) < b.cap {
		b.lines = append(b.lines, line)
	} else {
		b.lines = append(b.lines[1:], line)
	}
	b.count++
}

func (b *ringBuffer) Lines() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

func (b *ringBuffer) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

// slogRingHandler implements slog.Handler, writing formatted entries to a ring buffer.
type slogRingHandler struct {
	buf   *ringBuffer
	level slog.Level
}

func newSlogRingHandler(buf *ringBuffer, level slog.Level) *slogRingHandler {
	return &slogRingHandler{buf: buf, level: level}
}

func (h *slogRingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *slogRingHandler) Handle(_ context.Context, r slog.Record) error {
	ts := r.Time.Format(time.TimeOnly)
	line := fmt.Sprintf("%s %s %s", ts, r.Level.String(), r.Message)
	r.Attrs(func(a slog.Attr) bool {
		line += fmt.Sprintf(" %s=%v", a.Key, a.Value)
		return true
	})
	h.buf.Write(line)
	return nil
}

func (h *slogRingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *slogRingHandler) WithGroup(name string) slog.Handler {
	return h
}
