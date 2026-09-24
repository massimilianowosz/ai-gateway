package hivetrace

import (
	"bytes"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
)

// traceWriter copies the response aside while it streams to the client.
//
// The client is served first and the copy is made second, deliberately: the
// reverse order — what the semantic cache's capture writer does — puts a buffer
// append and a possible grow in front of every flush on a streamed turn. Here
// nothing the capture does can delay a chunk that has already left.
//
// The copy is bounded by maxBytes. Past the cap writes are dropped and the
// record is marked truncated; the client still receives the whole stream,
// because truncation applies only to what is retained.
type traceWriter struct {
	http.ResponseWriter

	buf      *bytes.Buffer
	maxBytes int

	status    int
	wroteHead bool
	truncated bool

	firstByte time.Time
}

func newTraceWriter(w http.ResponseWriter, maxBytes int) *traceWriter {
	return &traceWriter{
		ResponseWriter: w,
		buf:            perf.GetBuffer(),
		maxBytes:       maxBytes,
		status:         http.StatusOK,
	}
}

func (tw *traceWriter) WriteHeader(code int) {
	if !tw.wroteHead {
		tw.status = code
		tw.wroteHead = true
	}
	tw.ResponseWriter.WriteHeader(code)
}

func (tw *traceWriter) Write(b []byte) (int, error) {
	n, err := tw.ResponseWriter.Write(b)
	if tw.firstByte.IsZero() {
		tw.firstByte = time.Now()
	}
	tw.wroteHead = true
	if n > 0 {
		tw.capture(b[:n])
	}
	return n, err
}

func (tw *traceWriter) capture(b []byte) {
	remaining := tw.maxBytes - tw.buf.Len()
	if remaining <= 0 {
		tw.truncated = true
		return
	}
	if len(b) > remaining {
		tw.buf.Write(b[:remaining])
		tw.truncated = true
		return
	}
	tw.buf.Write(b)
}

func (tw *traceWriter) Flush() {
	if f, ok := tw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets other wrappers in the chain reach the writer underneath, the
// convention the feedback appender established.
func (tw *traceWriter) Unwrap() http.ResponseWriter { return tw.ResponseWriter }

// body returns the captured bytes. The result aliases the pooled buffer and is
// only valid until release.
func (tw *traceWriter) body() []byte { return tw.buf.Bytes() }

func (tw *traceWriter) release() {
	perf.PutBuffer(tw.buf)
	tw.buf = nil
}
