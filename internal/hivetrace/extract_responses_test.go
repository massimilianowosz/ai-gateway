package hivetrace

import "testing"

// Codex does not use function_call: every tool it runs arrives as a
// custom_tool_call, which an extractor that only knew function_call dropped,
// making a busy session look idle.
func TestExtractResponsesCustomToolCall(t *testing.T) {
	body := []byte(`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","id":"ci_1","call_id":"call_1","name":"shell","input":"{\"command\":[\"ls\"]}"}}

data: [DONE]
`)
	got := extractToolCalls(APIResponses, body)
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].Tool != "shell" || got[0].Source != ToolSourceNative {
		t.Fatalf("got tool=%q source=%q", got[0].Tool, got[0].Source)
	}
	if got[0].ArgumentsBytes == 0 {
		t.Error("arguments size not recorded")
	}
}

func TestExtractResponsesBuiltinCalls(t *testing.T) {
	// The built-ins encode the tool in the item type rather than a name.
	cases := map[string]string{
		"local_shell_call":      "shell",
		"web_search_call":       "web_search",
		"file_search_call":      "file_search",
		"code_interpreter_call": "code_interpreter",
		"computer_call":         "computer_use",
	}
	for itemType, want := range cases {
		body := []byte(`data: {"type":"response.output_item.done","item":{"type":"` +
			itemType + `","id":"i1","call_id":"c1"}}` + "\n")
		got := extractToolCalls(APIResponses, body)
		if len(got) != 1 || got[0].Tool != want {
			t.Errorf("%s: got %v, want tool %q", itemType, got, want)
		}
	}
}

func TestExtractResponsesMCPCall(t *testing.T) {
	// The Responses API carries remote MCP natively, naming the server in
	// server_label rather than in the mcp__server__tool convention.
	body := []byte(`data: {"type":"response.output_item.done","item":{"type":"mcp_call",` +
		`"id":"m1","call_id":"c1","name":"list_events","server_label":"google_calendar",` +
		`"arguments":"{}"}}` + "\n")
	got := extractToolCalls(APIResponses, body)
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].Server != "google_calendar" || got[0].Tool != "list_events" || got[0].Source != ToolSourceMCP {
		t.Fatalf("got server=%q tool=%q source=%q", got[0].Server, got[0].Tool, got[0].Source)
	}
}

func TestExtractResponsesIgnoresNonCalls(t *testing.T) {
	body := []byte(`data: {"type":"response.output_item.done","item":{"type":"message","id":"m1"}}
data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"r1"}}
`)
	if got := extractToolCalls(APIResponses, body); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}
