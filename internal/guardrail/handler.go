package guardrail

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// AnalyzeHandler exposes the guardrail Engine as an HTTP endpoint compatible
// with the legacy LLM Guard API format used by backend workflow nodes.
// Endpoint: POST /analyze/batch/beta/litellm_basic_guardrail_api
type AnalyzeHandler struct {
	engine *Engine
	logger *slog.Logger
}

// NewAnalyzeHandler creates a handler that wraps the guardrail engine.
func NewAnalyzeHandler(engine *Engine, logger *slog.Logger) *AnalyzeHandler {
	return &AnalyzeHandler{engine: engine, logger: logger}
}

type analyzeRequest struct {
	Messages   []analyzeMessage `json:"messages"`
	Guardrails interface{}      `json:"guardrails,omitempty"` // []string or CSV string
	TeamID     string           `json:"team_id,omitempty"`
	KeyHash    string           `json:"user_api_key_hash,omitempty"`
}

type analyzeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type analyzeResponse struct {
	Action              string   `json:"action"`
	BlockedReason       string   `json:"blocked_reason,omitempty"`
	TriggeredScanners   []string `json:"triggered_scanners,omitempty"`
	TriggeredGuardrails []string `json:"triggered_guardrails,omitempty"`
}

func (h *AnalyzeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body.Close()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, analyzeResponse{Action: "ERROR"})
		return
	}

	var req analyzeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, analyzeResponse{Action: "ERROR"})
		return
	}

	// Convert messages
	messages := make([]Message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = Message(m)
	}

	// Parse guardrails (supports []string, CSV string, or nil)
	guardrails := normalizeGuardrails(req.Guardrails)

	result := h.engine.Scan(r.Context(), messages, guardrails, nil)

	// Built field by field rather than converted: this is the wire shape the
	// legacy LLM Guard clients parse, and it must not grow a field just
	// because Result did.
	writeJSON(w, http.StatusOK, analyzeResponse{
		Action:              result.Action,
		BlockedReason:       result.BlockedReason,
		TriggeredScanners:   result.TriggeredScanners,
		TriggeredGuardrails: result.TriggeredGuardrails,
	})
}

func normalizeGuardrails(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		parts := strings.Split(v, ",")
		var out []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	case []interface{}:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(v)
	_, _ = w.Write(data)
}
