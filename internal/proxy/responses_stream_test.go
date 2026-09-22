package proxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// capturingSink records the events a stream state emits.
type capturingSink struct {
	events []map[string]interface{}
}

func (c *capturingSink) writeEvent(eventType string, payload []byte) {
	var decoded map[string]interface{}
	if json.Unmarshal(payload, &decoded) == nil {
		c.events = append(c.events, decoded)
	}
}

// A provider may open a tool call's state with arguments before its name
// arrives. output_index only advanced when a call *started*, so two states
// created before either could start both held the same index and their
// response.output_item.added events collided on it.
func TestResponsesStream_ToolCallsGetDistinctOutputIndexes(t *testing.T) {
	sink := &capturingSink{}
	state := newResponsesStreamState(newResponsesStreamWriterTo(sink))

	// Call 0 arrives id-first, with no name yet: its state exists but cannot start.
	state.appendToolCall(0, "call_0", "", "")
	// Call 1 arrives complete and starts immediately.
	state.appendToolCall(1, "call_1", "beta", "")
	// Call 0's name finally lands and it starts too.
	state.appendToolCall(0, "", "alpha", "")

	indexes := map[float64]string{}
	for _, ev := range sink.events {
		if ev["type"] != "response.output_item.added" {
			continue
		}
		idx, ok := ev["output_index"].(float64)
		require.True(t, ok, "output_index missing from %v", ev)
		item, _ := ev["item"].(map[string]interface{})
		callID, _ := item["call_id"].(string)
		if prev, dup := indexes[idx]; dup {
			t.Fatalf("output_index %v claimed by both %s and %s", idx, prev, callID)
		}
		indexes[idx] = callID
	}
	require.Len(t, indexes, 2, "both tool calls should have opened")
}
