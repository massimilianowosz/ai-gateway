package hivestate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// scriptedExtractor answers each extraction call with a different state, so a
// log that wrongly re-extracts old turns produces visibly different bytes.
type scriptedExtractor struct {
	mu       sync.Mutex
	calls    int
	prompts  []string
	systems  []string
	response func(call int) string
}

func (p *scriptedExtractor) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	for _, m := range req.Messages {
		if m.Role == "system" {
			p.systems = append(p.systems, fmt.Sprint(m.Content))
		} else {
			p.prompts = append(p.prompts, fmt.Sprint(m.Content))
		}
	}
	p.mu.Unlock()
	return &provider.CompletionResponse{
		Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: p.response(call)}}},
		Usage:   &provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	}, nil
}

func (p *scriptedExtractor) Stream(context.Context, *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, fmt.Errorf("stream not expected")
}

func (p *scriptedExtractor) Name() string { return "scripted" }

func (p *scriptedExtractor) snapshot() (int, []string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, append([]string(nil), p.prompts...), append([]string(nil), p.systems...)
}

// testLogWriter sends engine logs to stderr only when HIVESTATE_TEST_LOG is set,
// so a failing run can be diagnosed without drowning a passing one.
func testLogWriter() io.Writer {
	if os.Getenv("HIVESTATE_TEST_LOG") != "" {
		return os.Stderr
	}
	return io.Discard
}

func appendOnlyEngine(t *testing.T, mock provider.Provider) *HiveState {
	t.Helper()
	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name: "state-model", Provider: "test", ProviderModel: "state-model-upstream",
	}}, hiveStateFactory{provider: mock})
	require.NoError(t, err)

	engine, err := New(config.HiveStateConfig{
		Enabled:         true,
		Model:           "state-model",
		Threshold:       1,
		MaxLatencyMs:    1000,
		StepWindow:      1,
		AppendOnlyState: true,
	}, registry, slog.New(slog.NewTextHandler(testLogWriter(), &slog.HandlerOptions{Level: slog.LevelInfo})))
	require.NoError(t, err)
	return engine
}

// stateRegion returns the text HiveState puts in front of the recent turns.
func stateRegion(t *testing.T, res *Result) string {
	t.Helper()
	require.Equal(t, ModeState, res.Mode, "expected the state path, got %s (%s)", res.Mode, res.FallbackReason)
	for _, m := range res.Messages {
		if strings.Contains(m.Content, stateLogPreamble) {
			return m.Content
		}
	}
	t.Fatalf("no state region in the rewritten messages")
	return ""
}

// baseConversation is long enough that the zone split leaves a non-empty
// History: with a shorter one HiveState has nothing to compress and never
// reaches the state path.
func baseConversation(turn, firstMarker string) []Message {
	return []Message{
		{Role: "system", Content: strings.Repeat("Stable system policy. ", 10)},
		{Role: "user", Content: turn + " " + firstMarker},
		{Role: "assistant", Content: turn + " second"},
		{Role: "user", Content: turn + " third"},
		{Role: "assistant", Content: turn + " fourth"},
		{Role: "user", Content: turn + " fifth"},
		{Role: "assistant", Content: "The latest step remains verbatim."},
		{Role: "user", Content: "Continue."},
	}
}

// The reason this design exists: as the conversation grows, the state region
// must only gain bytes at the end. Anything else rewrites the head of the
// prompt, and the provider re-bills every token after the change at full input
// price — which is what makes the snapshot approach cost more than it saves.
func TestAppendOnlyState_StateRegionOnlyGrowsAcrossTurns(t *testing.T) {
	mock := &scriptedExtractor{response: func(call int) string {
		return fmt.Sprintf(`{"intent":"step_%d","difficulty":"standard","reasoning_effort":"medium","active_constraints":{"values":{"step":%d}},"conversation_status":"in_progress"}`, call, call)
	}}
	engine := appendOnlyEngine(t, mock)
	scope := Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"}

	turn := strings.Repeat("Detailed implementation context with identifiers and constraints. ", 45)
	messages := baseConversation(turn, "first")

	var regions []string
	for i := 0; i < 4; i++ {
		res := engine.Process(context.Background(), messages, scope)
		regions = append(regions, stateRegion(t, res))

		// The conversation grows: two more turns enter the history.
		messages = append(messages[:len(messages)-1],
			Message{Role: "assistant", Content: turn + fmt.Sprintf(" reply %d", i)},
			Message{Role: "user", Content: turn + fmt.Sprintf(" ask %d", i)},
			Message{Role: "user", Content: "Continue."},
		)
	}

	for i := 1; i < len(regions); i++ {
		if !strings.HasPrefix(regions[i], regions[i-1]) {
			t.Fatalf("state region at turn %d is not a byte-exact prefix of turn %d\nturn %d:\n%s\nturn %d:\n%s",
				i-1, i, i-1, regions[i-1], i, regions[i])
		}
	}
	if len(regions[len(regions)-1]) <= len(regions[0]) {
		t.Fatal("the state region never grew; the log is not accumulating")
	}
}

// A conversation that has not moved on must not cost another extraction call:
// the log already covers every message in the history.
func TestAppendOnlyState_UnchangedHistoryCostsNoExtraction(t *testing.T) {
	mock := &scriptedExtractor{response: func(int) string {
		return `{"intent":"hold","difficulty":"standard","reasoning_effort":"low","active_constraints":{},"conversation_status":"in_progress"}`
	}}
	engine := appendOnlyEngine(t, mock)
	scope := Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"}

	turn := strings.Repeat("Detailed implementation context with identifiers and constraints. ", 45)
	messages := baseConversation(turn, "first")

	first := stateRegion(t, engine.Process(context.Background(), messages, scope))
	callsAfterFirst, _, _ := mock.snapshot()

	second := stateRegion(t, engine.Process(context.Background(), messages, scope))
	callsAfterSecond, _, _ := mock.snapshot()

	assert.Equal(t, first, second, "an unchanged history must render an identical state region")
	assert.Equal(t, callsAfterFirst, callsAfterSecond, "an unchanged history must not trigger another extraction")
}

// The second extraction must be a delta: it is shown the earlier state and only
// the new turns. If it were handed the whole history again the log would cost
// as much as the snapshot it replaces.
func TestAppendOnlyState_SecondExtractionSeesOnlyNewTurns(t *testing.T) {
	mock := &scriptedExtractor{response: func(call int) string {
		return fmt.Sprintf(`{"intent":"step_%d","difficulty":"standard","reasoning_effort":"medium","active_constraints":{},"conversation_status":"in_progress"}`, call)
	}}
	engine := appendOnlyEngine(t, mock)
	scope := Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"}

	turn := strings.Repeat("Detailed implementation context with identifiers and constraints. ", 45)
	messages := baseConversation(turn, "OPENING_MARKER")
	engine.Process(context.Background(), messages, scope)

	messages = append(messages[:len(messages)-1],
		Message{Role: "assistant", Content: turn + " LATER_MARKER"},
		Message{Role: "user", Content: turn + " and then"},
		Message{Role: "user", Content: "Continue."},
	)
	// A second exchange, so LATER_MARKER has left the recent window and is part
	// of the history the next extraction is responsible for.
	messages = append(messages[:len(messages)-1],
		Message{Role: "assistant", Content: turn + " even later"},
		Message{Role: "user", Content: turn + " and after"},
		Message{Role: "user", Content: "Continue."},
	)
	engine.Process(context.Background(), messages, scope)

	calls, prompts, systems := mock.snapshot()
	require.Equal(t, 2, calls, "the second turn should have extracted exactly once")

	second := prompts[1]
	assert.Contains(t, second, "LATER_MARKER", "the delta must contain the new turns")
	assert.NotContains(t, second, "OPENING_MARKER",
		"the delta re-read a turn the log already covers; the log is not incremental")
	assert.Contains(t, systems[1], "APPEND-ONLY MODE",
		"the delta call must use the append-only contract, or the model rewrites the whole state")
}

// Different sessions must not share a log, even when they open identically:
// one conversation's state leaking into another is a correctness failure, not
// a cost one.
func TestAppendOnlyState_SessionsDoNotShareALog(t *testing.T) {
	mock := &scriptedExtractor{response: func(call int) string {
		return fmt.Sprintf(`{"intent":"step_%d","difficulty":"standard","reasoning_effort":"low","active_constraints":{},"conversation_status":"in_progress"}`, call)
	}}
	engine := appendOnlyEngine(t, mock)

	turn := strings.Repeat("Detailed implementation context with identifiers and constraints. ", 45)
	messages := baseConversation(turn, "first")

	a := stateRegion(t, engine.Process(context.Background(), messages, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"}))
	b := stateRegion(t, engine.Process(context.Background(), messages, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s2"}))

	assert.NotEqual(t, a, b, "a second session reused the first session's state log")
	calls, _, _ := mock.snapshot()
	assert.Equal(t, 2, calls, "each session must extract its own state")
}
