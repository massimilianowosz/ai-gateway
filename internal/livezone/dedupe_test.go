package livezone

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func fileContents(name string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "package %s\n\n", name)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "func handler%dInThe%sPackage(ctx context.Context) error { return nil }\n", i, name)
	}
	return sb.String()
}

// A coding agent reads the same file several times in a session — after an
// edit, after a failing test, to check itself — and every reading enters the
// prompt in full.
func repeatedReadBody(t *testing.T, contents string) []byte {
	t.Helper()
	msgs := []map[string]any{
		{"role": "user", "content": "look at the gateway package"},
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1", "content": contents},
		}},
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "t2", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t2", "content": contents},
		}},
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "t3", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t3", "content": contents},
		}},
	}
	body, err := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func dedupePolicy() Policy {
	pol := DefaultPolicy()
	pol.LiveTurns = 9999 // whole conversation live
	pol.DedupeRepeats = true
	return pol
}

// The first copy stays byte-for-byte, so every offset the agent might quote
// back still resolves. Only the repeats become a reference.
func TestDedupeKeepsFirstCopyAndReplacesLater(t *testing.T) {
	contents := fileContents("gateway")
	body := repeatedReadBody(t, contents)

	out, stats, err := RewriteAnthropic(body, dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 2 {
		t.Fatalf("deduped %d results, want 2", stats.Transformers["dedupe_repeat"])
	}
	if len(out) >= len(body) {
		t.Errorf("bytes %d -> %d, expected a reduction", len(body), len(out))
	}

	got := string(out)
	if n := strings.Count(got, "handler39InThegatewayPackage"); n != 1 {
		t.Errorf("file contents appear %d times, want 1", n)
	}
	if !strings.Contains(got, "identical to the earlier Read result") {
		t.Errorf("the reference does not name what it points at:\n%s", firstMarker(got))
	}
}

// Dedupe applies to protected tools on purpose: removing a *copy* shifts no
// offsets, because the original is still there. Compressing that original is
// what stays forbidden.
func TestDedupeAppliesToProtectedToolsButTransformDoesNot(t *testing.T) {
	body := repeatedReadBody(t, fileContents("gateway"))
	_, stats, err := RewriteAnthropic(body, dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] == 0 {
		t.Error("a repeated Read was left in full")
	}
	if stats.SkippedProtect == 0 {
		t.Error("Read was not treated as protected by the content transformers")
	}
}

// Off by default: nothing changes unless an operator asks for it.
func TestDedupeIsOptIn(t *testing.T) {
	body := repeatedReadBody(t, fileContents("gateway"))
	pol := DefaultPolicy()
	pol.LiveTurns = 9999

	out, stats, err := RewriteAnthropic(body, pol)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 0 {
		t.Error("deduped without being asked to")
	}
	if len(out) != len(body) {
		t.Errorf("body changed with every transformer off: %d -> %d", len(body), len(out))
	}
}

// Two different files must never collapse into one another.
func TestDedupeDistinguishesDifferentContents(t *testing.T) {
	a := fileContents("gateway")
	b := fileContents("router")
	msgs := []map[string]any{
		{"role": "user", "content": "compare them"},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Read"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": a}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t2", "name": "Read"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t2", "content": b}}},
	}
	body, err := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}

	out, stats, err := RewriteAnthropic(body, dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 0 {
		t.Error("collapsed two different files into one")
	}
	if !strings.Contains(string(out), "InTherouterPackage") {
		t.Error("the second file's contents were lost")
	}
}

// The same body must produce the same bytes every turn, or the provider's
// prefix cache misses on every request and the saving is worse than nothing.
func TestDedupeIsDeterministic(t *testing.T) {
	body := repeatedReadBody(t, fileContents("gateway"))

	first, _, err := RewriteAnthropic(append([]byte(nil), body...), dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := RewriteAnthropic(append([]byte(nil), body...), dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("two runs over the same body produced different bytes")
	}
}

// A result too small to matter is left alone: the reference would cost more
// than the repeat.
func TestDedupeIgnoresSmallResults(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "run it twice"},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t2", "name": "Bash"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t2", "content": "ok"}}},
	}
	body, err := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}

	_, stats, err := RewriteAnthropic(body, dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 0 {
		t.Error("replaced a two-byte result with a longer reference")
	}
}

func firstMarker(s string) string {
	if i := strings.Index(s, "identical to"); i >= 0 {
		end := i + 120
		if end > len(s) {
			end = len(s)
		}
		return s[i:end]
	}
	return "(no marker found)"
}

// dedupe_repeats must behave identically across the three wire formats: same
// hashing, same "first copy stays, later copies become a reference" rule,
// same marker wording. Only the envelope shape differs.

func repeatedReadBodyOpenAI(t *testing.T, contents string) []byte {
	t.Helper()
	msgs := []map[string]any{
		{"role": "user", "content": "look at the gateway package"},
		{"role": "assistant", "tool_calls": []map[string]any{
			{"id": "t1", "type": "function", "function": map[string]string{"name": "Read", "arguments": "{}"}},
		}},
		{"role": "tool", "tool_call_id": "t1", "content": contents},
		{"role": "assistant", "tool_calls": []map[string]any{
			{"id": "t2", "type": "function", "function": map[string]string{"name": "Read", "arguments": "{}"}},
		}},
		{"role": "tool", "tool_call_id": "t2", "content": contents},
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-4o", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDedupeOpenAI_KeepsFirstCopyAndReplacesLater(t *testing.T) {
	contents := fileContents("gateway")
	body := repeatedReadBodyOpenAI(t, contents)

	out, stats, err := RewriteOpenAI(body, dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 1 {
		t.Fatalf("deduped %d results, want 1", stats.Transformers["dedupe_repeat"])
	}
	if len(out) >= len(body) {
		t.Errorf("bytes %d -> %d, expected a reduction", len(body), len(out))
	}

	got := string(out)
	if n := strings.Count(got, "handler39InThegatewayPackage"); n != 1 {
		t.Errorf("file contents appear %d times, want 1", n)
	}
	if !strings.Contains(got, "identical to the earlier Read result") {
		t.Errorf("the reference does not name what it points at:\n%s", firstMarker(got))
	}
}

func TestDedupeOpenAI_IsOptIn(t *testing.T) {
	body := repeatedReadBodyOpenAI(t, fileContents("gateway"))
	pol := DefaultPolicy()
	pol.LiveTurns = 9999

	out, stats, err := RewriteOpenAI(body, pol)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 0 {
		t.Error("deduped without being asked to")
	}
	if len(out) != len(body) {
		t.Errorf("body changed with every transformer off: %d -> %d", len(body), len(out))
	}
}

func repeatedReadBodyResponses(t *testing.T, contents string) []byte {
	t.Helper()
	items := []map[string]any{
		{"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": "look at the gateway package"}}},
		{"type": "custom_tool_call", "call_id": "t1", "name": "Read", "input": "a.go"},
		{"type": "custom_tool_call_output", "call_id": "t1", "output": contents},
		{"type": "custom_tool_call", "call_id": "t2", "name": "Read", "input": "a.go"},
		{"type": "custom_tool_call_output", "call_id": "t2", "output": contents},
	}
	body, err := json.Marshal(map[string]any{"model": "codex-gpt-5.5", "input": items})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDedupeResponses_KeepsFirstCopyAndReplacesLater(t *testing.T) {
	contents := fileContents("gateway")
	body := repeatedReadBodyResponses(t, contents)

	out, stats, err := RewriteResponses(body, dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 1 {
		t.Fatalf("deduped %d results, want 1", stats.Transformers["dedupe_repeat"])
	}
	if len(out) >= len(body) {
		t.Errorf("bytes %d -> %d, expected a reduction", len(body), len(out))
	}

	got := string(out)
	if n := strings.Count(got, "handler39InThegatewayPackage"); n != 1 {
		t.Errorf("file contents appear %d times, want 1", n)
	}
	if !strings.Contains(got, "identical to the earlier Read result") {
		t.Errorf("the reference does not name what it points at:\n%s", firstMarker(got))
	}
}

func TestDedupeResponses_IsOptIn(t *testing.T) {
	body := repeatedReadBodyResponses(t, fileContents("gateway"))
	pol := DefaultPolicy()
	pol.LiveTurns = 9999

	out, stats, err := RewriteResponses(body, pol)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 0 {
		t.Error("deduped without being asked to")
	}
	if len(out) != len(body) {
		t.Errorf("body changed with every transformer off: %d -> %d", len(body), len(out))
	}
}

// Codex CLI's own function_call_output carries its text as
// [{"type":"input_text","text":"..."}], not a bare string — the shape
// resultText must also recognise, or a repeated structured result is never
// even seen as a duplicate.
func TestDedupeResponses_RecognisesInputTextParts(t *testing.T) {
	contents := fileContents("gateway")
	structured := []map[string]string{{"type": "input_text", "text": contents}}
	items := []map[string]any{
		{"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": "look at the gateway package"}}},
		{"type": "function_call", "call_id": "t1", "name": "Read", "arguments": `{"file":"a.go"}`},
		{"type": "function_call_output", "call_id": "t1", "output": structured},
		{"type": "function_call", "call_id": "t2", "name": "Read", "arguments": `{"file":"a.go"}`},
		{"type": "function_call_output", "call_id": "t2", "output": structured},
	}
	body, err := json.Marshal(map[string]any{"model": "codex-gpt-5.5", "input": items})
	if err != nil {
		t.Fatal(err)
	}

	out, stats, err := RewriteResponses(body, dedupePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_repeat"] != 1 {
		t.Fatalf("deduped %d results, want 1", stats.Transformers["dedupe_repeat"])
	}
	if len(out) >= len(body) {
		t.Errorf("bytes %d -> %d, expected a reduction", len(body), len(out))
	}

	got := string(out)
	if n := strings.Count(got, "handler39InThegatewayPackage"); n != 1 {
		t.Errorf("file contents appear %d times, want 1", n)
	}
}
