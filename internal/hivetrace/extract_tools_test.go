package hivetrace

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseToolName(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		tool, server string
		source       string
	}{
		{"native", "Read", "Read", "", ToolSourceNative},
		{"double underscore mcp", "mcp__github__create_issue", "create_issue", "github", ToolSourceMCP},
		{"single underscore mcp", "mcp_sentry_list_errors", "list_errors", "sentry", ToolSourceMCP},
		{"mcp prefix alone is not a bridge", "mcp__", "mcp__", "", ToolSourceNative},
		{"empty server is not a bridge", "mcp____tool", "", "", ToolSourceNative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, server, source := parseToolName(tc.in)
			assert.Equal(t, tc.source, source)
			if tc.source == ToolSourceMCP {
				assert.Equal(t, tc.tool, tool)
				assert.Equal(t, tc.server, server)
			}
		})
	}
}

// parseToolName keeps the single-underscore split greedy on the server, which
// is the documented limitation; pin it so a change is deliberate.
func TestParseToolName_SingleUnderscoreSplitsOnFirstSegment(t *testing.T) {
	tool, server, source := parseToolName("mcp_my_server_do_thing")
	assert.Equal(t, ToolSourceMCP, source)
	assert.Equal(t, "my", server)
	assert.Equal(t, "server_do_thing", tool)
}

func TestExtractOpenAITools_NonStreaming(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/src/a.go\"}"}},
		{"id":"call_2","type":"function","function":{"name":"mcp__github__create_issue","arguments":"{\"title\":\"x\"}"}}
	]}}]}`)

	calls := extractToolCalls(APIChatCompletions, body)
	require.Len(t, calls, 2)

	assert.Equal(t, "Read", calls[0].Tool)
	assert.Equal(t, ToolSourceNative, calls[0].Source)
	assert.Equal(t, "call_1", calls[0].CallID)
	assert.Positive(t, calls[0].ArgumentsBytes)

	assert.Equal(t, "create_issue", calls[1].Tool)
	assert.Equal(t, "github", calls[1].Server)
	assert.Equal(t, ToolSourceMCP, calls[1].Source)
}

// Streaming tool arguments arrive as fragments keyed by index, with the name
// only on the opening delta. Reassembly is the whole point of the accumulator.
func TestExtractOpenAITools_StreamingReassemblesArguments(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Write","arguments":"{\"file_"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"path\":\"/tmp/out.txt\"}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))

	calls := extractToolCalls(APIChatCompletions, body)
	require.Len(t, calls, 1)
	assert.Equal(t, "Write", calls[0].Tool)
	assert.Equal(t, "call_a", calls[0].CallID)
	assert.JSONEq(t, `{"file_path":"/tmp/out.txt"}`, calls[0].arguments)
}

func TestExtractAnthropicTools_NonStreaming(t *testing.T) {
	body := []byte(`{"content":[
		{"type":"text","text":"let me look"},
		{"type":"tool_use","id":"toolu_1","name":"Grep","input":{"pattern":"func main","path":"/src"}}
	]}`)

	calls := extractToolCalls(APIMessages, body)
	require.Len(t, calls, 1)
	assert.Equal(t, "Grep", calls[0].Tool)
	assert.Equal(t, "toolu_1", calls[0].CallID)
}

func TestExtractAnthropicTools_StreamingIgnoresTextBlocks(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"thinking"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"Edit"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"/src/b.go\"}"}}`,
		``,
	}, "\n\n"))

	calls := extractToolCalls(APIMessages, body)
	require.Len(t, calls, 1, "a text block must not be collected as a tool call")
	assert.Equal(t, "Edit", calls[0].Tool)
	assert.JSONEq(t, `{"file_path":"/src/b.go"}`, calls[0].arguments)
}

func TestExtractResponsesTools_NonStreaming(t *testing.T) {
	body := []byte(`{"output":[
		{"type":"message","content":[{"type":"output_text","text":"ok"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_z","name":"Glob","arguments":"{\"path\":\"/src\"}"}
	]}`)

	calls := extractToolCalls(APIResponses, body)
	require.Len(t, calls, 1)
	assert.Equal(t, "Glob", calls[0].Tool)
	assert.Equal(t, "call_z", calls[0].CallID)
}

// The done event carries the assembled arguments and must win over the
// concatenated deltas, which can lose a chunk.
func TestExtractResponsesTools_StreamingDonePayloadWins(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_z","name":"Read"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"file_"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"file_path\":\"/src/c.go\"}"}`,
		``,
	}, "\n\n"))

	calls := extractToolCalls(APIResponses, body)
	require.Len(t, calls, 1)
	assert.JSONEq(t, `{"file_path":"/src/c.go"}`, calls[0].arguments)
}

func TestExtractToolCalls_IgnoresGarbage(t *testing.T) {
	assert.Empty(t, extractToolCalls(APIChatCompletions, nil))
	assert.Empty(t, extractToolCalls(APIChatCompletions, []byte("not json")))
	assert.Empty(t, extractToolCalls(APIMessages, []byte(`{"content":[]}`)))
}
