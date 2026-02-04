package main

import (
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const defaultSSEKeepaliveInterval = 30 * time.Second

// keepaliveResponseWriter wraps an http.ResponseWriter to send periodic
// SSE comment lines (": keepalive\n\n") that prevent the connection from
// going idle and being silently dropped by the OS TCP stack or by the
// MCP client.
//
// All writes are serialized through a mutex so that keepalive comments
// never interleave with real SSE events written by the go-sdk.
type keepaliveResponseWriter struct {
	http.ResponseWriter
	interval time.Duration
	mu       sync.Mutex
	done     chan struct{}
}

func newKeepaliveResponseWriter(w http.ResponseWriter, interval time.Duration) *keepaliveResponseWriter {
	return &keepaliveResponseWriter{
		ResponseWriter: w,
		interval:       interval,
		done:           make(chan struct{}),
	}
}

// Write serializes writes to the underlying ResponseWriter.
func (w *keepaliveResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

// Flush implements http.Flusher.
func (w *keepaliveResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// startKeepalive spawns a goroutine that writes SSE comment lines
// periodically. The goroutine stops when stop() is called or when a
// write fails (indicating a broken connection).
func (w *keepaliveResponseWriter) startKeepalive() {
	go func() {
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				w.mu.Lock()
				_, err := w.ResponseWriter.Write([]byte(": keepalive\n\n"))
				if err == nil {
					if f, ok := w.ResponseWriter.(http.Flusher); ok {
						f.Flush()
					}
				}
				w.mu.Unlock()
				if err != nil {
					slog.Debug("sse keepalive write failed", "error", err)
					return
				}
			}
		}
	}()
}

func (w *keepaliveResponseWriter) stop() {
	close(w.done)
}

// sseWithKeepalive wraps an SSE handler to inject keepalive comments for
// GET requests (new SSE connections). POST requests pass through as-is.
func sseWithKeepalive(handler http.Handler, interval time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			handler.ServeHTTP(w, r)
			return
		}

		kw := newKeepaliveResponseWriter(w, interval)
		kw.startKeepalive()
		defer kw.stop()

		handler.ServeHTTP(kw, r)
	})
}
