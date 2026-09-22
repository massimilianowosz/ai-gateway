package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func ephemeral() map[string]interface{} { return map[string]interface{}{"type": "ephemeral"} }

// The breakpoints the caller placed are what makes Anthropic charge a tenth of
// the input price for a repeated prefix. Rebuilding the request without them
// meant re-reading a growing context at full price on every tool call.
func TestConvertToAnthropic_ReattachesCacheBreakpoints(t *testing.T) {
	req := &provider.CompletionRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "long brief", CacheControl: ephemeral()},
			{Role: "user", Content: "hi", CacheControl: ephemeral()},
			{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{
				{ID: "t1", Type: "function", Function: provider.FunctionCall{Name: "Read", Arguments: "{}"}},
			}, CacheControl: ephemeral()},
			{Role: "tool", ToolCallID: "t1", Content: "done", CacheControl: ephemeral()},
		},
		Tools: []provider.Tool{
			{Type: "function", Function: provider.Function{Name: "Read"}},
			{Type: "function", Function: provider.Function{Name: "Bash"}, CacheControl: ephemeral()},
		},
	}

	ar := convertToAnthropic(context.Background(), req, "claude-sonnet-4", false)
	raw, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), `"cache_control"`); n != 5 {
		t.Fatalf("cache_control markers = %d, want 5 (system, user, assistant, tool_result, one tool): %s", n, raw)
	}

	if ar.Tools[0].CacheControl != nil {
		t.Error("invented a breakpoint on a tool the caller did not mark")
	}
	if ar.Tools[1].CacheControl == nil {
		t.Error("last tool lost its breakpoint")
	}
}

// A caller that marks nothing must produce the request shape as before,
// strings and all.
func TestConvertToAnthropic_NoBreakpointsKeepsStringShape(t *testing.T) {
	req := &provider.CompletionRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
		},
	}

	ar := convertToAnthropic(context.Background(), req, "claude-sonnet-4", false)
	if _, ok := ar.System.(string); !ok {
		t.Errorf("system promoted to blocks without a breakpoint to carry: %#v", ar.System)
	}
	if _, ok := ar.Messages[0].Content.(string); !ok {
		t.Errorf("user content promoted to blocks without a breakpoint to carry: %#v", ar.Messages[0].Content)
	}
}

// Anthropic routes an OAuth subscription request on the agent identity in the
// billing block. Pinning one version ages out of support while the caller's
// own CLI keeps moving.
func TestConvertToAnthropic_RelaysCallerVersion(t *testing.T) {
	ctx := provider.WithClientIdentity(context.Background(), provider.ClientIdentity{
		Product: "claude-cli", Version: "2.1.263", App: "cli",
	})
	req := &provider.CompletionRequest{Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	ar := convertToAnthropic(ctx, req, "claude-sonnet-4", true)
	blocks, ok := ar.System.([]anthropicSystemBlock)
	if !ok || len(blocks) == 0 {
		t.Fatalf("system = %#v, want billing blocks", ar.System)
	}
	if !strings.Contains(blocks[0].Text, "cc_version=2.1.263") {
		t.Errorf("billing block = %q, want the caller's version", blocks[0].Text)
	}
}

func TestConvertToAnthropic_FallsBackToDefaultVersion(t *testing.T) {
	req := &provider.CompletionRequest{Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	ar := convertToAnthropic(context.Background(), req, "claude-sonnet-4", true)
	blocks := ar.System.([]anthropicSystemBlock)
	if !strings.Contains(blocks[0].Text, "cc_version="+defaultClaudeCodeVersion) {
		t.Errorf("billing block = %q, want the default version", blocks[0].Text)
	}
}

// The stream reported usage to nobody: the counters were parsed and dropped,
// so every streamed turn was billed off a character-count estimate and the
// client was told it had spent no input at all.
func TestStreamReader_ForwardsUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":200,"cache_read_input_tokens":100000}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n\n")

	r := &streamReader{
		reader: bufio.NewReader(strings.NewReader(sse)),
		body:   io.NopCloser(strings.NewReader("")),
	}

	var last map[string]interface{}
	for {
		chunk, err := r.Next()
		if err != nil {
			break
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(chunk, &decoded); err != nil {
			t.Fatal(err)
		}
		if _, ok := decoded["usage"]; ok {
			last = decoded
		}
	}

	if last == nil {
		t.Fatal("no chunk carried usage")
	}
	usage := last["usage"].(map[string]interface{})
	if usage["prompt_tokens"] != float64(100200) {
		t.Errorf("prompt_tokens = %v, want 100200 (inclusive of the cache read)", usage["prompt_tokens"])
	}
	if usage["cache_read_input_tokens"] != float64(100000) {
		t.Errorf("cache_read_input_tokens = %v, want 100000", usage["cache_read_input_tokens"])
	}
	if usage["completion_tokens"] != float64(42) {
		t.Errorf("completion_tokens = %v, want 42", usage["completion_tokens"])
	}
}
