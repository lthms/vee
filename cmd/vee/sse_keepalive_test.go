package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestKeepaliveCommentsAreSent verifies that the middleware sends periodic
// SSE comment lines to an idle connection.
func TestKeepaliveCommentsAreSent(t *testing.T) {
	const interval = 50 * time.Millisecond

	// A handler that blocks until the client disconnects, simulating an
	// idle SSE session (like the go-sdk SSEHandler does).
	idle := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})

	srv := httptest.NewServer(sseWithKeepalive(idle, interval))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Read lines until we see at least 2 keepalive comments.
	scanner := bufio.NewScanner(resp.Body)
	seen := 0
	deadline := time.After(5 * time.Second)
	lines := make(chan string)

	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	for seen < 2 {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("connection closed before receiving 2 keepalive comments")
			}
			if strings.TrimSpace(line) == ": keepalive" {
				seen++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for keepalive comments (saw %d)", seen)
		}
	}
}

// TestKeepaliveDoesNotAffectPOST verifies that POST requests pass through
// the middleware without any wrapping.
func TestKeepaliveDoesNotAffectPOST(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	srv := httptest.NewServer(sseWithKeepalive(inner, 50*time.Millisecond))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}

// TestKeepaliveWriteSerializesWithHandler verifies that keepalive comments
// and handler writes do not interleave (each line is intact).
func TestKeepaliveWriteSerializesWithHandler(t *testing.T) {
	const interval = 30 * time.Millisecond
	const eventData = "data: hello world\n\n"

	// A handler that writes an SSE event, then blocks.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Write a real event after a short delay to race with keepalive.
		time.Sleep(interval / 2)
		w.Write([]byte(eventData))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})

	srv := httptest.NewServer(sseWithKeepalive(handler, interval))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(5 * time.Second)
	lines := make(chan string)

	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	sawEvent := false
	sawKeepalive := false

	for !sawEvent || !sawKeepalive {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("connection closed prematurely")
			}
			trimmed := strings.TrimSpace(line)
			switch {
			case trimmed == ": keepalive":
				sawKeepalive = true
			case trimmed == "data: hello world":
				sawEvent = true
			case trimmed == "":
				// empty line (SSE record delimiter)
			default:
				t.Errorf("unexpected line: %q", line)
			}
		case <-deadline:
			t.Fatalf("timed out (sawEvent=%v, sawKeepalive=%v)", sawEvent, sawKeepalive)
		}
	}
}
