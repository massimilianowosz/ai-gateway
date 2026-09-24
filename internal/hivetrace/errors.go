package hivetrace

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// maxErrorMessage bounds what a failed turn keeps of its error. Enough for a
// provider's explanation; not enough for an error page to land in the store.
const maxErrorMessage = 300

// upstreamErrorMessage reads the error the gateway sent the client, which is
// the one place the provider's own explanation survives. It covers a JSON
// error body and an error event inside a stream, where the status was already
// 200 by the time the failure happened.
func upstreamErrorMessage(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}
	if body[0] == '{' {
		return clip(errorFromJSON(body))
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok || !bytes.Contains(data, []byte(`"error"`)) {
			continue
		}
		if msg := errorFromJSON(bytes.TrimSpace(data)); msg != "" {
			return clip(msg)
		}
	}
	return ""
}

// errorFromJSON accepts the shapes the gateway writes: {"error":{"message"}}
// for OpenAI and Anthropic, {"error":"text"}, and a failed Responses stream,
// which nests it under response.
func errorFromJSON(b []byte) string {
	var v struct {
		Error    json.RawMessage `json:"error"`
		Response *struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	if v.Response != nil && v.Response.Error != nil && v.Response.Error.Message != "" {
		return v.Response.Error.Message
	}
	if len(v.Error) == 0 {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(v.Error, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	var s string
	if json.Unmarshal(v.Error, &s) == nil {
		return s
	}
	return ""
}

func clip(s string) string {
	if len(s) <= maxErrorMessage {
		return s
	}
	cut := maxErrorMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\u2026"
}
