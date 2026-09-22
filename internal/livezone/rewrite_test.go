package livezone

import (
	"encoding/json"
	"strings"
	"testing"
)

func bigLog() string {
	var sb strings.Builder
	sb.WriteString("2026-01-02 10:00:00 INFO starting test run\n")
	for i := 0; i < 300; i++ {
		sb.WriteString("2026-01-02 10:00:01 INFO downloading module cache\n")
	}
	sb.WriteString("2026-01-02 10:00:30 INFO 412 tests passed\n")
	return sb.String()
}

func lossyPolicy() Policy {
	p := DefaultPolicy()
	p.Options.AllowLossy = true
	p.Options.MinBytes = 0
	return p
}

// The end-to-end question for the wiring: does a Bash tool result actually get
// smaller in the body that would be forwarded?
func TestRewriteAnthropic_CompressesShellOutput(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tests"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash",
					"input": map[string]string{"command": "go test ./..."}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": bigLog()},
			}},
		},
	})

	out, stats, err := RewriteAnthropic(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged == 0 {
		t.Fatalf("nothing was compressed (seen=%d protected=%d)", stats.BlocksSeen, stats.SkippedProtect)
	}
	if len(out) >= len(body) {
		t.Fatalf("body did not shrink: %d -> %d", len(body), len(out))
	}
	// The conclusion the agent needs must survive.
	if !strings.Contains(string(out), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
	t.Logf("%d -> %d bytes (%.0f%% saved), transformers=%v",
		len(body), len(out), 100*(1-float64(len(out))/float64(len(body))), stats.Transformers)
}

// The protection list is the safety story: a Read result must come out
// byte-identical even though it is large and repetitive.
func TestRewriteAnthropic_ProtectedToolIsUntouched(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{
			map[string]any{"role": "user", "content": "open the file"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_9", "name": "Read",
					"input": map[string]string{"file_path": "/app/main.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_9", "content": bigLog()},
			}},
		},
	})

	out, stats, err := RewriteAnthropic(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedProtect != 1 {
		t.Fatalf("expected the Read result to be skipped as protected, got %d", stats.SkippedProtect)
	}
	if string(out) != string(body) {
		t.Fatal("a protected tool result must be forwarded byte-for-byte")
	}
}

func TestRewriteOpenAI_CompressesShellOutput(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tests"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]string{"name": "bash", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": bigLog()},
		},
	})

	out, stats, err := RewriteOpenAI(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged == 0 {
		t.Fatalf("nothing compressed (seen=%d protected=%d)", stats.BlocksSeen, stats.SkippedProtect)
	}
	if !strings.Contains(string(out), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
	t.Logf("%d -> %d bytes", len(body), len(out))
}

func TestRewriteOpenAI_ProtectedToolIsUntouched(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "search"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_7", "type": "function",
					"function": map[string]string{"name": "Grep", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_7", "content": bigLog()},
		},
	})
	out, stats, err := RewriteOpenAI(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedProtect != 1 {
		t.Fatalf("Grep must be protected, skipped=%d", stats.SkippedProtect)
	}
	if string(out) != string(body) {
		t.Fatal("protected result must be byte-identical")
	}
}

func TestRewriteResponses_CompressesShellOutput(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "run the tests"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": bigLog()},
		},
	})

	out, stats, err := RewriteResponses(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged == 0 {
		t.Fatalf("nothing compressed (seen=%d protected=%d)", stats.BlocksSeen, stats.SkippedProtect)
	}
	if len(out) >= len(body) {
		t.Fatalf("body did not shrink: %d -> %d", len(body), len(out))
	}
	if !strings.Contains(string(out), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
	t.Logf("%d -> %d bytes", len(body), len(out))
}

// Codex CLI's own built-in tools (shell, apply_patch, ...) come through as
// custom_tool_call/custom_tool_call_output, not function_call/
// function_call_output — the shape that went unhandled and silently
// no-op'd live compression on real Codex traffic until this was added.
func TestRewriteResponses_CompressesCustomToolCallOutput(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "run the tests"}}},
			map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "shell", "input": "go test ./..."},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": bigLog()},
		},
	})

	out, stats, err := RewriteResponses(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged == 0 {
		t.Fatalf("nothing compressed (seen=%d protected=%d)", stats.BlocksSeen, stats.SkippedProtect)
	}
	if !strings.Contains(string(out), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
	t.Logf("%d -> %d bytes", len(body), len(out))
}

// The Responses API also carries output as an array of content parts
// (e.g. [{"type":"output_text","text":"..."}]), not only a bare string.
func TestRewriteResponses_CompressesStructuredOutput(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "run the tests"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{
				map[string]string{"type": "output_text", "text": bigLog()},
			}},
		},
	})

	out, stats, err := RewriteResponses(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged == 0 {
		t.Fatalf("nothing compressed (seen=%d protected=%d)", stats.BlocksSeen, stats.SkippedProtect)
	}
	if !strings.Contains(string(out), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
}

// Codex CLI itself never sends "output_text" or "text" in a
// function_call_output/custom_tool_call_output: FunctionCallOutputContentItem
// (openai/codex's protocol crate) tags a tool result's text parts
// "input_text" — the same content-item type it uses for input_image and
// input_audio. A result with more than one content part (text mixed with a
// screenshot, say) is the one case that does not collapse to the bare-string
// shape, so it is also the shape that went unrecognised — and silently
// uncompressed — until "input_text" was added alongside "output_text"/"text".
func TestRewriteResponses_CompressesInputTextParts(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "take a screenshot and describe it"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "view_image", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{
				map[string]string{"type": "input_text", "text": bigLog()},
				map[string]string{"type": "input_image", "image_url": "data:image/png;base64,notarealimage"},
			}},
		},
	})

	out, stats, err := RewriteResponses(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged == 0 {
		t.Fatalf("nothing compressed (seen=%d protected=%d)", stats.BlocksSeen, stats.SkippedProtect)
	}
	if !strings.Contains(string(out), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
	if !strings.Contains(string(out), "data:image/png;base64,notarealimage") {
		t.Error("the image part was dropped or corrupted")
	}
}

func TestRewriteResponses_ProtectedToolIsUntouched(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "search"}}},
			map[string]any{"type": "function_call", "call_id": "call_7", "name": "Grep", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_7", "output": bigLog()},
		},
	})
	out, stats, err := RewriteResponses(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedProtect != 1 {
		t.Fatalf("Grep must be protected, skipped=%d", stats.SkippedProtect)
	}
	if string(out) != string(body) {
		t.Fatal("protected result must be byte-identical")
	}
}

// Only the newest turn's function_call_output is in the live zone; earlier
// ones must survive in full so the provider's prefix cache stays valid.
func TestRewriteResponses_OnlyTheLiveZoneIsTouched(t *testing.T) {
	items := []any{}
	for turn := 0; turn < 3; turn++ {
		id := "call_" + string(rune('a'+turn))
		items = append(items,
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "turn"}}},
			map[string]any{"type": "function_call", "call_id": id, "name": "bash", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": id, "output": bigLog()},
		)
	}
	body := mustJSON(t, map[string]any{"model": "codex-gpt-5.5", "input": items})

	out, stats, err := RewriteResponses(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged != 1 {
		t.Fatalf("only the newest tool output should be compressed, changed=%d", stats.BlocksChanged)
	}
	if got := strings.Count(string(out), "downloading module cache"); got < 2 {
		t.Errorf("older turns were compressed too (found %d full logs, want >= 2)", got)
	}
}

// A body that is not our shape, or has nothing to compress, comes back
// byte-identical rather than re-encoded.
func TestRewriteResponses_PassthroughIsByteIdentical(t *testing.T) {
	for name, body := range map[string]string{
		"not json": `not a json body at all`,
		"no input": `{"model":"codex-gpt-5.5"}`,
		"nothing big": `{"model":"codex-gpt-5.5","input":[{"type":"message","role":"user",` +
			`"content":[{"type":"input_text","text":"hi"}]}]}`,
	} {
		out, _, err := RewriteResponses([]byte(body), lossyPolicy())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(out) != body {
			t.Errorf("%s: body was re-encoded\n got: %s\nwant: %s", name, out, body)
		}
	}
}

// Older turns are outside the live zone. Rewriting them would change history
// the provider may already have cached.
func TestRewrite_OnlyTheLiveZoneIsTouched(t *testing.T) {
	msgs := []any{}
	for turn := 0; turn < 3; turn++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": "turn"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_" + string(rune('a'+turn)), "name": "Bash",
					"input": map[string]string{"command": "make"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result",
					"tool_use_id": "toolu_" + string(rune('a'+turn)), "content": bigLog()},
			}},
		)
	}
	body := mustJSON(t, map[string]any{"model": "claude-sonnet-4", "messages": msgs})

	out, stats, err := RewriteAnthropic(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged != 1 {
		t.Fatalf("only the newest tool result should be compressed, changed=%d", stats.BlocksChanged)
	}
	// The two older logs must still be present in full.
	if got := strings.Count(string(out), "downloading module cache"); got < 2 {
		t.Errorf("older turns were compressed too (found %d full logs, want >= 2)", got)
	}
}

// A body that is not our shape, or has nothing to compress, comes back
// byte-identical rather than re-encoded.
func TestRewrite_PassthroughIsByteIdentical(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     `not a json body at all`,
		"no messages":  `{"model":"gpt-4o"}`,
		"nothing big":  `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		"pretty float": `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":1.0}`,
	} {
		out, _, err := RewriteOpenAI([]byte(body), lossyPolicy())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(out) != body {
			t.Errorf("%s: body was re-encoded\n got: %s\nwant: %s", name, out, body)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// toolResultContent digs out the compressed text of one tool_result, so two
// requests can be compared at the position the provider's prefix cache cares
// about.
func toolResultContent(t *testing.T, body []byte, toolUseID string) string {
	t.Helper()
	var env struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	for _, m := range env.Messages {
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID == toolUseID {
				return b.Content
			}
		}
	}
	t.Fatalf("tool_result %s not found", toolUseID)
	return ""
}

func anthropicTurns(t *testing.T, pairs int) []byte {
	t.Helper()
	msgs := []any{
		map[string]any{"role": "user", "content": "run the tests"},
	}
	for i := 1; i <= pairs; i++ {
		id := "toolu_" + itoa(i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": "Bash",
					"input": map[string]string{"command": "go test ./..."}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": bigLog()},
			}},
		)
	}
	return mustJSON(t, map[string]any{"model": "claude-sonnet-4", "messages": msgs})
}

// Anthropic delivers tool_result blocks inside role="user" messages. Counting
// one as a user turn walks the live zone forward every request, so a block
// compressed on turn N is forwarded at full size on turn N+1 — the prompt then
// diverges at a position the provider already cached, which costs more than the
// compression saves. The bytes at that position must be identical across turns.
func TestRewriteAnthropic_ToolResultIsNotAUserTurn(t *testing.T) {
	pol := lossyPolicy()

	turn1, s1, err := RewriteAnthropic(anthropicTurns(t, 1), pol)
	if err != nil {
		t.Fatal(err)
	}
	turn2, s2, err := RewriteAnthropic(anthropicTurns(t, 2), pol)
	if err != nil {
		t.Fatal(err)
	}

	if got := toolResultContent(t, turn2, "toolu_1"); got != toolResultContent(t, turn1, "toolu_1") {
		t.Error("the first tool result changed between turns: the cached prefix is invalidated at that point")
	}
	if s1.BlocksChanged != 1 {
		t.Errorf("turn 1: compressed %d blocks, want 1", s1.BlocksChanged)
	}
	if s2.BlocksChanged != 2 {
		t.Errorf("turn 2: compressed %d blocks, want 2 — an older tool result was left uncompressed", s2.BlocksChanged)
	}
}

// The zone must still stop at a genuine user turn, or compression would reach
// into history the provider cache already covers.
func TestRewriteAnthropic_LiveZoneStopsAtRealUserTurn(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tests"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash",
					"input": map[string]string{"command": "go test ./..."}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": bigLog()},
			}},
			map[string]any{"role": "assistant", "content": "all green"},
			map[string]any{"role": "user", "content": "now run the linter"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Bash",
					"input": map[string]string{"command": "golangci-lint run"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": bigLog()},
			}},
		},
	})

	out, stats, err := RewriteAnthropic(body, lossyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BlocksChanged != 1 {
		t.Fatalf("compressed %d blocks, want only the one after the newest user turn", stats.BlocksChanged)
	}
	if toolResultContent(t, out, "toolu_1") != bigLog() {
		t.Error("a tool result before the newest user turn was rewritten")
	}
}
