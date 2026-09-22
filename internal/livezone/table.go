package livezone

import "strings"

// tableMinLines is the number of rows below which column padding cannot add up
// to a saving worth changing the bytes for.
const tableMinLines = 4

// interiorPadRuns counts the runs of three or more spaces that fall after the
// first non-space character: the padding that aligns columns, as opposed to
// the leading indentation that gives code, YAML and stack traces their meaning.
func interiorPadRuns(l string) int {
	body := strings.TrimLeft(l, " \t")
	runs, run := 0, 0
	for i := 0; i < len(body); i++ {
		if body[i] == ' ' {
			run++
			continue
		}
		if run >= 3 {
			runs++
		}
		run = 0
	}
	return runs
}

// looksLikeTable recognises the fixed-width output of a shell tool — docker
// ps, kubectl get, ls -l, go test — where nearly every row carries the same
// column padding.
//
// The 80% rule is what separates a table from prose or code with an aligned
// trailing comment on the odd line.
func looksLikeTable(t string) bool {
	var nonEmpty, padded int
	for _, l := range strings.Split(t, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonEmpty++
		if interiorPadRuns(l) > 0 {
			padded++
		}
	}
	if nonEmpty < tableMinLines || padded == 0 {
		return false
	}
	return padded*5 >= nonEmpty*4
}

// compactTable removes the padding that aligns fixed-width columns.
//
// A shell tool pads every cell out to the width of the widest value in its
// column, which on a wide `kubectl get` is most of the line. No value is
// touched and no row is dropped: a run of padding shrinks to the two spaces
// that still read as a column break, and leading indentation is left exactly
// as it was.
func compactTable(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	changed := false

	for i, l := range lines {
		body := strings.TrimLeft(l, " \t")
		indent := l[:len(l)-len(body)]

		var sb strings.Builder
		sb.Grow(len(body))
		run := 0
		for j := 0; j < len(body); j++ {
			if body[j] == ' ' {
				run++
				continue
			}
			writePadding(&sb, run)
			run = 0
			sb.WriteByte(body[j])
		}
		writePadding(&sb, run)

		if out := indent + sb.String(); out != l {
			lines[i] = out
			changed = true
		}
	}

	if !changed {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

// writePadding renders a run of spaces, shrinking three or more to the two
// that still read as a column break.
func writePadding(sb *strings.Builder, run int) {
	if run > 2 {
		run = 2
	}
	for ; run > 0; run-- {
		sb.WriteByte(' ')
	}
}
