package livezone

import (
	"fmt"
	"regexp"
	"strings"
)

// ansiRe matches the escape sequences build tools emit for colour and cursor
// movement. They carry no meaning once the output is text in a prompt.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// severityRe marks a line as something the agent must still see after
// compaction: failures, warnings and stack frames.
var severityRe = regexp.MustCompile(`(?i)\b(error|err|fatal|panic|fail(ed|ure)?|exception|traceback|warn(ing)?|assert(ion)?|timeout|refused|denied|not found|cannot|unable)\b`)

// stackFrameRe matches common stack-trace continuation lines, which are
// meaningless without the frames around them.
var stackFrameRe = regexp.MustCompile(`^\s+(at |File "|\.\.\.|[a-zA-Z_./]+\.(go|py|js|ts|rb|java|rs):\d+)`)

// looksLikeLogs recognises line-oriented output: many lines, mostly short, no
// JSON envelope. Prose and source code are deliberately excluded — the first
// is not compressible this way and the second is content the agent quotes
// verbatim.
func looksLikeLogs(t string) bool {
	lines := strings.Split(t, "\n")
	if len(lines) < 10 {
		return false
	}
	var nonEmpty, timestamped, longLines int
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonEmpty++
		if len(l) > 400 {
			longLines++
		}
		if timestampish(l) || severityRe.MatchString(l) || ansiRe.MatchString(l) {
			timestamped++
		}
	}
	if nonEmpty < 10 {
		return false
	}
	// Mostly-prose blocks have few marker lines; wrapped prose has long lines.
	return timestamped*4 >= nonEmpty && longLines*4 < nonEmpty
}

// timestampish spots the leading clock or counter most log formats carry.
func timestampish(l string) bool {
	if len(l) < 8 {
		return false
	}
	head := l
	if len(head) > 40 {
		head = head[:40]
	}
	digits := 0
	for _, r := range head {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 6 && (strings.Contains(head, ":") || strings.Contains(head, "-") || strings.Contains(head, "/"))
}

// looksLikeRepetitive recognises output that is worth collapsing for the one
// reason that needs no interpretation: it literally repeats itself.
//
// looksLikeLogs asks what the text *is* — a timestamp, a severity word, an
// escape sequence — and a great deal of real build output answers none of
// those. `npm install`, a webpack build, a compiler emitting the same note per
// file: no clock, no level, just the same line again and again. This asks the
// narrower question of whether run-collapsing would remove anything, which is
// the only thing compactLogs does to a line it has not been told to protect.
//
// It is deliberately the last detector consulted, so anything with a real
// shape is classified by that shape instead.
func looksLikeRepetitive(t string) bool {
	lines := strings.Split(t, "\n")
	if len(lines) < 10 {
		return false
	}
	var nonEmpty, longLines int
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonEmpty++
		if len(l) > 400 {
			longLines++
		}
	}
	if nonEmpty < 10 || longLines*4 >= nonEmpty {
		return false // wrapped prose, not output
	}

	// Count exactly what compactLogs would remove: the tail of every run of
	// three or more identical lines.
	removable := 0
	for i := 0; i < len(lines); {
		run := 1
		for i+run < len(lines) && lines[i+run] == lines[i] {
			run++
		}
		if run > 2 && strings.TrimSpace(lines[i]) != "" {
			removable += run - 1
		}
		i += run
	}
	return removable >= 5 && removable*10 >= nonEmpty
}

// compactLogs collapses repetition in line-oriented output.
//
// It is lossy, and the loss is bounded by one rule: a line that names a
// failure, or that belongs to a stack trace, is always kept. What it removes
// is repetition — consecutive identical lines become one line plus a count —
// terminal escape sequences, and the intermediate frames of a progress display
// that a terminal would have overwritten anyway.
//
// Ordering is preserved. Nothing is reordered, summarised or paraphrased, so
// the agent reads the same sequence of events it would have read, minus the
// duplicates it did not need.
func compactLogs(s string, opts Options) (string, string, bool) {
	stripped := ansiRe.ReplaceAllString(s, "")
	stripped = collapseRedraws(stripped)
	normalized := stripped != s

	lines := strings.Split(stripped, "\n")
	out := make([]string, 0, len(lines))
	collapsed := 0

	for i := 0; i < len(lines); {
		line := lines[i]

		// Never collapse a line that reports a problem, even if repeated: a
		// test that failed 40 times failed 40 times.
		if severityRe.MatchString(line) || stackFrameRe.MatchString(line) {
			out = append(out, line)
			i++
			continue
		}

		run := 1
		for i+run < len(lines) && lines[i+run] == line {
			run++
		}
		out = append(out, line)
		if run > 1 {
			// Two identical lines are not worth a marker; three or more are.
			if run > 2 && strings.TrimSpace(line) != "" {
				out = append(out, fmt.Sprintf("    … previous line repeated %d more times …", run-1))
				collapsed += run - 1
			} else {
				for r := 1; r < run; r++ {
					out = append(out, line)
				}
			}
		}
		i += run
	}

	result := strings.Join(out, "\n")
	if !normalized && collapsed == 0 {
		return "", "", false
	}
	// Stripping escape sequences and resolving redraws is lossless; collapsing
	// repeats is not.
	return result, "log_compact", collapsed > 0
}
