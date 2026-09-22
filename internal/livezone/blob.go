package livezone

import (
	"fmt"
	"strings"
)

const (
	// blobMinLines is how much encoded data must be present before eliding it
	// beats leaving it alone.
	blobMinLines = 12
	// blobKeepEdge keeps the lines that identify the payload — a PEM header,
	// a trailing checksum — at both ends.
	blobKeepEdge = 2
)

// isBlobLine matches a long uninterrupted run of the alphabet base64, hex and
// the URL-safe variants draw from.
func isBlobLine(l string) bool {
	if len(l) < 40 {
		return false
	}
	for i := 0; i < len(l); i++ {
		switch c := l[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+', c == '/', c == '=', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// looksLikeBlob recognises encoded binary pasted into the conversation: a key,
// a certificate, an embedded image, a dumped archive.
func looksLikeBlob(t string) bool {
	var nonEmpty, blob int
	for _, l := range strings.Split(t, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		nonEmpty++
		if isBlobLine(l) {
			blob++
		}
	}
	if blob < blobMinLines {
		return false
	}
	return blob*5 >= nonEmpty*4
}

// compactBlob replaces the middle of an encoded payload with a note of what
// was there.
//
// Encoded binary is the one payload an agent cannot read: there is no decision
// to take from the middle of a base64 stream, while the ends carry what
// identifies it. The removal is real and unrecoverable until the retrieval
// path exists, which is why it waits for AllowLossy.
func compactBlob(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) <= blobKeepEdge*2+1 {
		return "", false
	}

	elided := lines[blobKeepEdge : len(lines)-blobKeepEdge]
	bytes := 0
	for _, l := range elided {
		bytes += len(l) + 1
	}

	out := make([]string, 0, blobKeepEdge*2+1)
	out = append(out, lines[:blobKeepEdge]...)
	out = append(out, fmt.Sprintf("[… %d lines of encoded data elided, %d bytes …]", len(elided), bytes))
	out = append(out, lines[len(lines)-blobKeepEdge:]...)
	return strings.Join(out, "\n"), true
}
