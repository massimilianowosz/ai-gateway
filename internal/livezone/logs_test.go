package livezone

import (
	"fmt"
	"strings"
	"testing"
)

func lossy() Options {
	o := DefaultOptions()
	o.AllowLossy = true
	o.MinBytes = 0
	o.MinGainRatio = 0
	return o
}

// The rule that makes log compaction safe: a line reporting a failure is never
// removed, no matter how often it repeats.
func TestLogs_FailuresAreNeverCollapsed(t *testing.T) {
	var sb strings.Builder
	// Identical noise, so the transform definitely has something to collapse.
	for i := 0; i < 40; i++ {
		sb.WriteString("2026-01-02 10:00:00 INFO  fetching next page\n")
	}
	// Identical failures. These repeat too, and must survive anyway: a check
	// that failed five times failed five times, and collapsing that would
	// misreport the severity to the agent.
	for i := 0; i < 5; i++ {
		sb.WriteString("2026-01-02 10:00:01 ERROR connection refused to db-primary\n")
	}
	sb.WriteString("2026-01-02 10:00:02 FATAL migration aborted\n")

	res := Transform(sb.String(), lossy())
	if !res.Applied {
		t.Fatalf("expected the repeated INFO lines to collapse, reason=%q", res.Reason)
	}

	if got := strings.Count(res.Content, "connection refused to db-primary"); got != 5 {
		t.Errorf("all 5 identical ERROR lines must survive, found %d:\n%s", got, res.Content)
	}
	if !strings.Contains(res.Content, "FATAL migration aborted") {
		t.Error("FATAL line was dropped")
	}
	// The noise, meanwhile, is gone.
	if got := strings.Count(res.Content, "fetching next page"); got != 1 {
		t.Errorf("repeated INFO lines should collapse to one, found %d", got)
	}
	t.Logf("%d -> %d bytes, every failure preserved", res.BytesBefore, res.BytesAfter)
}

func TestLogs_RepeatedNoiseIsCollapsed(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("2026-01-02 10:00:00 INFO starting build\n")
	for i := 0; i < 200; i++ {
		sb.WriteString("2026-01-02 10:00:00 INFO downloading dependency\n")
	}
	sb.WriteString("2026-01-02 10:00:30 INFO build finished\n")

	res := Transform(sb.String(), lossy())
	if !res.Applied {
		t.Fatalf("expected collapse, reason=%q", res.Reason)
	}
	if !res.Lossy {
		t.Error("collapsing repeats discards information and must be marked lossy")
	}
	if !strings.Contains(res.Content, "repeated 199 more times") {
		t.Errorf("expected a repeat marker, got:\n%s", res.Content)
	}
	// The bracketing lines are the ones that carry meaning.
	if !strings.Contains(res.Content, "starting build") || !strings.Contains(res.Content, "build finished") {
		t.Error("surrounding context was lost")
	}
	if res.BytesAfter >= res.BytesBefore {
		t.Fatalf("no saving: %d -> %d", res.BytesBefore, res.BytesAfter)
	}
	t.Logf("%d -> %d bytes", res.BytesBefore, res.BytesAfter)
}

// Stack frames are meaningless in isolation, so they are kept whole.
func TestLogs_StackTracesSurvive(t *testing.T) {
	trace := `2026-01-02 10:00:00 ERROR unhandled exception
Traceback (most recent call last):
  File "/app/main.py", line 42, in handler
  File "/app/db.py", line 17, in connect
ConnectionRefusedError: [Errno 111] Connection refused
`
	var sb strings.Builder
	for i := 0; i < 30; i++ {
		sb.WriteString("2026-01-02 09:59:00 INFO warming cache\n")
	}
	sb.WriteString(trace)

	res := Transform(sb.String(), lossy())
	for _, want := range []string{
		"Traceback (most recent call last):",
		`File "/app/main.py", line 42, in handler`,
		`File "/app/db.py", line 17, in connect`,
		"ConnectionRefusedError",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("stack trace line lost: %q", want)
		}
	}
}

// Terminal colour codes are pure noise once the output is text in a prompt.
func TestLogs_ANSIEscapesAreStripped(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&sb, "\x1b[32m2026-01-02 10:00:0%d PASS\x1b[0m TestFoo%d\n", i%10, i)
	}
	res := Transform(sb.String(), lossy())
	if !res.Applied {
		t.Fatalf("expected ANSI stripping, reason=%q", res.Reason)
	}
	if strings.Contains(res.Content, "\x1b[") {
		t.Error("escape sequences remain")
	}
	if !strings.Contains(res.Content, "TestFoo3") {
		t.Error("content was damaged while stripping escapes")
	}
}

// Ordering is a correctness property: the agent reads a sequence of events.
func TestLogs_OrderIsPreserved(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("2026-01-02 10:00:00 INFO step one\n")
	for i := 0; i < 20; i++ {
		sb.WriteString("2026-01-02 10:00:01 INFO polling\n")
	}
	sb.WriteString("2026-01-02 10:00:02 INFO step two\n")
	for i := 0; i < 20; i++ {
		sb.WriteString("2026-01-02 10:00:03 INFO polling again\n")
	}
	sb.WriteString("2026-01-02 10:00:04 INFO step three\n")

	res := Transform(sb.String(), lossy())
	one := strings.Index(res.Content, "step one")
	two := strings.Index(res.Content, "step two")
	three := strings.Index(res.Content, "step three")
	if one < 0 || two < 0 || three < 0 {
		t.Fatalf("markers lost:\n%s", res.Content)
	}
	if one >= two || two >= three {
		t.Fatal("events were reordered")
	}
}

// Lossy transforms must stay off unless explicitly enabled.
func TestLogs_LossyRequiresOptIn(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("2026-01-02 10:00:00 INFO repeated line\n")
	}
	o := DefaultOptions()
	o.MinBytes = 0
	res := Transform(sb.String(), o) // AllowLossy is false
	if res.Applied {
		t.Fatalf("lossy collapse ran without opt-in: %q", res.Reason)
	}
	if res.Reason != "lossy_not_allowed" {
		t.Fatalf("unexpected reason %q", res.Reason)
	}
	if res.Content != sb.String() {
		t.Fatal("content must be untouched when the transform is refused")
	}
}

// Prose and source code must not be routed to the log transformer.
func TestLogs_ProseAndCodeAreNotDetectedAsLogs(t *testing.T) {
	prose := strings.Repeat("This is an ordinary paragraph of explanatory text that a model wrote. ", 30)
	code := `package main

import "fmt"

func main() {
	for i := 0; i < 10; i++ {
		fmt.Println("hello", i)
	}
}
`
	for name, in := range map[string]string{"prose": prose, "code": code} {
		if got := Detect(in); got == KindLogs {
			t.Errorf("Detect(%s) = logs, want not logs", name)
		}
	}
}
