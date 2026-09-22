// Package logbuf provides a thread-safe ring buffer for log lines.
package logbuf

import (
	"strings"
	"sync"
)

// Buffer is a fixed-size ring buffer for log lines.
type Buffer struct {
	mu    sync.Mutex
	lines []string
	pos   int
	full  bool
}

// New creates a buffer that holds up to cap lines.
func New(cap int) *Buffer {
	return &Buffer{lines: make([]string, cap)}
}

// Write implements io.Writer. Each call appends lines split by newline.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		b.lines[b.pos] = line
		b.pos = (b.pos + 1) % len(b.lines)
		if b.pos == 0 {
			b.full = true
		}
	}
	return len(p), nil
}

// Lines returns up to n most recent lines (0 = all available).
func (b *Buffer) Lines(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []string
	if b.full {
		out = make([]string, len(b.lines))
		copy(out, b.lines[b.pos:])
		copy(out[len(b.lines)-b.pos:], b.lines[:b.pos])
	} else {
		out = make([]string, b.pos)
		copy(out, b.lines[:b.pos])
	}
	if n > 0 && n < len(out) {
		out = out[len(out)-n:]
	}
	return out
}
