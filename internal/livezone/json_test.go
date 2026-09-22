package livezone

import (
	"encoding/json"
	"strings"
	"testing"
)

func lossless() Options {
	o := DefaultOptions()
	o.MinBytes = 0
	o.MinGainRatio = 0
	return o
}

// Compaction must preserve the document exactly: same values, same keys, same
// order. It only removes whitespace.
func TestJSON_CompactionIsLossless(t *testing.T) {
	original := `{
  "users": [
    { "id": 1, "name": "Ada",    "active": true  },
    { "id": 2, "name": "Grace",  "active": false }
  ],
  "total": 2
}`
	res := Transform(original, lossless())

	if !res.Applied {
		t.Fatalf("expected compaction, got reason=%q", res.Reason)
	}
	if res.Lossy {
		t.Fatal("whitespace removal must not be marked lossy")
	}
	if res.BytesAfter >= res.BytesBefore {
		t.Fatalf("no saving: %d -> %d", res.BytesBefore, res.BytesAfter)
	}

	var before, after any
	if err := json.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(res.Content), &after); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, res.Content)
	}
	if string(mustMarshal(t, before)) != string(mustMarshal(t, after)) {
		t.Fatal("document changed during compaction")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Round-tripping through float64 would turn 1.0 into 1 and mangle integers
// above 2^53. Compaction must not touch the numeric literals at all.
func TestJSON_NumericPrecisionSurvives(t *testing.T) {
	in := `{
  "exact":  1.0,
  "big":    9007199254740993,
  "tiny":   0.1000000000000000055511151231257827,
  "money":  1234.5000
}`
	res := Transform(in, lossless())
	if !res.Applied {
		t.Fatalf("expected compaction, reason=%q", res.Reason)
	}
	for _, literal := range []string{"1.0", "9007199254740993", "1234.5000",
		"0.1000000000000000055511151231257827"} {
		if !strings.Contains(res.Content, literal) {
			t.Errorf("numeric literal %s was rewritten: %s", literal, res.Content)
		}
	}
}

// Already-compact JSON has nothing to gain; the transformer must decline
// rather than churn the bytes the model sees.
func TestJSON_AlreadyCompactIsLeftAlone(t *testing.T) {
	in := `{"a":1,"b":[2,3],"c":"x"}`
	res := Transform(in, lossless())
	if res.Applied {
		t.Fatalf("expected no change, got %q", res.Content)
	}
	if res.Content != in {
		t.Fatal("content must be returned unchanged")
	}
}

// Text that merely starts with a brace is not JSON and must not be routed to
// the JSON transformer.
func TestJSON_InvalidJSONIsNotDetected(t *testing.T) {
	for _, in := range []string{
		"{ this is not json, just prose that opens with a brace }",
		`{"unterminated": `,
		"[1, 2, 3",
	} {
		if got := Detect(in); got == KindJSON {
			t.Errorf("Detect(%q) = json, want not json", in)
		}
	}
}

func TestJSON_UnicodeAndEscapesSurvive(t *testing.T) {
	in := `{
  "emoji":  "héllo 🌍 世界",
  "escape": "line\nbreak\ttab\"quote\\slash",
  "url":    "https://example.com/a?b=c&d=e"
}`
	res := Transform(in, lossless())
	if !res.Applied {
		t.Fatalf("expected compaction, reason=%q", res.Reason)
	}
	var before, after map[string]string
	if err := json.Unmarshal([]byte(in), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(res.Content), &after); err != nil {
		t.Fatalf("invalid output: %v", err)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %q became %q", k, v, after[k])
		}
	}
}

// A realistic pretty-printed API response: the saving should be substantial.
func TestJSON_RealisticPayloadSavesMeaningfully(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("{\n  \"items\": [\n")
	for i := 0; i < 200; i++ {
		if i > 0 {
			sb.WriteString(",\n")
		}
		sb.WriteString("    {\n      \"id\": ")
		sb.WriteString(strings.Repeat("9", 6))
		sb.WriteString(",\n      \"status\": \"ok\",\n      \"region\": \"eu-west-1\"\n    }")
	}
	sb.WriteString("\n  ]\n}")

	res := Transform(sb.String(), DefaultOptions())
	if !res.Applied {
		t.Fatalf("expected compaction, reason=%q", res.Reason)
	}
	saved := 1 - float64(res.BytesAfter)/float64(res.BytesBefore)
	if saved < 0.30 {
		t.Fatalf("expected >30%% saving on pretty-printed JSON, got %.1f%%", saved*100)
	}
	t.Logf("%d -> %d bytes (%.1f%% saved)", res.BytesBefore, res.BytesAfter, saved*100)
}
