package hivetrace

import (
	"encoding/json"
	"strings"
)

// extractResponseText assembles the assistant text a turn produced.
//
// It exists for two reasons. Secret detection over a raw SSE body misses any
// credential that happened to straddle two chunks, because each chunk is a
// separate JSON document; reassembling first makes the scan see the text the
// user sees. And a stored body is far more useful — and an order of magnitude
// smaller — as the answer than as a few hundred `data:` frames.
func extractResponseText(api string, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var b strings.Builder
	switch api {
	case APIMessages:
		appendAnthropicText(&b, body)
	case APIResponses:
		appendResponsesText(&b, body)
	default:
		appendOpenAIText(&b, body)
	}
	return b.String()
}

func appendOpenAIText(b *strings.Builder, body []byte) {
	parse := func(chunk []byte) {
		var doc struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(chunk, &doc) != nil {
			return
		}
		for _, c := range doc.Choices {
			b.WriteString(c.Message.Content)
			b.WriteString(c.Delta.Content)
		}
	}
	if isJSONBody(body) {
		parse(body)
		return
	}
	for _, chunk := range sseData(body) {
		parse(chunk)
	}
}

func appendAnthropicText(b *strings.Builder, body []byte) {
	if isJSONBody(body) {
		var doc struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &doc) != nil {
			return
		}
		for _, block := range doc.Content {
			if block.Type == "text" {
				b.WriteString(block.Text)
			}
		}
		return
	}
	for _, chunk := range sseData(body) {
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(chunk, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "text" {
				b.WriteString(ev.ContentBlock.Text)
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				b.WriteString(ev.Delta.Text)
			}
		}
	}
}

func appendResponsesText(b *strings.Builder, body []byte) {
	appendOutput := func(output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}) {
		for _, item := range output {
			if item.Type != "message" {
				continue
			}
			for _, c := range item.Content {
				if c.Type == "output_text" {
					b.WriteString(c.Text)
				}
			}
		}
	}

	if isJSONBody(body) {
		var doc struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(body, &doc) != nil {
			return
		}
		appendOutput(doc.Output)
		return
	}

	// Deltas alone reconstruct the answer, so the terminal response.completed
	// event is skipped: absorbing both would duplicate every word.
	for _, chunk := range sseData(body) {
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(chunk, &ev) != nil {
			continue
		}
		if ev.Type == "response.output_text.delta" {
			b.WriteString(ev.Delta)
		}
	}
}
