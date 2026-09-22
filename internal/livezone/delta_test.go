package livezone

import (
	"fmt"
	"strings"
	"testing"
)

func deltaPolicy() Policy {
	pol := dedupePolicy()
	pol.DeltaRepeats = true
	return pol
}

// editedFileContents returns fileContents with one line changed, which is what
// a coding agent produces between two reads of the same file.
func editedFileContents(name string, atLine int) string {
	lines := strings.Split(fileContents(name), "\n")
	lines[atLine] = fmt.Sprintf("func handler%dInThe%sPackage(ctx context.Context) error { return errors.New(\"boom\") }", atLine, name)
	return strings.Join(lines, "\n")
}

// The case that dominates a long session: the same file read, edited, read
// again. Exact dedupe cannot see it because the bytes differ.
func TestDelta_NearIdenticalRereadCollapses(t *testing.T) {
	first := fileContents("gateway")
	second := editedFileContents("gateway", 12)

	body := twoReadsBodyResponses(t, first, second)
	out, stats, err := RewriteResponses(body, deltaPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_delta"] != 1 {
		t.Fatalf("delta applied %d times, want 1 (exact=%d)",
			stats.Transformers["dedupe_delta"], stats.Transformers["dedupe_repeat"])
	}
	if len(out) >= len(body) {
		t.Fatalf("bytes %d -> %d, expected a reduction", len(body), len(out))
	}

	got := string(out)
	// The first copy must survive in full: every offset the agent may quote
	// back resolves against it.
	if n := strings.Count(got, "handler39InThegatewayPackage"); n != 1 {
		t.Errorf("the surviving copy appears %d times, want exactly 1", n)
	}
	// The change itself must be stated, not dropped.
	if !strings.Contains(got, `errors.New(\"boom\")`) {
		t.Error("the edited line never reached the model")
	}
	if !strings.Contains(got, "same content as the earlier") {
		t.Errorf("no reference to the earlier copy:\n%s", firstMarker(got))
	}
	t.Logf("%d -> %d bytes (%.0f%% saved)", len(body), len(out),
		100*(1-float64(len(out))/float64(len(body))))
}

// Two unrelated payloads must never be described as edits of one another.
func TestDelta_DifferentFilesAreNotCollapsed(t *testing.T) {
	body := twoReadsBodyResponses(t, fileContents("gateway"), fileContents("router"))
	out, stats, err := RewriteResponses(body, deltaPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_delta"] != 0 {
		t.Error("two different files were collapsed into one another")
	}
	if !strings.Contains(string(out), "InTherouterPackage") {
		t.Error("the second file's contents were lost")
	}
}

// Off unless asked for: it rewrites content, not just recognises it.
func TestDelta_IsOptIn(t *testing.T) {
	body := twoReadsBodyResponses(t, fileContents("gateway"), editedFileContents("gateway", 12))

	out, stats, err := RewriteResponses(body, dedupePolicy()) // exact dedupe only
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_delta"] != 0 {
		t.Error("delta ran without being asked to")
	}
	if len(out) != len(body) {
		t.Errorf("body changed with delta off: %d -> %d", len(body), len(out))
	}
}

// The same body must produce the same bytes every turn, or the provider's
// prefix cache misses on every request and the saving is worse than nothing.
func TestDelta_IsDeterministic(t *testing.T) {
	body := twoReadsBodyResponses(t, fileContents("gateway"), editedFileContents("gateway", 12))

	first, _, err := RewriteResponses(append([]byte(nil), body...), deltaPolicy())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := RewriteResponses(append([]byte(nil), body...), deltaPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("two runs over the same body produced different bytes")
	}
}

// A rewrite that barely helps is not worth changing the bytes the model sees:
// when the edit touches most of the file, the description is not smaller.
func TestDelta_DeclinesWhenTheEditIsTheWholeFile(t *testing.T) {
	first := fileContents("gateway")
	var sb strings.Builder
	sb.WriteString("package gateway\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "func renamed%dEntirely(ctx context.Context) error { return nil }\n", i)
	}
	body := twoReadsBodyResponses(t, first, sb.String())

	_, stats, err := RewriteResponses(body, deltaPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["dedupe_delta"] != 0 {
		t.Error("described a rewrite of the whole file as an edit of it")
	}
}

// All three wire formats must behave identically.
func TestDelta_AppliesToAnthropicAndOpenAIToo(t *testing.T) {
	first := fileContents("gateway")
	second := editedFileContents("gateway", 12)

	anth := mustJSON(t, map[string]any{"model": "claude-x", "messages": []any{
		map[string]any{"role": "user", "content": "read it twice"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read"}}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": first}}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read"}}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": second}}},
	}})
	if _, stats, err := RewriteAnthropic(anth, deltaPolicy()); err != nil {
		t.Fatal(err)
	} else if stats.Transformers["dedupe_delta"] != 1 {
		t.Errorf("anthropic: delta applied %d times, want 1", stats.Transformers["dedupe_delta"])
	}

	oai := mustJSON(t, map[string]any{"model": "gpt-4o", "messages": []any{
		map[string]any{"role": "user", "content": "read it twice"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"id": "t1", "type": "function", "function": map[string]string{"name": "Read", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "t1", "content": first},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"id": "t2", "type": "function", "function": map[string]string{"name": "Read", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "t2", "content": second},
	}})
	if _, stats, err := RewriteOpenAI(oai, deltaPolicy()); err != nil {
		t.Fatal(err)
	} else if stats.Transformers["dedupe_delta"] != 1 {
		t.Errorf("openai: delta applied %d times, want 1", stats.Transformers["dedupe_delta"])
	}
}

func twoReadsBodyResponses(t *testing.T, first, second string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "read it twice"}}},
			map[string]any{"type": "custom_tool_call", "call_id": "t1", "name": "Read", "input": "a.go"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "t1", "output": first},
			map[string]any{"type": "custom_tool_call", "call_id": "t2", "name": "Read", "input": "a.go"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "t2", "output": second},
		},
	})
}

// --- describeEdit, directly ---

func TestDescribeEdit_ReportsTheReplacedRange(t *testing.T) {
	prev := []string{"a", "b", "c", "d"}
	cur := []string{"a", "B!", "c", "d"}

	got, ok := describeEdit(prev, cur, "Read result")
	if !ok {
		t.Fatal("an edit of one line was not described")
	}
	if !strings.Contains(got, "lines 2-2") {
		t.Errorf("wrong range reported: %s", got)
	}
	if !strings.Contains(got, "B!") {
		t.Errorf("the new content was not carried: %s", got)
	}
}

func TestDescribeEdit_ReportsPureDeletion(t *testing.T) {
	prev := []string{"a", "b", "c", "d"}
	cur := []string{"a", "d"}

	got, ok := describeEdit(prev, cur, "Read result")
	if !ok {
		t.Fatal("a deletion was not described")
	}
	if !strings.Contains(got, "no longer present") {
		t.Errorf("deletion not stated: %s", got)
	}
}

func TestDescribeEdit_RefusesWithoutSharedContext(t *testing.T) {
	if _, ok := describeEdit([]string{"a", "b"}, []string{"x", "y"}, "l"); ok {
		t.Error("described two unrelated payloads as an edit")
	}
}
