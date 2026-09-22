package livezone

import "strings"

// collapseRedraws resolves the carriage-return overwrites a progress display
// leaves behind.
//
// A spinner, a percentage counter or a download bar writes the same line
// hundreds of times separated by \r, and a terminal shows only the last one.
// The transcript that reaches the model carries every intermediate frame.
// Keeping the final segment reproduces what a human running the command
// actually saw, which is why this counts as lossless.
func collapseRedraws(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	// CRLF is a line ending, not an overwrite. Normalising it first is what
	// keeps the rule below from reading every line of a Windows transcript as
	// a redraw and emptying the whole block.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.ContainsRune(s, '\r') {
		return s
	}

	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if idx := strings.LastIndexByte(l, '\r'); idx >= 0 {
			lines[i] = l[idx+1:]
		}
	}
	return strings.Join(lines, "\n")
}
