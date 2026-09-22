package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustRewrite(t *testing.T, body, newText string, isAnthropic, isResponses bool) string {
	t.Helper()
	_, lastUser, idx := extractPrompt([]byte(body), isAnthropic, isResponses)
	if lastUser == "" {
		t.Fatalf("extractor found no prompt in %s", body)
	}
	out, err := rewriteLastUserText([]byte(body), newText, idx, isAnthropic, isResponses)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	return string(out)
}

// The extractor skips a turn whose content carries no text, so the last user
// turn it chose is not always the last user-role message. Rewriting by role
// alone wrote the workflow's answer into a different message than the one it
// was given — destroying an image and leaving the real prompt untouched.
func TestRewrite_TargetsTheTurnTheExtractorRead(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[
		{"role":"user","content":"descrivi questa foto"},
		{"role":"assistant","content":"certo"},
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}
	]}`

	out := mustRewrite(t, body, "PROMPT RISCRITTO", false, false)

	if !strings.Contains(out, "https://x/y.png") {
		t.Errorf("the image was destroyed:\n%s", out)
	}
	if strings.Contains(out, `"descrivi questa foto"`) {
		t.Errorf("the turn the extractor actually read was left untouched:\n%s", out)
	}
	if !strings.Contains(out, "PROMPT RISCRITTO") {
		t.Errorf("the rewrite did not land:\n%s", out)
	}
}

// Anthropic carries tool results inside user messages. contentText returns ""
// for one, so the extractor skips it — but the rewrite used to overwrite it
// with a plain string anyway, and the provider then rejects the request with
// "tool_use ids were found without tool_result blocks".
func TestRewrite_AnthropicToolResultSurvives(t *testing.T) {
	body := `{"model":"claude-sonnet-4","messages":[
		{"role":"user","content":"esegui i test"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
	]}`

	out := mustRewrite(t, body, "PROMPT RISCRITTO", true, false)

	if !strings.Contains(out, "tool_result") || !strings.Contains(out, "toolu_1") {
		t.Fatalf("the tool_result block was clobbered — the provider would 400:\n%s", out)
	}
}

// A mixed array keeps its non-text parts, and the joined text collapses back
// into a single block rather than being duplicated.
func TestRewrite_MixedContentKeepsNonTextParts(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[
		{"type":"text","text":"prima parte"},
		{"type":"image_url","image_url":{"url":"https://x/y.png"}},
		{"type":"text","text":"seconda parte"}
	]}]}`

	out := mustRewrite(t, body, "UNICO", false, false)

	if !strings.Contains(out, "https://x/y.png") {
		t.Errorf("the image was dropped:\n%s", out)
	}
	if strings.Contains(out, "prima parte") || strings.Contains(out, "seconda parte") {
		t.Errorf("the replaced text survived:\n%s", out)
	}
	if n := strings.Count(out, "UNICO"); n != 1 {
		t.Errorf("expected the new text once, got %d:\n%s", n, out)
	}
}

// A Responses item is edited in place: rebuilding it from scratch discarded its
// id and status and every input_image part.
func TestRewrite_ResponsesItemKeepsIDAndAttachments(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{
		"id":"msg_abc","type":"message","status":"completed","role":"user","content":[
			{"type":"input_text","text":"guarda"},
			{"type":"input_image","image_url":"https://x/y.png"}
		]}]}`

	out := mustRewrite(t, body, "RISCRITTO", false, true)

	for _, want := range []string{`"msg_abc"`, `"completed"`, "input_image", "https://x/y.png", "RISCRITTO"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s is missing from the rewritten item:\n%s", want, out)
		}
	}
}

// A bare-string Responses input is still replaced wholesale.
func TestRewrite_ResponsesPlainStringInput(t *testing.T) {
	out := mustRewrite(t, `{"model":"gpt-4o","input":"originale"}`, "RISCRITTO", false, true)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var input string
	if err := json.Unmarshal(doc["input"], &input); err != nil {
		t.Fatalf("input is no longer a string: %s", out)
	}
	if input != "RISCRITTO" {
		t.Errorf("input = %q, want RISCRITTO", input)
	}
}
