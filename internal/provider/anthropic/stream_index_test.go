package anthropic

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Anthropic numbers every content block; OpenAI numbers only tool calls. The
// reader emitted 0 for every tool call, so a relay treated the second parallel
// call as an update to the first and concatenated both argument streams into
// one unparseable blob.
func TestStreamReader_ParallelToolCallsGetDistinctIndexes(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"penso"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_a","name":"alpha"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_b","name":"beta"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"y\":2}"}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	r := &streamReader{
		reader: bufio.NewReader(strings.NewReader(sse)),
		body:   io.NopCloser(strings.NewReader("")),
		model:  "claude-sonnet-4",
	}

	argsByIndex := map[float64]string{}
	nameByIndex := map[float64]string{}
	for {
		chunk, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		var parsed struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    float64 `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(chunk, &parsed) != nil || len(parsed.Choices) == 0 {
			continue
		}
		for _, tc := range parsed.Choices[0].Delta.ToolCalls {
			if tc.Function.Name != "" {
				nameByIndex[tc.Index] = tc.Function.Name
			}
			argsByIndex[tc.Index] += tc.Function.Arguments
		}
	}

	if len(nameByIndex) != 2 {
		t.Fatalf("expected two distinct tool indexes, got %d: %v", len(nameByIndex), nameByIndex)
	}
	if nameByIndex[0] != "alpha" || nameByIndex[1] != "beta" {
		t.Errorf("tool ordinals are wrong: %v", nameByIndex)
	}
	for idx, args := range argsByIndex {
		if !json.Valid([]byte(args)) {
			t.Errorf("tool %v (%s) carries unparseable arguments %q — two payloads were merged",
				idx, nameByIndex[idx], args)
		}
	}
}
