package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// hookActionResponse is the optional JSON body a hook can return to signal an
// explicit action. Supported values:
//   - "BLOCKED" — reject the request (reason is used as the error message)
//
// The action body is only parsed when the hook returns a 2xx status.
// A non-2xx status always rejects regardless of the body.
type hookActionResponse struct {
	Action string `json:"action"` // "BLOCKED"
	Reason string `json:"reason,omitempty"`
}

// Hooks implements pre/post request HTTP callouts.
// Pre-request hooks can reject requests (non-2xx OR action:"BLOCKED" → 403 to client).
// Post-request hooks are fire-and-forget (async, non-blocking).
type Hooks struct {
	pre    *config.HookEndpoint
	post   *config.HookEndpoint
	client *http.Client
	logger *slog.Logger
}

const redactedHeaderValue = "[REDACTED]"

var sensitiveHookPayloadHeaders = map[string]struct{}{
	"api-key":                          {},
	"anthropic-api-key":                {},
	"authorization":                    {},
	"cookie":                           {},
	"openai-api-key":                   {},
	"proxy-authorization":              {},
	"set-cookie":                       {},
	"x-api-key":                        {},
	"x-auth-token":                     {},
	"x-goog-api-key":                   {},
	"x-ubiquum-upstream-authorization": {},
}

// NewHooks creates a hooks middleware. If both pre and post are nil, it's a no-op.
func NewHooks(pre, post *config.HookEndpoint, logger *slog.Logger) *Hooks {
	return &Hooks{
		pre:  pre,
		post: post,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		logger: logger,
	}
}

// PreRequest returns a middleware that calls out to the pre-request hook.
// If the hook returns non-2xx, the request is rejected with 403.
func (h *Hooks) PreRequest(next http.Handler) http.Handler {
	if h.pre == nil {
		return next
	}
	timeout := h.pre.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hookPayload := map[string]any{
			"method":      r.Method,
			"path":        r.URL.Path,
			"headers":     flattenHeaders(r.Header),
			"remote_addr": r.RemoteAddr,
		}

		body, _ := json.Marshal(hookPayload)
		hookReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.pre.URL, bytes.NewReader(body))
		if err != nil {
			h.logger.Warn("pre-hook: failed to create request", "error", err)
			next.ServeHTTP(w, r)
			return
		}
		hookReq.Header.Set("Content-Type", "application/json")
		for k, v := range h.pre.Headers {
			hookReq.Header.Set(k, v)
		}

		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(hookReq)
		if err != nil {
			h.logger.Warn("pre-hook: callout failed", "url", h.pre.URL, "error", err)
			// Fail open — let request through if hook is unreachable
			next.ServeHTTP(w, r)
			return
		}
		// Read and close the body once so we can inspect it
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"request rejected by pre-request hook","type":"hook_rejected"}}`))
			return
		}

		// 2xx — check for an explicit BLOCKED action in the response body
		var action hookActionResponse
		if json.Unmarshal(respBody, &action) == nil && action.Action == "BLOCKED" {
			reason := action.Reason
			if reason == "" {
				reason = "request blocked by pre-request hook"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			msg, _ := json.Marshal(map[string]any{
				"error": map[string]any{"message": reason, "type": "hook_rejected"},
			})
			_, _ = w.Write(msg)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// PostRequest returns a middleware that fires a post-request hook asynchronously.
func (h *Hooks) PostRequest(next http.Handler) http.Handler {
	if h.post == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		// Fire post-hook asynchronously
		go func() {
			hookPayload := map[string]any{
				"method": r.Method,
				"path":   r.URL.Path,
				"status": wrapped.status,
			}

			body, _ := json.Marshal(hookPayload)
			timeout := h.post.Timeout
			if timeout <= 0 {
				timeout = 5 * time.Second
			}

			hookReq, err := http.NewRequest(http.MethodPost, h.post.URL, bytes.NewReader(body))
			if err != nil {
				return
			}
			hookReq.Header.Set("Content-Type", "application/json")
			for k, v := range h.post.Headers {
				hookReq.Header.Set(k, v)
			}

			client := &http.Client{Timeout: timeout}
			resp, err := client.Do(hookReq)
			if err != nil {
				h.logger.Debug("post-hook: callout failed", "url", h.post.URL, "error", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	})
}

func flattenHeaders(h http.Header) map[string]string {
	flat := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			if isSensitiveHookPayloadHeader(k) {
				flat[k] = redactedHeaderValue
				continue
			}
			flat[k] = v[0]
		}
	}
	return flat
}

func isSensitiveHookPayloadHeader(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	if _, ok := sensitiveHookPayloadHeaders[normalized]; ok {
		return true
	}

	return strings.Contains(normalized, "apikey") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "token") ||
		strings.HasSuffix(normalized, "-api-key")
}
