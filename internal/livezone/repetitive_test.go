package livezone

import (
	"fmt"
	"strings"
	"testing"
)

// npmInstallish is the shape of build output that carries no clock and no
// severity word, and that the log detector therefore never claimed: the same
// line, over and over.
func npmInstallish() string {
	var sb strings.Builder
	sb.WriteString("added 1423 packages in 41s\n")
	for i := 0; i < 40; i++ {
		sb.WriteString("npm notice run `npm fund` for details\n")
	}
	sb.WriteString("found 0 vulnerabilities\n")
	return sb.String()
}

func TestRepetitive_CollapsesOutputWithNoTimestampOrSeverity(t *testing.T) {
	s := npmInstallish()
	if Detect(s) != KindLogs {
		t.Fatalf("repetitive build output was classified as %v", Detect(s))
	}

	opts := DefaultOptions()
	opts.AllowLossy = true
	opts.MinBytes = 0
	res := Transform(s, opts)
	if !res.Applied {
		t.Fatalf("nothing was collapsed: %s", res.Reason)
	}
	if !strings.Contains(res.Content, "added 1423 packages") ||
		!strings.Contains(res.Content, "found 0 vulnerabilities") {
		t.Error("the lines that carry the answer were lost")
	}
	t.Logf("%d -> %d bytes", res.BytesBefore, res.BytesAfter)
}

// Prose must not be dragged in by a couple of repeated blank-ish lines.
func TestRepetitive_LeavesProseAlone(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, "This paragraph explains decision number %d and the reasoning behind it in ordinary sentences.\n", i)
	}
	if Detect(sb.String()) == KindLogs {
		t.Error("prose was classified as collapsible output")
	}
}

// Source code repeats braces and blank lines, but not whole lines in runs of
// three, so it must not be claimed.
func TestRepetitive_LeavesSourceCodeAlone(t *testing.T) {
	if looksLikeRepetitive(fileContents("gateway")) {
		t.Error("source code was claimed as collapsible output")
	}
}

// Without an actual run to collapse there is nothing to gain, so the weakest
// detector must not claim the text at all.
func TestRepetitive_RequiresRealRepetition(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&sb, "compiling module_%d\n", i)
	}
	if looksLikeRepetitive(sb.String()) {
		t.Error("claimed output where every line is distinct")
	}
}
