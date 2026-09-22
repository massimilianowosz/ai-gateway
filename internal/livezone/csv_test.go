package livezone

import (
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
)

func csvTable(rows int, failAt ...int) string {
	fails := map[int]bool{}
	for _, i := range failAt {
		fails[i] = true
	}
	var sb strings.Builder
	sb.WriteString("id,region,status,latency_ms\n")
	for i := 0; i < rows; i++ {
		status := "ok"
		if fails[i] {
			status = "error: upstream timeout"
		}
		fmt.Fprintf(&sb, "srv-%04d,eu-west-1,%s,%d\n", i, status, 12+i%40)
	}
	return sb.String()
}

func TestCSV_LongTableCollapses(t *testing.T) {
	in := csvTable(500)
	res := Transform(in, lossy())
	if !res.Applied {
		t.Fatalf("expected truncation, reason=%q kind=%s", res.Reason, res.Kind)
	}
	if res.Transformer != "csv_compact" {
		t.Fatalf("expected csv_compact, got %q", res.Transformer)
	}
	saved := 1 - float64(res.BytesAfter)/float64(res.BytesBefore)
	if saved < 0.80 {
		t.Fatalf("expected >80%% saving, got %.1f%%", saved*100)
	}
	t.Logf("%d -> %d bytes (%.1f%% saved)", res.BytesBefore, res.BytesAfter, saved*100)
}

// The header is what tells the agent what the columns mean; losing it makes
// every surviving row unreadable.
func TestCSV_HeaderAlwaysSurvives(t *testing.T) {
	res := Transform(csvTable(200), lossy())
	if !strings.HasPrefix(res.Content, "id,region,status,latency_ms") {
		t.Fatalf("header row was lost:\n%s", res.Content[:120])
	}
}

func TestCSV_FailingRowsSurviveWhereverTheySit(t *testing.T) {
	res := Transform(csvTable(500, 3, 250, 495), lossy())
	if !res.Applied {
		t.Fatalf("expected truncation, reason=%q", res.Reason)
	}
	// srv-0250 is deep inside the omitted region.
	for _, want := range []string{"srv-0003", "srv-0250", "srv-0495"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("failing row %s was dropped", want)
		}
	}
	if got := strings.Count(res.Content, "upstream timeout"); got != 3 {
		t.Errorf("all 3 error rows must survive, found %d", got)
	}
}

// The output must still parse as CSV, or the agent cannot read it.
func TestCSV_OutputIsStillParseable(t *testing.T) {
	res := Transform(csvTable(300), lossy())
	body := res.Content
	if i := strings.LastIndex(body, "\n#"); i >= 0 {
		body = body[:i+1] // the trailing note is a comment, not a row
	}
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("output does not parse: %v", err)
	}
	for i, row := range rows {
		if len(row) != 4 {
			t.Fatalf("row %d has %d fields, want 4: %v", i, len(row), row)
		}
	}
}

// Quoted fields containing the delimiter must survive intact.
func TestCSV_QuotingIsPreserved(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("id,note,status\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, "%d,\"a note, with a comma\",ok\n", i)
	}
	res := Transform(sb.String(), lossy())
	if !res.Applied {
		t.Fatalf("expected truncation, reason=%q", res.Reason)
	}
	if !strings.Contains(res.Content, `"a note, with a comma"`) {
		t.Fatalf("quoting was lost:\n%s", res.Content[:200])
	}
}

func TestCSV_ShortTablesAreLeftAlone(t *testing.T) {
	in := csvTable(12)
	res := Transform(in, lossy())
	if res.Transformer == "csv_compact" {
		t.Fatal("a 12-row table must not be truncated")
	}
	for i := 0; i < 12; i++ {
		if !strings.Contains(res.Content, fmt.Sprintf("srv-%04d", i)) {
			t.Errorf("srv-%04d was dropped", i)
		}
	}
}

// Prose with commas is not a table.
func TestCSV_ProseIsNotDetected(t *testing.T) {
	prose := strings.Repeat("This sentence has commas, several of them, but it is not a table.\n", 40)
	if _, ok := looksLikeCSV(prose); ok {
		t.Error("prose with commas was classified as CSV")
	}
	if Detect(prose) == KindCSV {
		t.Error("Detect classified prose as CSV")
	}
}

func TestCSV_RequiresOptIn(t *testing.T) {
	o := DefaultOptions()
	o.MinBytes = 0
	res := Transform(csvTable(400), o)
	if res.Transformer == "csv_compact" {
		t.Fatal("csv truncation ran without opt-in")
	}
	if !strings.Contains(res.Content, "srv-0399") {
		t.Error("rows were dropped with lossy disabled")
	}
}

func TestCSV_TabSeparatedIsDetected(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("id\tregion\tstatus\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "%d\teu-west-1\tok\n", i)
	}
	delim, ok := looksLikeCSV(sb.String())
	if !ok || delim != '\t' {
		t.Fatalf("TSV not detected (ok=%v delim=%q)", ok, delim)
	}
}

// Detect classifies the trimmed text, so the transform must see the same
// string. Running looksLikeCSV on the untrimmed original made a table with a
// leading blank line detect as CSV and then decline every delimiter — and
// having committed to KindCSV, it could not fall through to the log
// transformer either.
func TestCSV_LeadingWhitespaceStillCompresses(t *testing.T) {
	in := "\n\n" + csvTable(500)
	if k := Detect(in); k != KindCSV {
		t.Fatalf("Detect = %s, want csv", k)
	}
	res := Transform(in, lossy())
	if !res.Applied {
		t.Fatalf("expected truncation, reason=%q transformer=%q", res.Reason, res.Transformer)
	}
	if res.Transformer != "csv_compact" {
		t.Fatalf("expected csv_compact, got %q", res.Transformer)
	}
	if !strings.HasPrefix(res.Content, "id,region,status,latency_ms") {
		t.Fatalf("header row was lost:\n%s", res.Content[:120])
	}
}
