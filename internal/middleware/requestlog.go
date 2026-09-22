package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// RequestLogConfig controls request body logging.
type RequestLogConfig struct {
	Dir string // directory to write log files
}

// requestLogEntry is the JSON structure written to each log file.
type requestLogEntry struct {
	Timestamp string            `json:"timestamp"`
	RequestID string            `json:"request_id"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	Body      json.RawMessage   `json:"body"`
}

var requestLogSeq atomic.Int64

// RequestLog returns a middleware that dumps full request bodies to JSON files.
// Each request is saved as a separate file: {dir}/{timestamp}_{seq}.json
func RequestLog(cfg RequestLogConfig, logger *slog.Logger) Middleware {
	if cfg.Dir == "" {
		cfg.Dir = "logs"
	}

	// Ensure log directory exists
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		logger.Error("requestlog: failed to create log dir", "dir", cfg.Dir, "error", err)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only log the surfaces that carry a conversation. Responses is
			// one of them: it is what every Codex session speaks.
			switch r.URL.Path {
			case "/v1/chat/completions", "/v1/messages", "/v1/responses":
			default:
				next.ServeHTTP(w, r)
				return
			}

			// Read body
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				logger.Error("requestlog: failed to read body", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			// Restore body for downstream handlers
			r.Body = io.NopCloser(bytes.NewReader(body))

			// Capture selected headers
			headers := map[string]string{
				"content-type":      r.Header.Get("Content-Type"),
				"x-request-id":      r.Header.Get("X-Request-ID"),
				"anthropic-version": r.Header.Get("anthropic-version"),
				"user-agent":        r.Header.Get("User-Agent"),
			}

			entry := requestLogEntry{
				Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				RequestID: RequestIDFromContext(r.Context()),
				Method:    r.Method,
				Path:      r.URL.Path,
				Headers:   headers,
				Body:      json.RawMessage(body),
			}

			// Write to file asynchronously
			go func() {
				seq := requestLogSeq.Add(1)
				ts := time.Now().UTC().Format("20060102_150405")
				filename := fmt.Sprintf("%s_%04d.json", ts, seq)
				fpath := filepath.Join(cfg.Dir, filename)

				data, err := json.MarshalIndent(entry, "", "  ")
				if err != nil {
					logger.Error("requestlog: marshal error", "error", err)
					return
				}

				if err := os.WriteFile(fpath, data, 0644); err != nil {
					logger.Error("requestlog: write error", "path", fpath, "error", err)
					return
				}
				logger.Debug("requestlog: saved", "path", fpath, "bytes", len(body))
			}()

			next.ServeHTTP(w, r)
		})
	}
}
