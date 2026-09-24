package hivetrace

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// toolCall is an extracted call plus its arguments. The arguments stay inside
// the package: they routinely carry file contents, diffs and credentials, so
// they are used to derive file accesses and a size, then dropped.
type toolCall struct {
	ToolInvocation
	arguments string
}

// extractToolCalls recovers the tool calls a model asked for from a captured
// response body, streamed or not.
//
// Extraction reads responses rather than requests on purpose. A request
// carries the whole conversation, so the same call would be counted again on
// every following turn; a response carries only what the model just decided,
// which makes each call appear exactly once in a session.
func extractToolCalls(api string, body []byte) []toolCall {
	if len(body) == 0 {
		return nil
	}
	switch api {
	case APIMessages:
		return extractAnthropicTools(body)
	case APIResponses:
		return extractResponsesTools(body)
	default:
		return extractOpenAITools(body)
	}
}

// isJSONBody reports whether the body is a plain JSON document rather than an
// SSE stream.
func isJSONBody(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{"))
}

// sseData returns the payload of every `data:` line, skipping the terminator.
func sseData(body []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		out = append(out, payload)
	}
	return out
}

// accumulator assembles calls whose arguments arrive in fragments, keyed by
// whatever the surface uses to correlate deltas.
type accumulator struct {
	order []string
	byKey map[string]*toolCall
}

func newAccumulator() *accumulator {
	return &accumulator{byKey: make(map[string]*toolCall)}
}

func (a *accumulator) get(key string) *toolCall {
	if tc, ok := a.byKey[key]; ok {
		return tc
	}
	tc := &toolCall{}
	a.byKey[key] = tc
	a.order = append(a.order, key)
	return tc
}

func (a *accumulator) result() []toolCall {
	out := make([]toolCall, 0, len(a.order))
	for _, k := range a.order {
		tc := a.byKey[k]
		if tc.Name == "" {
			continue
		}
		// A server already set by the extractor came from the protocol itself
		// (an mcp_call's server_label), which beats guessing it from a name.
		if tc.Server == "" {
			tc.Tool, tc.Server, tc.Source = parseToolName(tc.Name)
		} else {
			tc.Tool = tc.Name
		}
		tc.ArgumentsBytes = len(tc.arguments)
		out = append(out, *tc)

		// A shell call may be carrying a connector invocation rather than a
		// command; the connector is the part worth recording.
		if shellTools[strings.ToLower(tc.Tool)] {
			for _, bridged := range bridgedToolCalls(tc.arguments) {
				bridged.ArgumentsBytes = len(bridged.arguments)
				out = append(out, bridged)
			}
		}
	}
	return out
}

// --- OpenAI chat completions ---

type openAIToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func extractOpenAITools(body []byte) []toolCall {
	acc := newAccumulator()

	absorb := func(calls []openAIToolCall, fallbackKey string) {
		for i, c := range calls {
			// Streaming deltas identify a call by index and only carry the id
			// on the opening fragment, so the index is the stable key.
			key := fallbackKey + ":" + strconv.Itoa(i)
			if c.Index != nil {
				key = fallbackKey + ":" + strconv.Itoa(*c.Index)
			} else if c.ID != "" {
				key = c.ID
			}
			tc := acc.get(key)
			if c.ID != "" {
				tc.CallID = c.ID
			}
			if c.Function.Name != "" {
				tc.Name = c.Function.Name
			}
			tc.arguments += c.Function.Arguments
		}
	}

	parse := func(chunk []byte, key string) {
		var doc struct {
			Choices []struct {
				Message struct {
					ToolCalls []openAIToolCall `json:"tool_calls"`
				} `json:"message"`
				Delta struct {
					ToolCalls []openAIToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(chunk, &doc) != nil {
			return
		}
		for i, ch := range doc.Choices {
			absorb(ch.Message.ToolCalls, key+strconv.Itoa(i))
			absorb(ch.Delta.ToolCalls, key+strconv.Itoa(i))
		}
	}

	if isJSONBody(body) {
		parse(body, "c")
	} else {
		for _, chunk := range sseData(body) {
			parse(chunk, "c")
		}
	}
	return acc.result()
}

// --- Anthropic messages ---

func extractAnthropicTools(body []byte) []toolCall {
	acc := newAccumulator()

	if isJSONBody(body) {
		var doc struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &doc) != nil {
			return nil
		}
		for i, block := range doc.Content {
			if block.Type != "tool_use" {
				continue
			}
			tc := acc.get(block.ID + ":" + strconv.Itoa(i))
			tc.CallID = block.ID
			tc.Name = block.Name
			tc.arguments = string(block.Input)
		}
		return acc.result()
	}

	for _, chunk := range sseData(body) {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(chunk, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type != "tool_use" {
				continue
			}
			tc := acc.get(strconv.Itoa(ev.Index))
			tc.CallID = ev.ContentBlock.ID
			tc.Name = ev.ContentBlock.Name
		case "content_block_delta":
			if ev.Delta.Type != "input_json_delta" {
				continue
			}
			// Only blocks opened as tool_use are tracked; a delta for a text
			// block has no entry and must not create one.
			if tc, ok := acc.byKey[strconv.Itoa(ev.Index)]; ok {
				tc.arguments += ev.Delta.PartialJSON
			}
		}
	}
	return acc.result()
}

// --- Responses API ---

type responsesOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Input is what a custom tool call carries instead of Arguments. Codex
	// uses this shape for every tool it runs, which is why reading only
	// function_call left its sessions looking idle.
	Input string `json:"input"`
	// ServerLabel names the remote MCP server on an mcp_call, the Responses
	// API's own MCP transport. It is the server outright, so it does not go
	// through the mcp__server__tool name convention.
	ServerLabel string `json:"server_label"`
}

// responsesToolName maps an output item to the tool it represents.
//
// The Responses API does not have one call type: a function_call carries its
// own name, while the built-ins encode the tool in the item type itself. An
// extractor that only knew function_call reported no tools at all for clients
// built on the others.
func responsesToolName(item responsesOutputItem) string {
	switch item.Type {
	case "function_call", "custom_tool_call", "mcp_call":
		return item.Name
	case "local_shell_call":
		return "shell"
	case "computer_call":
		return "computer_use"
	case "web_search_call":
		return "web_search"
	case "file_search_call":
		return "file_search"
	case "code_interpreter_call":
		return "code_interpreter"
	case "image_generation_call":
		return "image_generation"
	default:
		return ""
	}
}

func extractResponsesTools(body []byte) []toolCall {
	acc := newAccumulator()

	absorbOutput := func(items []responsesOutputItem) {
		for i, item := range items {
			name := responsesToolName(item)
			if name == "" {
				continue
			}
			key := item.ID
			if key == "" {
				key = item.CallID
			}
			if key == "" {
				key = "o" + strconv.Itoa(i)
			}
			tc := acc.get(key)
			tc.CallID = item.CallID
			tc.Name = name
			if item.ServerLabel != "" {
				tc.Server = item.ServerLabel
				tc.Source = ToolSourceMCP
			}
			if item.Arguments != "" {
				tc.arguments = item.Arguments
			} else if item.Input != "" {
				tc.arguments = item.Input
			}
		}
	}

	if isJSONBody(body) {
		var doc struct {
			Output []responsesOutputItem `json:"output"`
		}
		if json.Unmarshal(body, &doc) != nil {
			return nil
		}
		absorbOutput(doc.Output)
		return acc.result()
	}

	for _, chunk := range sseData(body) {
		var ev struct {
			Type      string              `json:"type"`
			ItemID    string              `json:"item_id"`
			Delta     string              `json:"delta"`
			Arguments string              `json:"arguments"`
			Item      responsesOutputItem `json:"item"`
			Response  struct {
				Output []responsesOutputItem `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(chunk, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_item.added", "response.output_item.done":
			absorbOutput([]responsesOutputItem{ev.Item})
		case "response.function_call_arguments.delta":
			if tc, ok := acc.byKey[ev.ItemID]; ok {
				tc.arguments += ev.Delta
			}
		case "response.function_call_arguments.done":
			// The done event carries the assembled arguments, which is more
			// trustworthy than a concatenation that may have lost a chunk.
			if tc, ok := acc.byKey[ev.ItemID]; ok && ev.Arguments != "" {
				tc.arguments = ev.Arguments
			}
		case "response.completed", "response.incomplete":
			absorbOutput(ev.Response.Output)
		}
	}
	return acc.result()
}

// parseToolName splits a tool name into the bare tool and, when it arrived
// through an MCP bridge, the server that serves it.
//
// Both spellings agents use in the wild are handled: mcp__server__tool and
// mcp_server_tool. A server whose own name contains an underscore is
// misattributed by the single-underscore form, which is unavoidable without a
// registry — the double-underscore form, which every current client emits, is
// unambiguous.
func parseToolName(name string) (tool, server, source string) {
	raw := strings.TrimSpace(name)
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "mcp__"):
		if parts := strings.SplitN(raw[len("mcp__"):], "__", 2); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[1], parts[0], ToolSourceMCP
		}
	case strings.HasPrefix(lower, "mcp_"):
		if parts := strings.SplitN(raw[len("mcp_"):], "_", 2); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[1], parts[0], ToolSourceMCP
		}
	}
	return raw, "", ToolSourceNative
}
