package hivestate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// outgoingPrefixHashes fingerprints the start of the body actually sent
// upstream, so two consecutive turns can be compared to find where the prompt
// stops being identical.
//
// The append-only log guarantees a stable prefix in what this package renders;
// it does not by itself guarantee a stable prefix in the bytes the provider
// tokenises. Only the outgoing body can tell us which of the two is failing,
// and a provider reporting zero cached tokens does not say where the divergence
// began. Hashes rather than content: locating the break must not put the user's
// conversation into the logs.
func outgoingPrefixHashes(body []byte) string {
	var parts []string
	for _, n := range []int{256, 1024, 4096, 16384} {
		if len(body) < n {
			break
		}
		parts = append(parts, fmt.Sprintf("%d:%s", n, shortHash(body[:n])))
	}

	var req struct {
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err == nil {
		parts = append(parts, "instr:"+shortHash([]byte(req.Instructions)))
		parts = append(parts, fmt.Sprintf("instr_len:%d", len(req.Instructions)))
		parts = append(parts, fmt.Sprintf("items:%d", len(req.Input)))

		// Every leading item up to a fixed cap, by raw array index rather than
		// a "state" counter that skips whatever it guesses is preamble. The
		// guess was the problem last time: it silently folded an item this
		// probe never saw into "preamble", and the real break turned out to
		// sit exactly where the guess stopped looking.
		for i := 0; i < len(req.Input) && i < 10; i++ {
			it := parseResponsesItem(req.Input[i])
			label := it.Type
			if it.Role != "" {
				label += "/" + it.Role
			}
			parts = append(parts, fmt.Sprintf("i%d:%s(%dB,%s)", i, label, len(req.Input[i]), shortHash(req.Input[i])))
		}
	}
	return strings.Join(parts, " ")
}

// describeLeadingItems reports the type and role of the first few input items.
// The Responses rewriter replaces everything before its split point, so this
// says whether anything the model needs — a developer preamble, a system item —
// is being dropped rather than summarised.
func describeLeadingItems(body []byte, n int) string {
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "unparsable"
	}
	var parts []string
	for i := 0; i < len(req.Input) && i < n; i++ {
		it := parseResponsesItem(req.Input[i])
		desc := it.Type
		if it.Role != "" {
			desc += "/" + it.Role
		}
		// The hash matters as much as the size: a preamble item that changes
		// between turns moves the session identity and breaks the provider
		// cache at exactly that point, and a byte count alone hides that.
		parts = append(parts, fmt.Sprintf("%d:%s(%dB,%s)", i, desc, len(req.Input[i]), shortHash(req.Input[i])))
	}
	return strings.Join(parts, " ")
}

// countUserTurns reports how many human turns a body still carries.
//
// The suspected cause of an agent losing the thread after a rewrite: facts
// survive into the state summary, but the user's own instructions are
// summarised away with everything else. Comparing this before and after the
// rewrite turns that suspicion into a number.
func countBodyUserTurns(body []byte, api APIFlavor) int {
	if api == APIAnthropic {
		var req struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return -1
		}
		n := 0
		for _, raw := range req.Messages {
			var m struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(raw, &m) == nil && m.Role == "user" {
				n++
			}
		}
		return n
	}

	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return -1
	}
	n := 0
	for _, raw := range req.Input {
		it := parseResponsesItem(raw)
		if it.Type == "message" && it.Role == "user" {
			n++
		}
	}
	return n
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:10]
}
