package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// contentString renders a message's polymorphic content as text.
func contentString(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		raw, _ := json.Marshal(c)
		return string(raw)
	}
}

// noisyLog is the kind of tool output live compression exists to shrink: many
// identical lines around a couple that matter.
func noisyLog() string {
	var b strings.Builder
	b.WriteString("2026-01-02 10:00:00 INFO starting test run\n")
	for i := 0; i < 300; i++ {
		b.WriteString("2026-01-02 10:00:01 INFO downloading module cache\n")
	}
	b.WriteString("2026-01-02 10:00:30 INFO 412 tests passed\n")
	return b.String()
}

// Live compression shrinks the newest tool output and keeps the line that
// carries the answer. What matters is what the provider receives, which is the
// one thing unit tests of the transform cannot check.
func TestLiveCompression_ShrinksToolOutputOnTheWire(t *testing.T) {
	lz := config.LiveCompressionConfig{Enabled: true, AllowLossy: true}
	g := newGatewayOpts(t, []string{"m"}, options{liveZone: &lz})
	g.scripts.set("m", &script{complete: textAnswer("ok")})
	key := g.mintKey(100, "m")

	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tests"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]string{"name": "bash", "arguments": "{}"},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": noisyLog()},
		},
	})
	require.NoError(t, err)

	resp := g.post("/v1/chat/completions", key, string(body))
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	seen := g.scripts.lastRequest()
	require.NotNil(t, seen, "the provider was never called")

	toolContent := ""
	for _, m := range seen.Messages {
		if m.Role == "tool" {
			toolContent = contentString(m.Content)
		}
	}
	require.NotEmpty(t, toolContent, "the tool result never reached the provider")

	assert.Less(t, len(toolContent), len(noisyLog()),
		"the tool output reached the provider uncompressed")
	assert.Contains(t, toolContent, "412 tests passed",
		"compression dropped the line the agent actually needs")
}

// Compression must not disturb the conversation around it: the user's own turn
// reaches the provider unchanged.
func TestLiveCompression_LeavesTheUserTurnAlone(t *testing.T) {
	lz := config.LiveCompressionConfig{Enabled: true, AllowLossy: true}
	g := newGatewayOpts(t, []string{"m"}, options{liveZone: &lz})
	g.scripts.set("m", &script{complete: textAnswer("ok")})
	key := g.mintKey(100, "m")

	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "una domanda molto specifica"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]string{"name": "bash", "arguments": "{}"},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": noisyLog()},
		},
	})
	resp := g.post("/v1/chat/completions", key, string(body))
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	seen := g.scripts.lastRequest()
	require.NotNil(t, seen)

	user := ""
	for _, m := range seen.Messages {
		if m.Role == "user" {
			user = contentString(m.Content)
		}
	}
	assert.Equal(t, "una domanda molto specifica", user,
		"the user's own turn was rewritten")
}

// Disabled means untouched: the provider sees exactly what the client sent.
func TestLiveCompression_DisabledForwardsVerbatim(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("ok")})
	key := g.mintKey(100, "m")

	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tests"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]string{"name": "bash", "arguments": "{}"},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": noisyLog()},
		},
	})
	resp := g.post("/v1/chat/completions", key, string(body))
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	seen := g.scripts.lastRequest()
	require.NotNil(t, seen)
	for _, m := range seen.Messages {
		if m.Role == "tool" {
			assert.Equal(t, noisyLog(), contentString(m.Content), "the body was rewritten with compression off")
		}
	}
}

// A tool call and the result answering it must stay adjacent through any
// rewrite. Anthropic and OpenAI both reject a request where they are not, so
// this is the invariant a compressed agentic turn lives or dies by.
//
// Driven over /v1/messages: that is the Claude Code path, and its rewriter
// splits on assistant steps, so an agentic conversation with a single human
// turn is actually compressed. The OpenAI rewriter splits on user turns and
// would leave this shape alone.
func TestCompression_ToolPairsStayAdjacent(t *testing.T) {
	// A real extraction model, so history compression actually rewrites. With
	// none configured the extractor is nil, Process returns NO_OP, and a test
	// of what the rewrite preserves proves nothing.
	hs := config.HiveStateConfig{Enabled: true, Threshold: 1, StepWindow: 2, Model: "state-model"}
	g := newGatewayOpts(t, []string{"m", "state-model"}, options{hiveState: &hs})
	g.scripts.set("m", &script{complete: textAnswer("ok")})
	g.scripts.set("state-model", &script{complete: textAnswer(
		`{"intent":"run the build","conversation_status":"in_progress","difficulty":"standard"}`)})
	key := g.mintKey(100, "m")

	msgs := []any{map[string]any{"role": "user", "content": "avvia il lavoro"}}
	for i := 0; i < 8; i++ {
		id := "toolu_" + string(rune('a'+i))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": id, "name": "Bash", "input": map[string]string{"cmd": "ls"},
			}}},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": noisyLog(),
			}}},
		)
	}
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 64, "messages": msgs})

	resp := g.post("/v1/messages", key, string(body))
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	seen := g.scripts.lastRequest()
	require.NotNil(t, seen)
	// The rewrite must have happened, or the invariant below is untested.
	require.Less(t, len(seen.Messages), len(msgs),
		"history was not compressed: this test would pass on any code")

	// Both directions. A call with no answer and an answer with no call are
	// each rejected by the provider, and a rewrite can produce either: cutting
	// the history one message late orphans a result whose call was compressed
	// away, which a one-directional check does not see.
	open := map[string]bool{}
	for i, m := range seen.Messages {
		for _, tc := range m.ToolCalls {
			require.Less(t, i+1, len(seen.Messages),
				"tool call %s is the final message: nothing answers it", tc.ID)
			next := seen.Messages[i+1]
			require.Equal(t, tc.ID, next.ToolCallID,
				"tool call %s at %d is answered by %q", tc.ID, i, next.ToolCallID)
			open[tc.ID] = true
		}
		if m.Role == "tool" {
			require.True(t, open[m.ToolCallID],
				"message %d answers tool call %q, which was never made", i, m.ToolCallID)
		}
	}
}

// Compression is an optimisation: a body it cannot handle must be forwarded
// rather than mangled or refused.
func TestCompression_UnparseableBodyIsForwarded(t *testing.T) {
	lz := config.LiveCompressionConfig{Enabled: true, AllowLossy: true}
	g := newGatewayOpts(t, []string{"m"}, options{liveZone: &lz})
	key := g.mintKey(100, "m")

	// Valid JSON the compressor has no shape for.
	resp := g.post("/v1/chat/completions", key, `{"model":"m","messages":[{"role":"user","content":"ciao"}],"weird":{"nested":[1,2,3]}}`)
	assert.Equal(t, http.StatusOK, resp.code, resp.body)
}
