package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockLedger records what each content block index did over a stream.
type blockLedger struct {
	opened  map[float64]int
	closed  map[float64]int
	kind    map[float64]string
	deltaIn map[float64][]string // delta types seen per index
	order   []float64
}

func readBlocks(t *testing.T, events []sseEvent) *blockLedger {
	t.Helper()
	l := &blockLedger{
		opened: map[float64]int{}, closed: map[float64]int{},
		kind: map[float64]string{}, deltaIn: map[float64][]string{},
	}
	for _, ev := range events {
		idx, ok := ev.Data["index"].(float64)
		if !ok {
			continue
		}
		switch ev.Name {
		case "content_block_start":
			l.opened[idx]++
			l.order = append(l.order, idx)
			if cb, ok := ev.Data["content_block"].(map[string]any); ok {
				l.kind[idx], _ = cb["type"].(string)
			}
		case "content_block_stop":
			l.closed[idx]++
		case "content_block_delta":
			if d, ok := ev.Data["delta"].(map[string]any); ok {
				dt, _ := d["type"].(string)
				l.deltaIn[idx] = append(l.deltaIn[idx], dt)
			}
		}
	}
	return l
}

// assertWellFormed checks the invariants an Anthropic client relies on.
func (l *blockLedger) assertWellFormed(t *testing.T) {
	t.Helper()
	for idx, n := range l.opened {
		assert.Equal(t, 1, n, "block %v opened %d times", idx, n)
		assert.Equal(t, 1, l.closed[idx], "block %v opened once, closed %d times", idx, l.closed[idx])
	}
	for idx, n := range l.closed {
		assert.NotZero(t, l.opened[idx], "block %v closed %d times but never opened", idx, n)
	}
	for idx, deltas := range l.deltaIn {
		for _, dt := range deltas {
			switch l.kind[idx] {
			case "text":
				assert.Equal(t, "text_delta", dt, "block %v is text but carried a %s", idx, dt)
			case "tool_use":
				assert.Equal(t, "input_json_delta", dt, "block %v is tool_use but carried a %s", idx, dt)
			}
		}
	}
}

// The shape a coding agent actually produces: the model says something, calls a
// tool, then says something else. Every block must open and close exactly once
// at its own index, and no delta may land in a block of the wrong kind.
func TestAnthropicStream_TextToolText(t *testing.T) {
	g := newGateway(t, []string{"m"})
	stop := "tool_calls"
	g.scripts.set("m", &script{chunks: [][]byte{
		openAIChunk(map[string]any{"role": "assistant"}, nil),
		openAIChunk(map[string]any{"content": "penso"}, nil),
		toolCallChunk(0, "call_a", "Bash", `{"cmd":"ls"}`),
		openAIChunk(map[string]any{"content": "fatto"}, nil),
		openAIChunk(map[string]any{}, &stop),
	}})

	key := g.mintKey(100, "m")
	resp := g.post("/v1/messages", key,
		`{"model":"m","messages":[{"role":"user","content":"ciao"}],"max_tokens":64,"stream":true}`)
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	events := parseSSE(t, resp.body)
	readBlocks(t, events).assertWellFormed(t)

	names := map[string]int{}
	for _, ev := range events {
		names[ev.Name]++
	}
	assert.Equal(t, 1, names["message_start"], "exactly one message_start")
	assert.Equal(t, 1, names["message_stop"], "exactly one message_stop")
	assert.Contains(t, resp.body, "penso")
	assert.Contains(t, resp.body, "fatto")
}

// Parallel tool calls must land in separate blocks with parseable arguments.
// Merging them produced one block holding two JSON objects glued together.
func TestAnthropicStream_ParallelToolCalls(t *testing.T) {
	g := newGateway(t, []string{"m"})
	stop := "tool_calls"
	g.scripts.set("m", &script{chunks: [][]byte{
		openAIChunk(map[string]any{"role": "assistant"}, nil),
		toolCallChunk(0, "call_a", "alpha", `{"x":1}`),
		toolCallChunk(1, "call_b", "beta", `{"y":2}`),
		openAIChunk(map[string]any{}, &stop),
	}})

	key := g.mintKey(100, "m")
	resp := g.post("/v1/messages", key,
		`{"model":"m","messages":[{"role":"user","content":"ciao"}],"max_tokens":64,"stream":true}`)
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	events := parseSSE(t, resp.body)
	l := readBlocks(t, events)
	l.assertWellFormed(t)

	tools := 0
	for _, k := range l.kind {
		if k == "tool_use" {
			tools++
		}
	}
	assert.Equal(t, 2, tools, "each parallel call needs a block of its own")

	args := map[float64]string{}
	for _, ev := range events {
		if ev.Name != "content_block_delta" {
			continue
		}
		idx, _ := ev.Data["index"].(float64)
		if d, ok := ev.Data["delta"].(map[string]any); ok && d["type"] == "input_json_delta" {
			pj, _ := d["partial_json"].(string)
			args[idx] += pj
		}
	}
	require.Len(t, args, 2)
	for idx, a := range args {
		assert.True(t, json.Valid([]byte(a)),
			"block %v carries unparseable arguments %q — two payloads were merged", idx, a)
	}
}

// A tool call whose name arrives after its arguments must not push a later text
// delta into the tool's block.
func TestAnthropicStream_LateToolNameKeepsTextInItsOwnBlock(t *testing.T) {
	g := newGateway(t, []string{"m"})
	stop := "stop"
	g.scripts.set("m", &script{chunks: [][]byte{
		openAIChunk(map[string]any{"content": "prima"}, nil),
		toolCallChunk(0, "call_a", "", ""), // id only: state exists, cannot start
		openAIChunk(map[string]any{"content": "dopo"}, nil),
		openAIChunk(map[string]any{}, &stop),
	}})

	key := g.mintKey(100, "m")
	resp := g.post("/v1/messages", key,
		`{"model":"m","messages":[{"role":"user","content":"ciao"}],"max_tokens":64,"stream":true}`)
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	readBlocks(t, parseSSE(t, resp.body)).assertWellFormed(t)
}

// An upstream failure must reach the client as a real status, not a 200
// carrying an error event: clients back off on the status line.
func TestAnthropicStream_UpstreamFailureKeepsItsStatus(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{err: assert.AnError})

	key := g.mintKey(100, "m")
	resp := g.post("/v1/messages", key,
		`{"model":"m","messages":[{"role":"user","content":"ciao"}],"max_tokens":64,"stream":true}`)

	assert.NotEqual(t, http.StatusOK, resp.code,
		"an upstream failure answered 200; a client would never back off")
}
