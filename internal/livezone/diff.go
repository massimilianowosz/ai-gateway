package livezone

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

// looksLikeDiff recognises unified-diff output. It requires a hunk header,
// which is the one construct nothing else produces.
func looksLikeDiff(t string) bool {
	if !strings.Contains(t, "@@ -") {
		return false
	}
	for _, line := range strings.Split(t, "\n") {
		if hunkHeaderRe.MatchString(line) {
			return true
		}
	}
	return false
}

// compactDiff narrows the context around each change.
//
// A diff generated with -U20, or a whole-file diff, spends most of its bytes
// on unchanged lines the agent already has. This keeps every added and removed
// line, and a small window of context around each change, dropping the rest.
//
// Hunk headers are recomputed rather than left stale. Dropping context without
// fixing the line counts produces a diff that reads plausibly and applies
// wrongly, which is worse than not compressing at all: where a window is cut,
// the hunk is split into two hunks with correct headers, exactly as a smaller
// -U would have produced.
func compactDiff(s string, cfg DiffConfig) (string, bool) {
	// A patch must end with a newline or git rejects it as corrupt. Split
	// leaves a trailing "" for that newline; drop it here and restore the
	// newline at the end rather than carrying a phantom line through the
	// hunk arithmetic.
	trailingNewline := strings.HasSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	if trailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var out []string
	changed := false

	i := 0
	for i < len(lines) {
		m := hunkHeaderRe.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			i++
			continue
		}

		oldStart := atoiOr(m[1], 1)
		newStart := atoiOr(m[3], 1)
		oldCount := atoiOr(m[2], 1)
		newCount := atoiOr(m[4], 1)
		heading := m[5]

		// Collect the hunk body by consuming the line counts the header
		// declares, not by watching for a terminator.
		//
		// Terminator matching cannot work here: a deleted line whose source
		// begins with "-- " (a SQL or Lua comment, an "--" rule) is written as
		// "--- ...", indistinguishable from a file header by prefix alone.
		// Breaking there truncates the body while the recomputed header still
		// claims the full line count — a patch that reads plausibly and applies
		// wrongly. Every body line starts with ' ', '+', '-' or '\', so
		// anything else ends the hunk even if the declared counts disagree.
		body := []string{}
		j := i + 1
		gotOld, gotNew := 0, 0
		for j < len(lines) && (gotOld < oldCount || gotNew < newCount) {
			l := lines[j]
			if !isBodyLine(l) {
				break
			}
			gotOld += oldDelta(l)
			gotNew += newDelta(l)
			body = append(body, l)
			j++
		}
		// A "\ No newline at end of file" marker trails the line it annotates
		// and advances neither counter, so a trailing one sits just past the
		// satisfied counts.
		for j < len(lines) && strings.HasPrefix(lines[j], "\\ No newline") {
			body = append(body, lines[j])
			j++
		}

		rebuilt, did := splitHunk(body, oldStart, newStart, heading, cfg)
		out = append(out, rebuilt...)
		changed = changed || did
		i = j
	}

	if !changed {
		return "", false
	}
	result := strings.Join(out, "\n")
	if trailingNewline {
		result += "\n"
	}
	return result, true
}

// DiffConfig bounds diff compaction.
type DiffConfig struct {
	// Context is how many unchanged lines to keep on each side of a change.
	Context int
	// MinHunkLines is the hunk size below which nothing is trimmed; a small
	// hunk is already mostly signal.
	MinHunkLines int
}

// DefaultDiffConfig keeps three lines of context, the git default.
func DefaultDiffConfig() DiffConfig { return DiffConfig{Context: 3, MinHunkLines: 12} }

// splitHunk rewrites one hunk body as one or more hunks with tight context.
func splitHunk(body []string, oldStart, newStart int, heading string, cfg DiffConfig) ([]string, bool) {
	if len(body) < cfg.MinHunkLines {
		return append([]string{hunkHeader(oldStart, countOld(body), newStart, countNew(body), heading)}, body...), false
	}

	// Mark which lines to keep: every change, plus a window of context.
	keep := make([]bool, len(body))
	for i, l := range body {
		if isChangeLine(l) {
			lo := i - cfg.Context
			if lo < 0 {
				lo = 0
			}
			hi := i + cfg.Context
			if hi >= len(body) {
				hi = len(body) - 1
			}
			for k := lo; k <= hi; k++ {
				keep[k] = true
			}
		}
	}
	// A hunk with no change lines is not a hunk this function can improve, and
	// it is not necessarily a hunk at all: looksLikeDiff accepts any content
	// holding one hunk header, so a chat log or PR description quoting a diff
	// fragment lands here too. Emit it unchanged rather than discarding
	// everything up to the next header.
	anyKept := false
	for _, k := range keep {
		if k {
			anyKept = true
			break
		}
	}
	if !anyKept {
		return append([]string{hunkHeader(oldStart, countOld(body), newStart, countNew(body), heading)}, body...), false
	}
	allKept := true
	for _, k := range keep {
		if !k {
			allKept = false
			break
		}
	}
	if allKept {
		return append([]string{hunkHeader(oldStart, countOld(body), newStart, countNew(body), heading)}, body...), false
	}

	// Emit each contiguous kept run as its own hunk, tracking line numbers so
	// the recomputed headers are correct.
	var out []string
	oldLine, newLine := oldStart, newStart
	i := 0
	for i < len(body) {
		if !keep[i] {
			oldLine += oldDelta(body[i])
			newLine += newDelta(body[i])
			i++
			continue
		}
		runOld, runNew := oldLine, newLine
		var run []string
		for i < len(body) && keep[i] {
			run = append(run, body[i])
			oldLine += oldDelta(body[i])
			newLine += newDelta(body[i])
			i++
		}
		out = append(out, hunkHeader(runOld, countOld(run), runNew, countNew(run), heading))
		out = append(out, run...)
	}
	return out, true
}

func hunkHeader(oldStart, oldCount, newStart, newCount int, heading string) string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@%s", oldStart, oldCount, newStart, newCount, heading)
}

func isChangeLine(l string) bool {
	return len(l) > 0 && (l[0] == '+' || l[0] == '-')
}

// isBodyLine reports whether l can appear inside a hunk body. Context, added
// and removed lines carry their marker in column one; "\ No newline at end of
// file" is the only other legal member. An empty line is accepted because some
// producers emit one for a blank context line instead of a lone space.
func isBodyLine(l string) bool {
	if l == "" {
		return true
	}
	switch l[0] {
	case ' ', '+', '-', '\\':
		return true
	}
	return false
}

// oldDelta and newDelta report how a body line advances each side's counter.
//
// An empty string is not a diff line: a blank context line is written as a
// single space. "" only appears as the trailing artifact of splitting on "\n",
// and counting it inflates the hunk header, which git rejects as a corrupt
// patch.
func oldDelta(l string) int {
	if l == "" {
		return 0
	}
	if l[0] == ' ' || l[0] == '-' {
		return 1
	}
	return 0
}

func newDelta(l string) int {
	if l == "" {
		return 0
	}
	if l[0] == ' ' || l[0] == '+' {
		return 1
	}
	return 0
}

func countOld(body []string) int {
	n := 0
	for _, l := range body {
		if strings.HasPrefix(l, "\\ No newline") {
			continue
		}
		n += oldDelta(l)
	}
	return n
}

func countNew(body []string) int {
	n := 0
	for _, l := range body {
		if strings.HasPrefix(l, "\\ No newline") {
			continue
		}
		n += newDelta(l)
	}
	return n
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
