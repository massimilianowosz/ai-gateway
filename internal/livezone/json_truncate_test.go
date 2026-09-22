package livezone

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func apiResponse(n int, failAt ...int) string {
	fails := map[int]bool{}
	for _, i := range failAt {
		fails[i] = true
	}
	items := make([]any, 0, n)
	for i := 0; i < n; i++ {
		it := map[string]any{
			"id": fmt.Sprintf("srv-%04d", i), "region": "eu-west-1",
			"status": "healthy", "uptime_seconds": 86400 + i,
		}
		if fails[i] {
			it["status"] = "unhealthy"
			it["error"] = "disk pressure on /var"
		}
		items = append(items, it)
	}
	b, _ := json.Marshal(map[string]any{"servers": items, "total": n})
	return string(b)
}

// The case that pays: hundreds of near-identical records collapse to the shape
// plus the exceptions.
func TestJSONTruncate_LongArrayCollapses(t *testing.T) {
	in := apiResponse(500)
	res := Transform(in, lossy())

	if !res.Applied {
		t.Fatalf("expected truncation, reason=%q", res.Reason)
	}
	if res.Transformer != "json_truncate" {
		t.Fatalf("expected json_truncate, got %q", res.Transformer)
	}
	if !res.Lossy {
		t.Fatal("dropping elements is lossy and must be marked")
	}
	if !json.Valid([]byte(res.Content)) {
		t.Fatalf("output is not valid JSON:\n%s", res.Content)
	}
	saved := 1 - float64(res.BytesAfter)/float64(res.BytesBefore)
	if saved < 0.80 {
		t.Fatalf("expected >80%% saving on 500 uniform records, got %.1f%%", saved*100)
	}
	t.Logf("%d -> %d bytes (%.1f%% saved)", res.BytesBefore, res.BytesAfter, saved*100)
}

// Dropping the one failure in a list of successes is the failure mode that
// matters. Every notable element survives regardless of position.
func TestJSONTruncate_FailuresSurviveWhereverTheySit(t *testing.T) {
	in := apiResponse(500, 7, 250, 480)
	res := Transform(in, lossy())
	if !res.Applied {
		t.Fatalf("expected truncation, reason=%q", res.Reason)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("invalid output: %v", err)
	}
	servers, _ := out["servers"].([]any)

	found := 0
	for _, s := range servers {
		m, _ := s.(map[string]any)
		if m["status"] == "unhealthy" {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("all 3 unhealthy records must survive, found %d", found)
	}
	// srv-0250 sits deep in the omitted region and must still be there.
	if !strings.Contains(res.Content, "srv-0250") {
		t.Error("a failing record in the middle of the array was dropped")
	}
}

func TestJSONTruncate_MarkerRecordsWhatWasDropped(t *testing.T) {
	res := Transform(apiResponse(200), lossy())
	if !strings.Contains(res.Content, omittedKey) {
		t.Fatalf("no omission marker:\n%s", res.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	servers, _ := out["servers"].([]any)
	var omitted float64
	for _, s := range servers {
		if m, ok := s.(map[string]any); ok {
			if v, ok := m[omittedKey]; ok {
				omitted, _ = v.(float64)
			}
		}
	}
	// 200 total, 5 head + 3 tail kept.
	if int(omitted) != 192 {
		t.Fatalf("marker says %v omitted, want 192", omitted)
	}
}

// Short arrays are usually the answer, not a haystack.
func TestJSONTruncate_ShortArraysAreLeftAlone(t *testing.T) {
	in := apiResponse(8)
	res := Transform(in, lossy())
	if res.Transformer == "json_truncate" {
		t.Fatalf("an 8-element array must not be truncated: %s", res.Content)
	}
	for i := 0; i < 8; i++ {
		if !strings.Contains(res.Content, fmt.Sprintf("srv-%04d", i)) {
			t.Errorf("srv-%04d was dropped from a short array", i)
		}
	}
}

// Truncation is lossy, so it stays off unless enabled.
func TestJSONTruncate_RequiresOptIn(t *testing.T) {
	in := apiResponse(500)
	o := DefaultOptions()
	o.MinBytes = 0
	res := Transform(in, o) // AllowLossy false
	if res.Transformer == "json_truncate" {
		t.Fatal("truncation ran without opt-in")
	}
	// All 500 records must still be present.
	if !strings.Contains(res.Content, "srv-0499") {
		t.Error("content was truncated despite lossy being disabled")
	}
}

// Kept elements must not be damaged by the re-encode.
func TestJSONTruncate_KeptValuesAndNumbersAreIntact(t *testing.T) {
	in := `{"rows":[` + strings.Repeat(`{"v":1.0,"big":9007199254740993,"s":"héllo 🌍"},`, 49) +
		`{"v":1.0,"big":9007199254740993,"s":"héllo 🌍"}]}`
	res := Transform(in, lossy())
	if !res.Applied {
		t.Fatalf("expected truncation, reason=%q", res.Reason)
	}
	for _, lit := range []string{"1.0", "9007199254740993", "héllo 🌍"} {
		if !strings.Contains(res.Content, lit) {
			t.Errorf("kept element lost %q:\n%s", lit, res.Content)
		}
	}
}
