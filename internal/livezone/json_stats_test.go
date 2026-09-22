package livezone

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A metric series is better described than sampled: the statistics are
// computed over every element, so nothing about the distribution is guessed.
func TestNumericSummary_SeriesBecomesStatistics(t *testing.T) {
	vals := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		vals = append(vals, fmt.Sprint(i))
	}
	in := `{"latencies":[` + strings.Join(vals, ",") + `]}`

	res := Transform(in, lossy())
	if !res.Applied {
		t.Fatalf("expected a summary, reason=%q", res.Reason)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("invalid output: %v", err)
	}
	m, ok := out["latencies"].(map[string]any)
	if !ok {
		t.Fatalf("expected an object summary, got %T", out["latencies"])
	}
	// Statistics over the true 0..1999 range.
	for key, want := range map[string]float64{
		"count": 2000, "min": 0, "max": 1999, "mean": 999.5, "median": 1000,
	} {
		got, _ := m[key].(float64)
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if _, ok := m["first"]; !ok {
		t.Error("the leading elements should be kept verbatim")
	}
	saved := 1 - float64(res.BytesAfter)/float64(res.BytesBefore)
	t.Logf("%d -> %d bytes (%.1f%% saved)", res.BytesBefore, res.BytesAfter, saved*100)
	if saved < 0.90 {
		t.Fatalf("expected >90%% saving on a 2000-number series, got %.1f%%", saved*100)
	}
}

// A mixed array is not a metric series and must not be summarised away.
func TestNumericSummary_MixedArraysAreLeftAlone(t *testing.T) {
	items := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprint(i))
	}
	items[30] = `"not a number"`
	in := `{"vals":[` + strings.Join(items, ",") + `]}`

	res := Transform(in, lossy())
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if _, isObject := out["vals"].(map[string]any); isObject {
		t.Fatal("a mixed array must not be turned into numeric statistics")
	}
}

// Repeats carry no information the first occurrence did not.
func TestDedupe_IdenticalRecordsCollapse(t *testing.T) {
	var items []string
	for i := 0; i < 300; i++ {
		items = append(items, `{"shard":"a","status":"healthy"}`)
	}
	items = append(items, `{"shard":"b","status":"degraded"}`)
	in := `{"shards":[` + strings.Join(items, ",") + `]}`

	res := Transform(in, lossy())
	if !res.Applied {
		t.Fatalf("expected dedup, reason=%q", res.Reason)
	}
	if !strings.Contains(res.Content, "_ubiquum_repeated") {
		t.Fatalf("no repeat count recorded:\n%s", res.Content)
	}
	// The unique record must survive.
	if !strings.Contains(res.Content, "degraded") {
		t.Error("the one distinct record was dropped")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("invalid output: %v", err)
	}
	shards, _ := out["shards"].([]any)
	for _, s := range shards {
		m, _ := s.(map[string]any)
		if m["status"] == "healthy" {
			if n, _ := m["_ubiquum_repeated"].(float64); n != 300 {
				t.Errorf("repeat count = %v, want 300", n)
			}
		}
	}
	t.Logf("%d -> %d bytes", res.BytesBefore, res.BytesAfter)
}

// Records differing in any field are distinct. Deciding which difference is
// unimportant is the agent's call, not the gateway's.
func TestDedupe_RecordsDifferingByOneFieldAreKept(t *testing.T) {
	var items []string
	for i := 0; i < 50; i++ {
		items = append(items, fmt.Sprintf(`{"id":%d,"status":"ok"}`, i))
	}
	in := `{"rows":[` + strings.Join(items, ",") + `]}`

	res := Transform(in, lossy())
	if strings.Contains(res.Content, "_ubiquum_repeated") {
		t.Fatal("records with distinct ids must not be collapsed as duplicates")
	}
}

// Key order on the wire must not affect equality.
func TestDedupe_KeyOrderDoesNotAffectEquality(t *testing.T) {
	var items []string
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			items = append(items, `{"a":1,"b":2}`)
		} else {
			items = append(items, `{"b":2,"a":1}`)
		}
	}
	in := `{"rows":[` + strings.Join(items, ",") + `]}`
	res := Transform(in, lossy())
	if !strings.Contains(res.Content, "_ubiquum_repeated") {
		t.Fatalf("objects differing only in key order should be equal:\n%s", res.Content)
	}
}

// Both remain lossy and opt-in.
func TestStatsAndDedupe_RequireOptIn(t *testing.T) {
	var items []string
	for i := 0; i < 200; i++ {
		items = append(items, `{"same":"record"}`)
	}
	in := `{"rows":[` + strings.Join(items, ",") + `]}`
	o := DefaultOptions()
	o.MinBytes = 0
	res := Transform(in, o)
	if strings.Contains(res.Content, "_ubiquum_repeated") {
		t.Fatal("dedup ran without opt-in")
	}
}
