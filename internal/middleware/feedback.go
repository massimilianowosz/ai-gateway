package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// --- Trigger types ---

const (
	TriggerHiveStateCompression = "hivestate_compression"
	TriggerHiveRouteDowngrade   = "hiveroute_downgrade"
	TriggerComplexTask          = "complex_task"
	TriggerLongSession          = "long_session"
	TriggerSavingsMilestone     = "savings_milestone"
	TriggerTaskCompleted        = "task_completed"
)

// --- Feedback option & prompt types ---

// feedbackOption represents a single clickable option shown to the user.
type feedbackOption struct {
	ID    string `json:"id"`    // "A", "B", "C", "D", "E"
	Label string `json:"label"` // "No, everything's fine"
}

// feedbackPrompt bundles a trigger's question and options for rendering as a tool call.
type feedbackPrompt struct {
	Trigger  string           `json:"trigger"`
	Question string           `json:"question"`
	Options  []feedbackOption `json:"options"`
	Context  json.RawMessage  `json:"context,omitempty"`
}

// Standard option sets used across triggers.

var (
	optionsStandard = []feedbackOption{
		{ID: "A", Label: "No, everything's fine"},
		{ID: "B", Label: "Slightly different but nothing serious"},
		{ID: "C", Label: "Yes, it lost track of some things"},
		{ID: "D", Label: "Yes, responses were less accurate"},
	}

	optionsWithOptOut = []feedbackOption{
		{ID: "A", Label: "Perfect, goal achieved on the first try"},
		{ID: "B", Label: "Good, a few extra steps but OK"},
		{ID: "C", Label: "It struggled to get there"},
		{ID: "D", Label: "It didn't complete correctly"},
		{ID: "E", Label: "Don't ask again"},
	}
)

// --- Per-key session state ---

type keySession struct {
	requestCount   atomic.Int64
	lastPromptAt   time.Time
	promptsToday   int
	promptDate     string // "2006-01-02"
	firedTriggers  map[string]bool
	awaitingAnswer bool
	pendingRecord  *store.FeedbackRecord
	mu             sync.Mutex

	// Rolling window of recent request signals
	recentRouteModels []string  // last N routed models
	recentRatios      []float64 // last N compression ratios
	recentDifficulty  []string  // last N difficulty levels
	totalSavings      float64   // cumulative savings from route
	lastMilestone     int       // last savings milestone hit ($)
	taskCompleted     bool      // HiveState detected task completion
	savedTokens       int       // cumulative tokens saved by compression
	totalOrigTokens   int       // cumulative original tokens (before compression)
}

// --- Feedback Middleware ---

// Feedback handles in-session feedback collection.
type Feedback struct {
	cfg    config.FeedbackConfig
	store  store.Store
	logger *slog.Logger

	sessions sync.Map // keyHash → *keySession
	toggles  sync.Map // keyID+teamID → feedbackToggle
}

// NewFeedback creates a new feedback middleware. Returns nil if disabled.
func NewFeedback(cfg config.FeedbackConfig, db store.Store, logger *slog.Logger) *Feedback {
	if !cfg.Enabled {
		return nil
	}
	cfg.ApplyDefaults()
	return &Feedback{
		cfg:    cfg,
		store:  db,
		logger: logger,
	}
}

// Middleware returns the HTTP middleware. If Feedback is nil, returns pass-through.
func (f *Feedback) Middleware(next http.Handler) http.Handler {
	if f == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyInfo := auth.KeyInfoFromContext(r.Context())
		if keyInfo == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Check if feedback is disabled for this key
		if f.isDisabledForKey(r.Context(), keyInfo) {
			next.ServeHTTP(w, r)
			return
		}

		// Check if team is in scope
		if len(f.cfg.Teams) > 0 && !contains(f.cfg.Teams, keyInfo.TeamID) {
			next.ServeHTTP(w, r)
			return
		}

		sess := f.getSession(r.Context(), keyInfo)

		// Phase 1: If awaiting answer, capture it
		if f.captureAnswer(r, sess, keyInfo) {
			// Answer captured, continue with normal proxy
			next.ServeHTTP(w, r)
			return
		}

		// Phase 2: Record signals from response headers (set by HiveState middleware upstream)
		// These are recorded AFTER the request via a response wrapper.
		// Instead, we check for triggers BEFORE proxying based on accumulated signals.

		count := sess.requestCount.Add(1)

		// Phase 3: check triggers. A firing trigger no longer answers on the
		// request's behalf — the request is proxied either way and the question
		// rides along with the model's own reply.
		prompt := f.checkTriggers(r.Context(), sess, keyInfo, int(count))

		// Phase 4: proxy, wrapping the response to capture HiveState headers and
		// to append the question if the turn ends facing the user.
		rw := &feedbackAppender{ResponseWriter: w, header: w.Header(), surface: surfaceForPath(r.URL.Path)}
		if prompt != nil {
			rw.question = renderPromptText(prompt)
		}
		next.ServeHTTP(rw, r)
		shown := rw.finish()

		// The prompt is only spent when the user actually saw it. A turn that
		// ended in a tool call showed them nothing, so the trigger stays armed
		// for a later turn instead of being silently consumed.
		if prompt != nil && shown {
			f.markPromptShown(r, sess, keyInfo, prompt, int(count))
		}

		// Record signals from response headers for future trigger evaluation
		f.recordSignals(sess, rw)
	})
}

// --- Signal recording ---

func (f *Feedback) recordSignals(sess *keySession, rw *feedbackAppender) {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	// HiveRoute signals
	if model := rw.header.Get("X-Hiveroute-Model"); model != "" {
		sess.recentRouteModels = appendCapped(sess.recentRouteModels, model, 20)
	}
	if level := rw.header.Get("X-Hiveroute-Level"); level != "" {
		sess.recentDifficulty = appendCapped(sess.recentDifficulty, level, 20)
	}

	// Compression ratio from HiveState (set as header by middleware)
	if ratio := rw.header.Get("X-Hivestate-Ratio"); ratio != "" {
		var r float64
		if _, err := fmt.Sscanf(ratio, "%f", &r); err == nil {
			sess.recentRatios = appendCapped(sess.recentRatios, r, 20)
		}
	}

	// Token savings from compression
	if orig := rw.header.Get("X-Hivestate-Original-Tokens"); orig != "" {
		var o, t int
		if _, err := fmt.Sscanf(orig, "%d", &o); err == nil {
			sess.totalOrigTokens += o
			if result := rw.header.Get("X-Hivestate-Tokens"); result != "" {
				if _, err := fmt.Sscanf(result, "%d", &t); err == nil {
					sess.savedTokens += (o - t)
				}
			}
		}
	}

	// Task completion signal from HiveState
	if status := rw.header.Get("X-Hivestate-Status"); status == "completed" {
		sess.taskCompleted = true
	}

	// Savings accumulation
	if savings := rw.header.Get("X-Hiveroute-Savings"); savings != "" {
		var s float64
		if _, err := fmt.Sscanf(savings, "%f", &s); err == nil {
			sess.totalSavings += s
		}
	}
}

// --- Trigger evaluation ---

var errNoStore = errors.New("feedback: no store configured")

// checkTriggers takes the request context so its cross-pod dedup lookup is
// cancellable; a hung metrics DB otherwise blocks the proxied request.
func (f *Feedback) checkTriggers(ctx context.Context, sess *keySession, keyInfo *store.APIKey, count int) *feedbackPrompt {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	// Anti-spam: check interval and daily limit
	now := time.Now()
	today := now.Format("2006-01-02")
	if sess.promptDate != today {
		sess.promptsToday = 0
		sess.promptDate = today
	}
	if sess.promptsToday >= f.cfg.MaxPerDay {
		return nil
	}
	if !sess.lastPromptAt.IsZero() && now.Sub(sess.lastPromptAt) < time.Duration(f.cfg.MinIntervalMinutes)*time.Minute {
		return nil
	}
	if sess.awaitingAnswer {
		return nil
	}

	// Hydrate firedTriggers from DB (cross-pod dedup).
	//
	// The query runs with sess.mu released: holding it serialized every
	// concurrent request for the same key behind a SELECT on the request path.
	sess.mu.Unlock()
	hydrated, hydratedCount, hydrateErr := func() ([]string, int, error) {
		if f.store == nil {
			return nil, 0, errNoStore
		}
		return f.store.GetFeedbackFiredToday(ctx, keyInfo.KeyHash)
	}()
	sess.mu.Lock()

	if hydrateErr == nil {
		if triggers, dbCount, err := hydrated, hydratedCount, error(nil); err == nil {
			for _, t := range triggers {
				// task_completed is repeatable — don't block it via firedTriggers
				if t == TriggerTaskCompleted {
					continue
				}
				sess.firedTriggers[t] = true
			}
			if dbCount > sess.promptsToday {
				sess.promptsToday = dbCount
			}
		}
	}

	// Trigger 1: HiveState Compression
	if !sess.firedTriggers[TriggerHiveStateCompression] && len(sess.recentRatios) >= 5 {
		avgRatio := avg(sess.recentRatios[len(sess.recentRatios)-5:])
		if avgRatio > 0.3 { // >30% compression in last 5 requests
			ctx, _ := json.Marshal(map[string]any{"avg_compression_pct": int(avgRatio * 100), "recent_requests": 5})
			return &feedbackPrompt{
				Trigger:  TriggerHiveStateCompression,
				Question: fmt.Sprintf("In the last 5 interactions, HiveState compressed %d%% of the conversation context to reduce costs.\n\nHave you noticed any changes in response quality?", int(avgRatio*100)),
				Options:  optionsStandard,
				Context:  ctx,
			}
		}
	}

	// Trigger 2: HiveRoute Downgrade (routing to cheaper model)
	if !sess.firedTriggers[TriggerHiveRouteDowngrade] && len(sess.recentDifficulty) >= 5 {
		downgrades := 0
		for _, d := range sess.recentDifficulty[len(sess.recentDifficulty)-5:] {
			if d == "trivial" || d == "standard" {
				downgrades++
			}
		}
		if downgrades >= 4 { // 4 out of last 5 were downgraded
			savings := sess.totalSavings
			ctx, _ := json.Marshal(map[string]any{"downgrade_count": downgrades, "savings_usd": fmt.Sprintf("%.2f", savings)})
			savingsStr := ""
			if savings > 0.01 {
				savingsStr = fmt.Sprintf(", saving you about $%.2f", savings)
			}
			return &feedbackPrompt{
				Trigger:  TriggerHiveRouteDowngrade,
				Question: fmt.Sprintf("Recent requests were handled by a model optimized for standard-complexity tasks%s.\n\nHave you noticed any differences in response quality?", savingsStr),
				Options:  optionsStandard,
				Context:  ctx,
			}
		}
	}

	// Trigger 3: Complex task with many tool calls (high difficulty + lots of requests)
	if !sess.firedTriggers[TriggerComplexTask] && len(sess.recentDifficulty) >= 5 {
		complexCount := 0
		for _, d := range sess.recentDifficulty[len(sess.recentDifficulty)-5:] {
			if d == "complex" || d == "expert" {
				complexCount++
			}
		}
		if complexCount >= 3 && count > 15 {
			ctx, _ := json.Marshal(map[string]any{"complex_count": complexCount, "total_requests": count})
			return &feedbackPrompt{
				Trigger:  TriggerComplexTask,
				Question: "You're working on a complex task.\nHow is the response quality?",
				Options: []feedbackOption{
					{ID: "A", Label: "Accurate and fast, no issues"},
					{ID: "B", Label: "A few extra rounds but it gets there"},
					{ID: "C", Label: "Seems confused, loses track often"},
					{ID: "D", Label: "Wrong or out-of-context responses"},
				},
				Context: ctx,
			}
		}
	}

	// Trigger 4: Savings milestone
	if !sess.firedTriggers[TriggerSavingsMilestone] && sess.totalSavings > 0 {
		for _, milestone := range f.cfg.SavingsMilestones {
			if sess.totalSavings >= float64(milestone) && sess.lastMilestone < milestone {
				sess.lastMilestone = milestone
				ctx, _ := json.Marshal(map[string]any{"milestone_usd": milestone, "total_savings_usd": fmt.Sprintf("%.2f", sess.totalSavings)})
				return &feedbackPrompt{
					Trigger:  TriggerSavingsMilestone,
					Question: fmt.Sprintf("You've saved $%.2f so far compared to standard usage.\nHow is the overall experience?", sess.totalSavings),
					Options: []feedbackOption{
						{ID: "A", Label: "Great, no perceived difference"},
						{ID: "B", Label: "Good, some minor acceptable trade-offs"},
						{ID: "C", Label: "Savings are there but quality suffers"},
						{ID: "D", Label: "I'd rather pay more for full quality"},
					},
					Context: ctx,
				}
			}
		}
	}

	// Trigger 5: Long session
	if !sess.firedTriggers[TriggerLongSession] && count >= f.cfg.LongSessionAfter {
		savings := sess.totalSavings
		ctx, _ := json.Marshal(map[string]any{"request_count": count, "savings_usd": fmt.Sprintf("%.2f", savings)})
		savingsStr := ""
		if savings > 0.01 {
			savingsStr = fmt.Sprintf("\nYou've saved $%.2f so far compared to standard usage.", savings)
		}
		return &feedbackPrompt{
			Trigger:  TriggerLongSession,
			Question: fmt.Sprintf("This session is quite long (%d interactions).%s\n\nHow is it going?", count, savingsStr),
			Options: []feedbackOption{
				{ID: "A", Label: "Smooth and fast, precise responses"},
				{ID: "B", Label: "Good, a few things to adjust"},
				{ID: "C", Label: "Agent loses context too often"},
				{ID: "D", Label: "Slow or frequent errors"},
			},
			Context: ctx,
		}
	}

	// Trigger 6: Task completed — fires every time (repeatable, not blocked by firedTriggers)
	if sess.taskCompleted && count >= 5 {
		sess.taskCompleted = false // reset so it can fire again on next task
		savings := sess.totalSavings
		savedTok := sess.savedTokens
		ctx, _ := json.Marshal(map[string]any{"request_count": count, "savings_usd": fmt.Sprintf("%.2f", savings), "saved_tokens": savedTok})

		// Build savings summary
		var parts []string
		if savedTok > 1000 {
			parts = append(parts, fmt.Sprintf("compressing %dk context tokens", savedTok/1000))
		}
		if savings > 0.01 {
			parts = append(parts, fmt.Sprintf("saving $%.2f with smart routing", savings))
		}
		savingsStr := ""
		if len(parts) > 0 {
			savingsStr = " " + strings.Join(parts, " and ") + "."
		}

		return &feedbackPrompt{
			Trigger:  TriggerTaskCompleted,
			Question: fmt.Sprintf("Task completed!%s\nHow did it go?", savingsStr),
			Options:  optionsWithOptOut,
			Context:  ctx,
		}
	}

	return nil
}

// --- Feedback response ---

// feedbackAppender wraps the response so a feedback question can ride along
// with the model's own answer instead of replacing it.
//
// Replacing it discarded the user's request outright: next was never called,
// the prompt they had written reached no provider, and in an agentic loop the
// caller got prose where it expected a tool_use block. Appending keeps the turn
// intact — the question is extra text at the end of an answer the user was
// already going to read.
//
// The question is only appended to a turn that actually ends facing the user.
// A turn ending in a tool call is the agent's own loop: the client will execute
// the tool and never show the text, so the prompt would be wasted and the
// tool_result would then be captured as its "answer".
type feedbackAppender struct {
	http.ResponseWriter
	header http.Header

	// surface is taken from the route, not sniffed from the bytes: the three
	// APIs shape both their bodies and their terminal events differently, and
	// guessing between two of them wrote an OpenAI chunk into a Responses
	// stream.
	surface apiSurface

	question string // empty when nothing is to be appended

	wroteHeader bool
	sse         bool

	jsonBuf      bytes.Buffer
	heldTerminal []byte
	sawToolCall  bool
	appended     bool
}

func (rw *feedbackAppender) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.wroteHeader = true
	rw.sse = strings.HasPrefix(rw.Header().Get("Content-Type"), "text/event-stream")
	if rw.buffering() && code == http.StatusOK {
		// The body is rewritten, so the length the upstream declared is wrong.
		rw.Header().Del("Content-Length")
	}
	rw.ResponseWriter.WriteHeader(code)
}

// buffering reports whether this response is a candidate for an append at all.
func (rw *feedbackAppender) buffering() bool { return rw.question != "" }

func (rw *feedbackAppender) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	if !rw.buffering() {
		return rw.ResponseWriter.Write(b)
	}
	if !rw.sse {
		return rw.jsonBuf.Write(b)
	}
	// Streaming: pass everything through except the bytes that close the
	// stream, which are held so the question can go out ahead of them.
	if rw.isTerminalSSE(b) {
		rw.heldTerminal = append(rw.heldTerminal, b...)
		return len(b), nil
	}
	if sseCarriesToolCall(b) {
		rw.sawToolCall = true
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *feedbackAppender) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *feedbackAppender) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// finish emits whatever was withheld, with the question appended when the turn
// ended facing the user. It reports whether the question was actually shown.
func (rw *feedbackAppender) finish() bool {
	if !rw.buffering() {
		return false
	}
	if rw.sse {
		if !rw.sawToolCall {
			rw.appended = rw.writeSSEQuestion()
		}
		if len(rw.heldTerminal) > 0 {
			_, _ = rw.ResponseWriter.Write(rw.heldTerminal)
		}
		rw.Flush()
		return rw.appended
	}

	body := rw.jsonBuf.Bytes()
	if rewritten, ok := appendQuestionToJSON(body, rw.question); ok {
		_, _ = rw.ResponseWriter.Write(rewritten)
		rw.appended = true
	} else {
		_, _ = rw.ResponseWriter.Write(body)
	}
	return rw.appended
}

// apiSurface names the wire dialect a response is speaking.
type apiSurface int

const (
	surfaceOpenAI apiSurface = iota
	surfaceAnthropic
	surfaceResponses
)

func surfaceForPath(path string) apiSurface {
	switch path {
	case "/v1/messages":
		return surfaceAnthropic
	case "/v1/responses":
		return surfaceResponses
	default:
		return surfaceOpenAI
	}
}

// isTerminalSSE reports whether a write closes the stream, in this surface's
// dialect. OpenAI ends with "data: [DONE]", Anthropic with message_delta and
// message_stop, Responses with a terminal response.* event.
func (rw *feedbackAppender) isTerminalSSE(b []byte) bool {
	switch rw.surface {
	case surfaceAnthropic:
		return bytes.Contains(b, []byte("event: message_stop")) ||
			bytes.Contains(b, []byte("event: message_delta"))
	case surfaceResponses:
		return bytes.Contains(b, []byte("event: response.completed")) ||
			bytes.Contains(b, []byte("event: response.incomplete")) ||
			bytes.Contains(b, []byte("event: response.failed"))
	default:
		return bytes.Contains(b, []byte("data: [DONE]"))
	}
}

// sseCarriesToolCall reports whether the stream opened a tool call, which means
// the turn belongs to the agent's loop rather than to the user.
func sseCarriesToolCall(b []byte) bool {
	return bytes.Contains(b, []byte(`"tool_use"`)) ||
		bytes.Contains(b, []byte(`"tool_calls"`)) ||
		bytes.Contains(b, []byte(`"function_call"`))
}

// writeSSEQuestion emits the question as ordinary assistant text, in whichever
// dialect the stream is speaking.
//
// Responses is deliberately left alone. Adding text there means opening an
// output item, which needs an index and an item id consistent with what the
// provider already sent — get that wrong and the client sees a malformed turn,
// which is worse than not asking. The question is simply not shown, so the
// trigger stays armed for a turn that can carry it.
func (rw *feedbackAppender) writeSSEQuestion() bool {
	if rw.surface == surfaceResponses {
		return false
	}
	w := rw.ResponseWriter
	text, err := json.Marshal("\n\n" + rw.question)
	if err != nil {
		return false
	}
	if rw.surface == surfaceAnthropic {
		// A fresh text block of its own, opened and closed, so it cannot
		// disturb the indexes of the blocks already sent.
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":99,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":99,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", text)
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":99}\n\n")
		return true
	}
	fmt.Fprintf(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\n", text)
	return true
}

// appendQuestionToResponsesOutput appends the question as a text part on the
// last assistant message of a Responses output, declining when the turn ended
// in a function call.
func appendQuestionToResponsesOutput(raw json.RawMessage, question string) (json.RawMessage, bool) {
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil, false
	}
	last := -1
	for i, item := range items {
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		if strings.HasSuffix(typ, "_call") {
			return nil, false
		}
		if typ == "message" {
			last = i
		}
	}
	if last < 0 {
		return nil, false
	}

	var parts []json.RawMessage
	if json.Unmarshal(items[last]["content"], &parts) != nil {
		return nil, false
	}
	appended, err := json.Marshal(map[string]interface{}{
		"type": "output_text", "text": question, "annotations": []interface{}{},
	})
	if err != nil {
		return nil, false
	}
	encoded, err := json.Marshal(append(parts, appended))
	if err != nil {
		return nil, false
	}
	items[last]["content"] = encoded
	out, err := json.Marshal(items)
	if err != nil {
		return nil, false
	}
	return out, true
}

// appendQuestionToJSON adds the question to the assistant text of a
// non-streaming body, leaving everything else alone. It declines when the turn
// ended in a tool call, or when the body is not a shape it recognises.
func appendQuestionToJSON(body []byte, question string) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}

	// Responses: {"output":[{"type":"message","content":[{"type":"output_text",...}]}]}
	if raw, ok := doc["output"]; ok {
		rewritten, appended := appendQuestionToResponsesOutput(raw, question)
		if !appended {
			return nil, false
		}
		doc["output"] = rewritten
		out, err := json.Marshal(doc)
		if err != nil {
			return nil, false
		}
		return out, true
	}

	// Anthropic: {"content":[{"type":"text","text":...}], ...}
	if raw, ok := doc["content"]; ok {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(raw, &blocks) != nil {
			return nil, false
		}
		for _, b := range blocks {
			var typ string
			if json.Unmarshal(b["type"], &typ) == nil && typ == "tool_use" {
				return nil, false
			}
		}
		var rawBlocks []json.RawMessage
		if json.Unmarshal(raw, &rawBlocks) != nil {
			return nil, false
		}
		appended, err := json.Marshal(map[string]string{"type": "text", "text": question})
		if err != nil {
			return nil, false
		}
		encoded, err := json.Marshal(append(rawBlocks, appended))
		if err != nil {
			return nil, false
		}
		doc["content"] = encoded
		out, err := json.Marshal(doc)
		if err != nil {
			return nil, false
		}
		return out, true
	}

	// OpenAI: {"choices":[{"message":{"content":...,"tool_calls":...}}]}
	raw, ok := doc["choices"]
	if !ok {
		return nil, false
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(raw, &choices) != nil || len(choices) == 0 {
		return nil, false
	}
	msg := map[string]json.RawMessage{}
	if json.Unmarshal(choices[0]["message"], &msg) != nil {
		return nil, false
	}
	if _, hasTools := msg["tool_calls"]; hasTools {
		return nil, false
	}
	var content string
	_ = json.Unmarshal(msg["content"], &content)
	encoded, err := json.Marshal(content + "\n\n" + question)
	if err != nil {
		return nil, false
	}
	msg["content"] = encoded
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	choices[0]["message"] = msgJSON
	choicesJSON, err := json.Marshal(choices)
	if err != nil {
		return nil, false
	}
	doc["choices"] = choicesJSON
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// renderPromptText turns a prompt into the plain text appended to an answer.
//
// Plain text rather than the tool call this used to send: a tool call grafted
// onto a real response would have to renumber the content blocks already sent
// and change the turn's stop_reason, and a client that had been told the turn
// ended will not answer one anyway. The reply comes back as an ordinary
// message, which captureAnswer already reads.
func renderPromptText(prompt *feedbackPrompt) string {
	var b strings.Builder
	b.WriteString("---\n📊 ")
	b.WriteString(prompt.Question)
	for _, opt := range prompt.Options {
		b.WriteString("\n  ")
		b.WriteString(opt.ID)
		b.WriteString(") ")
		b.WriteString(opt.Label)
	}
	return b.String()
}

// markPromptShown records that the question reached the user: it arms answer
// capture, spends one of the day's prompts, and files the record other pods
// hydrate from.
//
// One row, not two. The prompt used to write a stub here and captureAnswer a
// second row later, while GetFeedbackFiredToday counts rows as prompts — so
// every completed cycle counted twice and a key was silenced after roughly one
// question. The stub also left answerless rows in every listing.
func (f *Feedback) markPromptShown(r *http.Request, sess *keySession, keyInfo *store.APIKey, prompt *feedbackPrompt, count int) {
	optionsJSON, _ := json.Marshal(prompt.Options)

	record := &store.FeedbackRecord{
		KeyHash:             keyInfo.KeyHash,
		KeyPrefix:           keyInfo.KeyPrefix,
		TeamID:              keyInfo.TeamID,
		TriggerType:         prompt.Trigger,
		TriggerContext:      prompt.Context,
		QuestionShown:       prompt.Question,
		OptionsShown:        string(optionsJSON),
		SessionRequestCount: count,
		CreatedAt:           time.Now(),
	}

	sess.mu.Lock()
	sess.firedTriggers[prompt.Trigger] = true
	sess.lastPromptAt = time.Now()
	sess.promptsToday++
	sess.awaitingAnswer = true
	sess.pendingRecord = record
	sess.mu.Unlock()

	if err := f.store.StoreFeedback(r.Context(), record); err != nil {
		f.logger.Error("feedback: failed to store pending record", "error", err)
	}

	f.logger.Info("feedback: prompt shown",
		"trigger", prompt.Trigger,
		"key_prefix", keyInfo.KeyPrefix,
		"team_id", keyInfo.TeamID,
	)
}

// --- Answer capture ---

func (f *Feedback) captureAnswer(r *http.Request, sess *keySession, keyInfo *store.APIKey) bool {
	sess.mu.Lock()
	if !sess.awaitingAnswer || sess.pendingRecord == nil {
		sess.mu.Unlock()
		return false
	}
	record := sess.pendingRecord
	sess.mu.Unlock()

	// The pending state is cleared only once the answer is accepted. Clearing
	// first threw the record away whenever the user ignored the question and
	// typed something else: the row stayed answerless forever, and a genuine
	// answer on the following turn was no longer being waited for.

	// Extract answer from tool_result or last user message
	answer := extractToolResult(r)
	if answer == "" {
		answer = extractLastUserMessage(r)
	}
	if answer == "" {
		return false
	}

	// Discard if the answer is too long — it's a regular prompt, not a feedback response
	if len(answer) > 200 {
		f.logger.Info("feedback: answer too long, discarding",
			"trigger", record.TriggerType,
			"length", len(answer),
		)
		return false
	}

	// Accepted: now the pending state can be released.
	sess.mu.Lock()
	sess.awaitingAnswer = false
	sess.pendingRecord = nil
	sess.lastPromptAt = time.Now() // Reset cooldown from answer time, not prompt time
	sess.mu.Unlock()

	record.Answer = answer
	record.AnswerNormalized = normalizeAnswer(answer)

	// Handle opt-out: "E" option or explicit "don't ask" phrases
	if record.AnswerNormalized == "opt_out" || strings.Contains(strings.ToLower(answer), "don't ask") || strings.Contains(strings.ToLower(answer), "do not ask") {
		go func() {
			if err := f.store.SetKeyFeedbackEnabled(context.Background(), keyInfo.KeyHash, false); err != nil {
				f.logger.Error("feedback: failed to disable for key", "error", err)
			} else {
				f.logger.Info("feedback: disabled by user",
					"key_prefix", keyInfo.KeyPrefix,
					"team_id", keyInfo.TeamID,
				)
			}
		}()
		record.AnswerNormalized = "opt_out"
	}

	// Store async
	go func() {
		if err := f.store.StoreFeedback(context.Background(), record); err != nil {
			f.logger.Error("feedback: failed to store", "error", err)
		} else {
			f.logger.Info("feedback: answer stored",
				"trigger", record.TriggerType,
				"answer", record.Answer,
				"normalized", record.AnswerNormalized,
				"team_id", record.TeamID,
			)
		}
	}()

	return true
}

// extractToolResult looks for a tool_result in the request body.
// Anthropic: messages[].content[] with type "tool_result"
// OpenAI: messages[] with role "tool"
func extractToolResult(r *http.Request) string {
	if r.Body == nil {
		return ""
	}

	bodyBytes, err := readAndRestoreBody(r)
	if err != nil || len(bodyBytes) == 0 {
		return ""
	}

	// Try Anthropic format: look for tool_result content blocks in user messages
	var anthropicReq struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(bodyBytes, &anthropicReq) == nil {
		for i := len(anthropicReq.Messages) - 1; i >= 0; i-- {
			msg := anthropicReq.Messages[i]
			if msg.Role != "user" {
				continue
			}
			// Try array of content blocks
			var blocks []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(msg.Content, &blocks) == nil {
				for j := len(blocks) - 1; j >= 0; j-- {
					if blocks[j].Type == "tool_result" {
						return extractContentString(blocks[j].Content)
					}
				}
			}
		}
	}

	// Try OpenAI format: look for role:"tool" messages
	var openaiReq struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(bodyBytes, &openaiReq) == nil {
		for i := len(openaiReq.Messages) - 1; i >= 0; i-- {
			if openaiReq.Messages[i].Role == "tool" && strings.HasPrefix(openaiReq.Messages[i].ToolCallID, "call_feedback_") {
				return openaiReq.Messages[i].Content
			}
		}
	}

	return ""
}

// --- Helpers ---

// getSession hydrates a key's session, using the request's context for the
// lookups. context.Background() made them uncancellable: a hung metrics DB
// blocked the proxied request with no way for a client disconnect to abort it.
func (f *Feedback) getSession(ctx context.Context, keyInfo *store.APIKey) *keySession {
	if s, ok := f.sessions.Load(keyInfo.KeyHash); ok {
		return s.(*keySession)
	}
	s := &keySession{
		firedTriggers: make(map[string]bool),
	}
	// Hydrate from DB to handle multi-pod and pod restarts
	if f.store != nil {
		if triggers, count, err := f.store.GetFeedbackFiredToday(ctx, keyInfo.KeyHash); err == nil {
			for _, t := range triggers {
				if t == TriggerTaskCompleted {
					continue
				}
				s.firedTriggers[t] = true
			}
			s.promptsToday = count
			s.promptDate = time.Now().Format("2006-01-02")
		}
		// Hydrate session signals (request count, ratios, route levels) from metrics DB
		if signals, err := f.store.GetFeedbackSessionSignals(ctx, keyInfo.KeyPrefix); err == nil && signals != nil {
			s.requestCount.Store(int64(signals.RequestCount))
			if len(signals.Ratios) > 0 {
				s.recentRatios = signals.Ratios
			}
			if len(signals.RouteLevels) > 0 {
				s.recentDifficulty = signals.RouteLevels
			}
			if signals.TotalSavings > 0 {
				s.totalSavings = signals.TotalSavings
			}
		}
	}
	actual, _ := f.sessions.LoadOrStore(keyInfo.KeyHash, s)
	return actual.(*keySession)
}

// feedbackToggleTTL is how long a resolved enable/disable decision is reused.
//
// This runs on the request hot path and neither lookup is cached in the store,
// so without it every proxied request paid two uncached queries just to learn a
// toggle that changes by hand, once in a while. A few seconds of staleness on
// an operator action is a fair trade for that.
const feedbackToggleTTL = 30 * time.Second

type feedbackToggle struct {
	disabled bool
	at       time.Time
}

func (f *Feedback) isDisabledForKey(ctx context.Context, keyInfo *store.APIKey) bool {
	if f.store == nil {
		return false
	}

	cacheKey := fmt.Sprintf("%s\x00%s\x00%d", keyInfo.ID, keyInfo.TeamID, keyInfo.GovernanceRevision)
	if v, ok := f.toggles.Load(cacheKey); ok {
		if entry := v.(feedbackToggle); time.Since(entry.at) < feedbackToggleTTL {
			return entry.disabled
		}
	}

	disabled := f.resolveDisabled(ctx, keyInfo)
	f.toggles.Store(cacheKey, feedbackToggle{disabled: disabled, at: time.Now()})
	return disabled
}

func (f *Feedback) resolveDisabled(ctx context.Context, keyInfo *store.APIKey) bool {
	// Check per-key override first (highest priority)
	// Keyed by KeyHash, which is what HiveRoute writes and reads for the same
	// table. Using keyInfo.ID here addressed a different primary key, so an
	// opt-out created a second row that HiveRoute never saw — with an empty
	// team_id on a not-null column.
	keySettings, captured := auth.KeyRouteSettingsFromContext(ctx)
	var err error
	if !captured {
		keySettings, err = f.store.GetKeyRouteSettings(ctx, keyInfo.KeyHash)
	}
	if err == nil && keySettings != nil && keySettings.FeedbackEnabled != nil {
		return !*keySettings.FeedbackEnabled
	}

	// Check per-team override
	if keyInfo.TeamID != "" {
		ts, err := f.store.GetTenantSettings(ctx, keyInfo.TeamID)
		if err == nil && ts != nil && ts.FeedbackEnabled != nil {
			return !*ts.FeedbackEnabled
		}
	}

	return false
}

func extractLastUserMessage(r *http.Request) string {
	if r.Body == nil {
		return ""
	}

	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}

	bodyBytes, err := readAndRestoreBody(r)
	if err != nil || len(bodyBytes) == 0 {
		return ""
	}

	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return ""
	}

	// Find last user message
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if parsed.Messages[i].Role == "user" {
			return extractContentString(parsed.Messages[i].Content)
		}
	}
	return ""
}

func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, err
}

func extractContentString(raw json.RawMessage) string {
	// Try string
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

func normalizeAnswer(answer string) string {
	a := strings.TrimSpace(strings.ToUpper(answer))
	// Check if it's a simple A/B/C/D/E answer
	if len(a) == 1 {
		switch a {
		case "A":
			return "positive"
		case "B":
			return "neutral"
		case "C":
			return "negative"
		case "D":
			return "critical"
		case "E":
			return "opt_out"
		}
	}
	// Check for "A)" style
	if len(a) >= 2 && a[1] == ')' {
		switch a[0] {
		case 'A':
			return "positive"
		case 'B':
			return "neutral"
		case 'C':
			return "negative"
		case 'D':
			return "critical"
		case 'E':
			return "opt_out"
		}
	}
	// If user typed something else, it's freetext
	if a == "" || a == "SKIP" {
		return "skipped"
	}
	return "freetext"
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func appendCapped[T any](slice []T, item T, max int) []T {
	slice = append(slice, item)
	if len(slice) > max {
		slice = slice[len(slice)-max:]
	}
	return slice
}

func avg(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
