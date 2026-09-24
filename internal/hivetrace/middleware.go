package hivetrace

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/middleware"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// Middleware captures every model call and hands it to the recorder.
//
// It belongs last in the chain, closest to the proxy handler: there the
// request body is the one actually sent upstream — after guardrail redaction,
// workflow rewrites and HiveState compression — and the response bytes are the
// ones the provider produced.
//
// What happens here is deliberately minimal: two body copies and a struct.
// Parsing, scanning, redaction and persistence all belong to the recorder's
// worker, so a traced request is not a slower request.
func Middleware(rec *Recorder) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec == nil || !isTraceablePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			// Only install a usage capture when nothing upstream did. Shadowing
			// HiveState's would leave it reading token counts nobody fills.
			usage := store.GetUsageCapture(r.Context())
			if usage == nil {
				usage = &store.UsageCapture{}
				r = r.WithContext(store.WithUsageCapture(r.Context(), usage))
			}

			tw := newTraceWriter(w, rec.cfg.MaxBodyBytes)
			start := time.Now()
			next.ServeHTTP(tw, r)
			elapsed := time.Since(start)

			// The captured bytes alias a pooled buffer, so the copy has to
			// happen before it goes back to the pool.
			responseBody := append([]byte(nil), tw.body()...)
			tw.release()

			rec.Record(buildCapture(r, tw, body, responseBody, usage, start, elapsed))
		})
	}
}

func buildCapture(r *http.Request, tw *traceWriter, requestBody, responseBody []byte, usage *store.UsageCapture, start time.Time, elapsed time.Duration) capture {
	api, _ := apiFor(r.URL.Path)
	ctx := r.Context()

	ev := Event{
		ID:         uuid.NewString(),
		RequestID:  middleware.RequestIDFromContext(ctx),
		API:        api,
		Path:       r.URL.Path,
		ClientIP:   callerIP(r),
		Status:     tw.status,
		Streaming:  strings.Contains(tw.Header().Get("Content-Type"), "event-stream"),
		StartedAt:  start.UTC(),
		DurationMs: elapsed.Milliseconds(),
		CreatedAt:  time.Now().UTC(),
	}
	if !tw.firstByte.IsZero() {
		ev.TTFBMs = tw.firstByte.Sub(start).Milliseconds()
	}

	if key := auth.KeyInfoFromContext(ctx); key != nil {
		ev.TeamID = key.TeamID
		ev.KeyHash = key.KeyHash
		ev.KeyPrefix = key.KeyPrefix
		ev.AgentID = key.AgentID
		ev.UserID = key.CreatedByUserID
	}
	id, ok := provider.ClientIdentityFrom(ctx)
	if !ok {
		id = provider.ParseClientIdentity(r.Header.Get("User-Agent"), r.Header.Get("x-app"))
	}
	ev.ClientProduct = id.Product
	ev.ClientVersion = id.Version

	ev.Model = tw.Header().Get("X-Ubiquum-Model")
	if ev.Model == "" {
		ev.Model = requestModel(requestBody)
	}
	ev.Provider = tw.Header().Get("X-Ubiquum-Provider")
	if ev.Provider == "" && ev.Status >= 400 {
		// Nothing served a failed request, so name the provider that refused it last.
		if failed := tw.Header().Get("X-Ubiquum-Failed"); failed != "" {
			ev.Provider = failed[strings.LastIndex(failed, ",")+1:]
		}
	}
	ev.BillingMode = tw.Header().Get("X-Ubiquum-Billing-Mode")
	if cost, err := strconv.ParseFloat(tw.Header().Get("X-Ubiquum-Cost"), 64); err == nil {
		ev.Cost = cost
	}

	if usage != nil && usage.Filled {
		ev.PromptTokens = usage.PromptTokens
		ev.CompletionTokens = usage.CompletionTokens
		ev.TotalTokens = usage.TotalTokens
		ev.CachedPromptTokens = usage.CachedPromptTokens
		if ev.Cost == 0 {
			ev.Cost = usage.Cost
		}
	}

	sessionHeader := r.Header.Get(hivestate.SessionHeader)
	// Fill the session from the header here as well as in the worker, so the
	// live capture message already carries it. A console can light the session
	// up as the turn lands instead of waiting for the flush that derives it.
	// The worker recomputes it either way, and DeriveSessionID returns the
	// header verbatim, so the two never disagree.
	ev.SessionID = strings.TrimSpace(sessionHeader)

	return capture{
		event:         ev,
		sessionHeader: sessionHeader,
		clientApp:     id.App,
		upstream:      auth.ScopedUpstreamProvider(ctx),
		requestBody:   requestBody,
		responseBody:  responseBody,
		responseCut:   tw.truncated,
	}
}

// isTraceablePath keeps capture to the model-facing surfaces. Listing models
// or uploading a file produces no conversation to account for.
func isTraceablePath(path string) bool {
	return strings.HasSuffix(path, "/chat/completions") ||
		strings.HasSuffix(path, "/messages") ||
		strings.HasSuffix(path, "/responses")
}

// callerIP is the address the request came from.
//
// A forwarded header is trusted only for its first entry, which is what the
// edge saw; the rest is client-supplied and forgeable. With no proxy in front
// the connection's own address is the only honest answer, and on a workstation
// appliance that is a loopback address — correct, if unexciting.
func callerIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first := forwarded
		if comma := strings.Index(forwarded, ","); comma > 0 {
			first = forwarded[:comma]
		}
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func requestModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	return req.Model
}
