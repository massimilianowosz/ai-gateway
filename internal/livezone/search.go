package livezone

import "strings"

// searchMinLines is the number of result lines below which grouping cannot
// repay the per-file heading it adds.
const searchMinLines = 6

// parseSearchLine splits a grep-style "path:line:content" result.
//
// Only the colon form is recognised. Grep writes context lines with '-'
// instead, but a path may legitimately contain a dash, and splitting on the
// wrong one would silently reattribute a match to a file that does not exist.
//
// The candidate path may not contain whitespace, which is what keeps an
// ordinary timestamped log line out: "2026-01-01 12:00:00 starting" offers
// ":00:" to the digit test and would otherwise be filed under a path of
// "2026-01-01 12".
func parseSearchLine(l string) (path, entry string, ok bool) {
	for i := 1; i < len(l); i++ {
		switch l[i] {
		case ' ', '\t':
			return "", "", false
		case ':':
		default:
			continue
		}
		rest := l[i+1:]
		digits := 0
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits >= len(rest) || rest[digits] != ':' {
			continue
		}
		return l[:i], rest, true
	}
	return "", "", false
}

// looksLikeSearch recognises the output of a code-search tool: every line a
// "path:line:content" hit, with at least one file matched more than once.
//
// The all-or-nothing rule is deliberate. A block that mixes hits with summary
// lines or ripgrep's "--" separators would need those interleaved back into
// the grouping, and getting that wrong reorders results.
func looksLikeSearch(t string) bool {
	var nonEmpty, parsed int
	paths := map[string]struct{}{}
	for _, l := range strings.Split(t, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonEmpty++
		p, _, ok := parseSearchLine(l)
		if !ok {
			return false
		}
		parsed++
		paths[p] = struct{}{}
	}
	if parsed < searchMinLines || parsed != nonEmpty {
		return false
	}
	// One hit per file is a list, not a grouped result set: there is no
	// repeated path to factor out.
	return len(paths) < parsed
}

// compactSearch states each file's path once instead of once per hit.
//
// Nothing is discarded and nothing is reordered: a heading is emitted only
// where the path changes, so hits keep the order the search produced them in,
// and a heading plus a hit reconstructs the original line exactly. On a
// hundred-hit grep across a handful of files the repeated path is most of the
// payload.
func compactSearch(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	prev := ""
	grouped := 0

	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			out = append(out, l)
			continue
		}
		path, entry, ok := parseSearchLine(l)
		if !ok {
			return "", false
		}
		if path == prev {
			grouped++
		} else {
			out = append(out, path)
			prev = path
		}
		out = append(out, "  "+entry)
	}

	if grouped == 0 {
		return "", false
	}
	return strings.Join(out, "\n"), true
}
