package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivetrace"
)

// SetTrafficStore wires AI traffic observability into the admin API. When it
// is never called the traffic endpoints answer 503, which is what a gateway
// running with hivetrace disabled should say.
func (h *Handler) SetTrafficStore(ts hivetrace.TrafficStore) {
	h.traffic = ts
}

// SetTrafficHub wires the live update feed behind GET /v1/traffic/stream.
func (h *Handler) SetTrafficHub(hub *hivetrace.Hub) {
	h.trafficHub = hub
}

// StreamTraffic pushes trace activity to a watching console over SSE.
//
// SSE rather than a WebSocket: the feed is one-directional, the gateway
// already speaks it on every streaming route, and EventSource reconnects on
// its own — a WebSocket would add a dependency and a reconnect loop to write
// for no capability this needs. Browsers cannot set an Authorization header on
// an EventSource, so this relies on the console's session cookie, which
// RequireMasterKey already accepts.
func (h *Handler) StreamTraffic(w http.ResponseWriter, r *http.Request) {
	if h.trafficHub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "hivetrace is not enabled"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	updates, unsubscribe := h.trafficHub.Subscribe(256)
	defer unsubscribe()

	// Idle consoles would otherwise sit behind a proxy that drops a silent
	// connection; a comment frame keeps it open without being an event.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg, open := <-updates:
			if !open {
				return
			}
			payload, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// GetTraceSessions lists session summaries: what each conversation did, cost,
// tools and MCP servers used, files touched, and any PII or secret detected.
//
// Query params: team_id, key, user_id, agent_id, start_date, end_date,
// has_findings, page, page_size.
func (h *Handler) GetTraceSessions(w http.ResponseWriter, r *http.Request) {
	if h.traffic == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "hivetrace is not enabled"})
		return
	}
	filter, page, pageSize, ok := h.traceFilter(w, r)
	if !ok {
		return
	}

	sessions, err := h.traffic.ListSessions(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin: list trace sessions failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":      sessions,
		"page":      page,
		"page_size": pageSize,
		// Reported so the console can say on screen that matched values are
		// being retained. A setting this consequential must not be discoverable
		// only by reading the appliance's config file.
		"finding_samples": h.findingSamplesOn(),
	})
}

func (h *Handler) findingSamplesOn() bool {
	return h.cfg != nil && h.cfg.HiveTrace.FindingSamples
}

// GetTraceSession returns one session summary together with its turns, so the
// caller gets both the account and the timeline behind it.
//
// Query params: session_id (required).
func (h *Handler) GetTraceSession(w http.ResponseWriter, r *http.Request) {
	if h.traffic == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "hivetrace is not enabled"})
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session_id is required"})
		return
	}

	summary, err := h.traffic.GetSession(r.Context(), sessionID)
	if err != nil {
		h.logger.Error("admin: get trace session failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	events, err := h.traffic.ListEvents(r.Context(), hivetrace.Filter{SessionID: sessionID, Limit: 1000})
	if err != nil {
		h.logger.Error("admin: list trace session events failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	// A session whose summary has not been computed yet still has events, and
	// answering 404 would make a just-captured conversation look lost.
	if summary == nil && len(events) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session": summary,
		"events":  events,
	})
}

// GetTraceEvents lists individual captured turns.
//
// Query params: session_id, team_id, key, user_id, agent_id, model,
// start_date, end_date, has_findings, page, page_size.
func (h *Handler) GetTraceEvents(w http.ResponseWriter, r *http.Request) {
	if h.traffic == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "hivetrace is not enabled"})
		return
	}
	filter, page, pageSize, ok := h.traceFilter(w, r)
	if !ok {
		return
	}
	filter.SessionID = r.URL.Query().Get("session_id")
	filter.Model = r.URL.Query().Get("model")

	events, err := h.traffic.ListEvents(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin: list trace events failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":      events,
		"page":      page,
		"page_size": pageSize,
	})
}

// traceFilter parses the query params shared by the traffic endpoints. It
// writes the error response itself and reports whether the caller may proceed.
func (h *Handler) traceFilter(w http.ResponseWriter, r *http.Request) (hivetrace.Filter, int, int, bool) {
	q := r.URL.Query()
	filter := hivetrace.Filter{
		TeamID:      q.Get("team_id"),
		UserID:      q.Get("user_id"),
		AgentID:     q.Get("agent_id"),
		HasFindings: q.Get("has_findings") == "true",
	}

	if keyParam := q.Get("key"); keyParam != "" {
		key, err := h.resolveKeyIdentifier(r.Context(), keyParam)
		if err != nil {
			h.writeKeyResolutionError(w, "list traffic", err)
			return filter, 0, 0, false
		}
		filter.KeyHash = key.KeyHash
	}

	if sd := q.Get("start_date"); sd != "" {
		t, err := time.Parse("2006-01-02", sd)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid start_date, use YYYY-MM-DD"})
			return filter, 0, 0, false
		}
		filter.Since = t
	}
	if ed := q.Get("end_date"); ed != "" {
		t, err := time.Parse("2006-01-02", ed)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid end_date, use YYYY-MM-DD"})
			return filter, 0, 0, false
		}
		filter.Until = t.Add(24*time.Hour - time.Nanosecond)
	}
	// A whole day is the wrong unit for a live view: the console asks for the
	// last hour. These take precedence over the date form when both are given.
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid since, use RFC3339"})
			return filter, 0, 0, false
		}
		filter.Since = t
	}
	if u := q.Get("until"); u != "" {
		t, err := time.Parse(time.RFC3339, u)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid until, use RFC3339"})
			return filter, 0, 0, false
		}
		filter.Until = t
	}

	page := 1
	pageSize := 100
	if p := q.Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if ps := q.Get("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 1000 {
			pageSize = v
		}
	}
	filter.Limit = pageSize
	filter.Offset = (page - 1) * pageSize
	return filter, page, pageSize, true
}
