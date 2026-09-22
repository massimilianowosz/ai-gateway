package livezone

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildDiff produces a real unified diff with `context` lines of context.
func buildDiff(t *testing.T, before, after string, context int) string {
	t.Helper()
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("diff", "-U", itoa(context), "--label", "a/f.txt", "--label", "b/f.txt", a, b).Output()
	if len(out) == 0 {
		t.Skip("diff produced no output")
	}
	return string(out)
}

func numberedFile(n int, replace map[int]string) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		if s, ok := replace[i]; ok {
			sb.WriteString(s + "\n")
			continue
		}
		sb.WriteString("line " + itoa(i) + " unchanged filler content\n")
	}
	return sb.String()
}

func lossyDiffOpts() Options {
	o := DefaultOptions()
	o.AllowLossy = true
	o.MinBytes = 0
	o.MinGainRatio = 0
	return o
}

// The property that matters: a compressed diff must still apply, and produce
// exactly the same file. Dropping context without fixing the hunk headers
// yields a diff that reads plausibly and applies wrongly.
func TestDiff_CompressedDiffStillAppliesCleanly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	before := numberedFile(200, nil)
	after := numberedFile(200, map[int]string{
		20:  "line 20 MODIFIED",
		100: "line 100 MODIFIED",
		180: "line 180 MODIFIED",
	})

	raw := buildDiff(t, before, after, 25) // deliberately wide context
	res := Transform(raw, lossyDiffOpts())
	if !res.Applied {
		t.Fatalf("expected compaction, reason=%q kind=%s", res.Reason, res.Kind)
	}
	t.Logf("%d -> %d bytes (%.0f%% saved)", res.BytesBefore, res.BytesAfter,
		100*(1-float64(res.BytesAfter)/float64(res.BytesBefore)))

	// Apply the compressed diff to the original and compare.
	dir := t.TempDir()
	work := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(work, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(dir, "p.diff")
	if err := os.WriteFile(patch, []byte(res.Content), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "apply", "--unsafe-paths", "--directory", ".", patch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compressed diff does not apply: %v\n%s\n--- diff ---\n%s", err, out, res.Content)
	}

	got, err := os.ReadFile(work)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != after {
		t.Fatal("applying the compressed diff produced a different file")
	}
}

// Every changed line must survive; only context is negotiable.
func TestDiff_AllChangedLinesSurvive(t *testing.T) {
	before := numberedFile(150, nil)
	after := numberedFile(150, map[int]string{
		10: "line 10 CHANGED-A", 75: "line 75 CHANGED-B", 140: "line 140 CHANGED-C",
	})
	raw := buildDiff(t, before, after, 20)

	res := Transform(raw, lossyDiffOpts())
	if !res.Applied {
		t.Fatalf("expected compaction, reason=%q", res.Reason)
	}
	for _, want := range []string{"CHANGED-A", "CHANGED-B", "CHANGED-C"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("changed line %s was dropped", want)
		}
	}
	// And the removals too.
	for _, want := range []string{"-line 10 ", "-line 75 ", "-line 140 "} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("removed line %q was dropped", want)
		}
	}
}

// A tight diff has nothing to give back.
func TestDiff_AlreadyTightIsLeftAlone(t *testing.T) {
	before := numberedFile(40, nil)
	after := numberedFile(40, map[int]string{20: "line 20 MODIFIED"})
	raw := buildDiff(t, before, after, 3)

	res := Transform(raw, lossyDiffOpts())
	if res.Applied && res.BytesAfter >= res.BytesBefore {
		t.Fatal("a tight diff must not be inflated")
	}
	if res.Applied {
		t.Logf("still saved %d bytes", res.BytesBefore-res.BytesAfter)
	}
}

func TestDiff_DetectedOnlyWithHunkHeaders(t *testing.T) {
	if Detect("--- a/f.txt\n+++ b/f.txt\nno hunks here at all\n") == KindDiff {
		t.Error("file headers alone must not be classified as a diff")
	}
	if got := Detect("@@ -1,3 +1,4 @@\n a\n+b\n c\n"); got != KindDiff {
		t.Errorf("Detect = %v, want diff", got)
	}
}

// Diff must win over logs: both are line-oriented, and a diff run through the
// log transformer would lose changed lines.
func TestDiff_TakesPrecedenceOverLogDetection(t *testing.T) {
	before := numberedFile(120, nil)
	after := numberedFile(120, map[int]string{60: "line 60 ERROR failed"})
	raw := buildDiff(t, before, after, 20)
	if got := Detect(raw); got != KindDiff {
		t.Fatalf("Detect = %v, want diff", got)
	}
}

// commentedFile builds a file where some lines are SQL-style comments, so a
// deletion produces a diff line beginning "--- ".
func commentedFile(n int, drop map[int]bool) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		if drop[i] {
			continue
		}
		switch i % 25 {
		case 0:
			sb.WriteString("-- legacy flag " + itoa(i) + ", remove after migration\n")
		default:
			sb.WriteString("select " + itoa(i) + " from filler_table_with_a_long_name;\n")
		}
	}
	return sb.String()
}

// A deleted line whose source begins with "-- " is written as "--- ...", which
// a prefix scan cannot tell from a file header. Breaking the hunk body there
// truncates it while the recomputed header still claims the full line count —
// a patch that reads plausibly and applies wrongly.
func TestDiff_DeletedCommentLineDoesNotCorruptHunk(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	before := commentedFile(200, nil)
	after := commentedFile(200, map[int]bool{50: true, 100: true, 150: true})

	raw := buildDiff(t, before, after, 25)
	if !strings.Contains(raw, "\n--- legacy flag") {
		t.Fatalf("fixture did not produce a deleted comment line:\n%s", raw)
	}

	res := Transform(raw, lossyDiffOpts())
	if !res.Applied {
		t.Fatalf("expected compaction, reason=%q kind=%s", res.Reason, res.Kind)
	}

	dir := t.TempDir()
	work := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(work, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(dir, "p.diff")
	if err := os.WriteFile(patch, []byte(res.Content), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "apply", "--unsafe-paths", "--directory", ".", patch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compressed diff does not apply: %v\n%s\n--- diff ---\n%s", err, out, res.Content)
	}
	got, err := os.ReadFile(work)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != after {
		t.Fatal("applying the compressed diff produced a different file")
	}
}

// looksLikeDiff accepts any content holding one hunk header, so a hunk with no
// change lines may not be a hunk at all — a chat log or PR description quoting
// a fragment lands here too. Dropping it discarded everything up to the next
// header.
func TestDiff_ContextOnlyHunkIsNotDiscarded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("--- a/f.txt\n+++ b/f.txt\n@@ -1,15 +1,15 @@\n")
	for i := 1; i <= 15; i++ {
		sb.WriteString(" quoted context line " + itoa(i) + " that carries real information\n")
	}
	sb.WriteString("@@ -100,14 +100,15 @@\n")
	for i := 1; i <= 7; i++ {
		sb.WriteString(" second hunk filler line " + itoa(i) + " padding it past the minimum\n")
	}
	sb.WriteString("+an added line\n")
	for i := 8; i <= 14; i++ {
		sb.WriteString(" second hunk filler line " + itoa(i) + " padding it past the minimum\n")
	}

	res := Transform(sb.String(), lossyDiffOpts())
	if !res.Applied {
		t.Skipf("no compaction to check, reason=%q", res.Reason)
	}
	for i := 1; i <= 15; i++ {
		if !strings.Contains(res.Content, "quoted context line "+itoa(i)+" ") {
			t.Fatalf("context-only hunk was discarded; line %d is gone:\n%s", i, res.Content)
		}
	}
}

// Both lossy transformers are gated before they run, so with AllowLossy off the
// request path does not pay for a parse-and-rewrite that is thrown away.
func TestDiff_LossyGateReportedBeforeTransforming(t *testing.T) {
	raw := buildDiff(t, numberedFile(200, nil),
		numberedFile(200, map[int]string{20: "line 20 MODIFIED"}), 25)

	opts := lossyDiffOpts()
	opts.AllowLossy = false
	res := Transform(raw, opts)
	if res.Applied {
		t.Fatal("a lossy transform ran with AllowLossy off")
	}
	if res.Reason != "lossy_not_allowed" {
		t.Errorf("reason = %q, want %q", res.Reason, "lossy_not_allowed")
	}
}
