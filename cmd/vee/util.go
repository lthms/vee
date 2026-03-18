package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// veeRuntimeDir returns the base runtime directory for this vee instance.
// Uses $XDG_RUNTIME_DIR/vee, falling back to /run/user/<uid>/vee.
func veeRuntimeDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, "vee")
}

// ensureRuntimeDir creates the vee runtime directory if it doesn't exist.
func ensureRuntimeDir() error {
	return os.MkdirAll(veeRuntimeDir(), 0700)
}

// shelljoin quotes a string for safe use in a shell command if it contains
// special characters.
func shelljoin(s string) string {
	if s == "" {
		return "''"
	}
	// If it contains no shell-special characters, return as-is.
	safe := true
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' ||
			c == '.' || c == '/' || c == ':' || c == '=' || c == '+') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	// Use single quotes; escape existing single quotes.
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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
