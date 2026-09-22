package livezone

import (
	"bytes"
	"encoding/json"
)

// looksLikeJSON confirms the content actually parses. Detection by first
// character alone would send prose that happens to start with a brace into the
// JSON transformer.
func looksLikeJSON(t string) bool {
	return json.Valid([]byte(t))
}

// compactJSON removes encoding overhead from a JSON document.
//
// This is lossless: indentation and inter-token whitespace carry no meaning to
// the model, and every value, key and ordering survives byte-identical. Tool
// output from HTTP APIs and database clients is routinely pretty-printed, so
// the saving is large and free.
//
// Numbers are preserved as written rather than round-tripped through float64,
// which would turn 1.0 into 1 and lose precision above 2^53.
func compactJSON(s string, opts Options) (string, string, bool) {
	// When lossy work is permitted, try array truncation first: it subsumes
	// compaction (the re-encode is already compact) and saves far more.
	if opts.AllowLossy {
		if out, ok := truncateJSONArrays(s, opts.JSONTruncate); ok && len(out) < len(s) {
			return out, "json_truncate", true
		}
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		// Detect said it parses; if compaction disagrees, pass through.
		return "", "", false
	}
	out := buf.String()
	if out == s {
		return "", "", false
	}
	return out, "json_compact", false
}
