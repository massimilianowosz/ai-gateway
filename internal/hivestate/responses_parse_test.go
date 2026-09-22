package hivestate

import (
	"encoding/json"
	"testing"
)

// Codex does not use function_call. While its item types were unhandled the
// parser saw only the plain messages, so every token count — and therefore the
// threshold check and the whole cost comparison — was computed against a small
// fraction of the real conversation.
func TestParseResponsesMessages_ReadsCodexItemTypes(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"instructions": "be helpful",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "find the bug"},
			{"type": "custom_tool_call", "call_id": "c1", "name": "shell", "input": "rg -n panic"},
			{"type": "custom_tool_call_output", "call_id": "c1", "output": "main.go:42: panic(err)"},
			{"type": "local_shell_call", "call_id": "c2", "action": map[string]any{"command": []string{"go", "build"}}},
			{"type": "function_call_output", "call_id": "c2", "output": "build failed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := parseResponsesMessages(body)
	if len(msgs) != 6 {
		t.Fatalf("parsed %d messages, want 6 (system + user + 2 calls + 2 outputs): %+v", len(msgs), msgs)
	}

	joined := ""
	for _, m := range msgs {
		joined += m.Role + ":" + m.Content + "\n"
	}
	for _, want := range []string{"rg -n panic", "main.go:42: panic(err)", "go", "build failed"} {
		if !contains(joined, want) {
			t.Errorf("parsed messages lost %q\ngot:\n%s", want, joined)
		}
	}

	if msgs[2].ToolCallID != "" && msgs[3].ToolCallID != "c1" {
		t.Errorf("tool output lost its call id: %+v", msgs[3])
	}
}

// The token count is what the threshold and the cache guard compare against, so
// tool output has to be part of it. A body that is mostly tool output must not
// look small.
func TestParseResponsesMessages_CountsToolOutputTowardsSize(t *testing.T) {
	big := make([]byte, 0, 4000)
	for len(big) < 4000 {
		big = append(big, "output line with detail "...)
	}
	body, err := json.Marshal(map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "go"},
			{"type": "custom_tool_call_output", "call_id": "c1", "output": string(big)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	counter := NewTokenCounter()
	withOutput := counter.CountMessages(parseResponsesMessages(body))
	if withOutput < 100 {
		t.Fatalf("a body dominated by tool output counted %d tokens; it is being ignored", withOutput)
	}
}

// The count feeds the threshold and every cache-cost comparison, so it has to
// track the real size of the request. A truncating parser made a 4000-character
// output and a 40000-character one look the same, and the guard then weighed a
// third of the real prompt.
func TestParseResponsesMessages_CountGrowsWithRealSize(t *testing.T) {
	bodyWith := func(chars int) []byte {
		out := make([]byte, 0, chars)
		for len(out) < chars {
			out = append(out, "output line with detail "...)
		}
		b, err := json.Marshal(map[string]any{
			"input": []map[string]any{
				{"type": "message", "role": "user", "content": "go"},
				{"type": "custom_tool_call_output", "call_id": "c1", "output": string(out)},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	counter := NewTokenCounter()
	small := counter.CountMessages(parseResponsesMessages(bodyWith(4000)))
	large := counter.CountMessages(parseResponsesMessages(bodyWith(40000)))

	if large < small*5 {
		t.Fatalf("a 10x larger body counted %d tokens against %d; the count is being clipped", large, small)
	}
}

// The tool schema is the largest permanently-frozen part of a Codex request.
// Ignoring it hid the most cacheable content from the guard.
func TestParseResponsesMessages_IncludesToolSchema(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"input": []map[string]any{
			{"type": "additional_tools", "role": "developer", "tools": map[string]any{"shell": "run a command"}},
			{"type": "message", "role": "user", "content": "go"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := parseResponsesMessages(body)
	if len(msgs) != 2 || msgs[0].Role != "developer" {
		t.Fatalf("tool schema missing from the parsed view: %+v", msgs)
	}
	if !contains(msgs[0].Content, "shell") {
		t.Errorf("tool schema parsed but empty: %q", msgs[0].Content)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
