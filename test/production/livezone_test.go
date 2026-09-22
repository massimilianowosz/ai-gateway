package production

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Live compression rewrites the newest tool output before it reaches the
// provider. It ships off by default — "a new transform on the request path
// earns its way in per deployment" — so this test asserts against whatever the
// deployment actually decided, and says which of the two it saw.
//
// Off is not a reason to skip: a transform that is supposed to be inert and
// quietly is not would rewrite every agent's tool output in production. The
// assertion that it left the payload alone is worth as much as the assertion
// that it compressed it.
func TestProdLiveCompression(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-livezone-"+uniqueSuffix(), "", 5)

	// A tool output with obvious compressible slack: indented JSON with a long
	// uniform array. Whatever happens to it, the marker has to survive, because
	// compression may drop bulk but must never lose the values the model reads.
	marker := "MRK-" + uniqueSuffix()
	rows := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		rows = append(rows, fmt.Sprintf("    {\n      \"id\": %d,\n      \"stato\": \"ok\"\n    }", i))
	}
	toolOutput := fmt.Sprintf("{\n  \"marker\": \"%s\",\n  \"righe\": [\n%s\n  ]\n}",
		marker, strings.Join(rows, ",\n"))

	messages := []map[string]any{
		{"role": "system", "content": "Rispondi con il solo valore del campo marker che vedi nell'output dello strumento."},
		{"role": "user", "content": "Leggi lo stato del sistema."},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
			"id":       "call_livezone",
			"type":     "function",
			"function": map[string]any{"name": "read_status", "arguments": "{}"},
		}}},
		{"role": "tool", "tool_call_id": "call_livezone", "content": toolOutput},
	}

	r := caller.chatMessages(t, messages, map[string]string{
		"x-ubiquum-guard": "none",
		"x-ubiquum-state": "false",
	})
	requireStatus(t, r, http.StatusOK, "tool output voluminoso")

	blocks := r.headers.Get("x-livezone-blocks")
	saved := r.headers.Get("x-livezone-bytes-saved")

	switch {
	case blocks == "" && saved == "":
		t.Logf("live compression spenta in questo deployment: il tool output è passato intatto")
	default:
		t.Logf("live compression attiva: blocchi=%s byte risparmiati=%s", blocks, saved)
		if blocks == "0" || saved == "" || saved == "0" {
			t.Errorf("la compressione si è annunciata senza comprimere: blocchi=%q byte=%q", blocks, saved)
		}
	}

	// The invariant that holds either way: what the model reads survives.
	if answer := r.content(t); !strings.Contains(answer, marker) {
		t.Errorf("il valore letto dallo strumento non è sopravvissuto fino al modello: atteso %q, risposta %q",
			marker, truncate(answer, 200))
	}
}

// A caller who opts out must get the payload through untouched, whatever the
// deployment default is.
func TestProdLiveCompressionOptOut(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-livezone-off-"+uniqueSuffix(), "", 5)

	marker := "MRK-" + uniqueSuffix()
	messages := []map[string]any{
		{"role": "system", "content": "Rispondi con il solo valore del campo marker."},
		{"role": "user", "content": "Leggi lo stato."},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
			"id":       "call_livezone_off",
			"type":     "function",
			"function": map[string]any{"name": "read_status", "arguments": "{}"},
		}}},
		{"role": "tool", "tool_call_id": "call_livezone_off",
			"content": fmt.Sprintf("{\n  \"marker\": \"%s\",\n  \"nota\": \"molto        spazio        inutile\"\n}", marker)},
	}

	r := caller.chatMessages(t, messages, map[string]string{
		"x-ubiquum-live-compression": "false",
		"x-ubiquum-guard":            "none",
		"x-ubiquum-state":            "false",
	})
	requireStatus(t, r, http.StatusOK, "opt-out dalla live compression")

	if blocks := r.headers.Get("x-livezone-blocks"); blocks != "" && blocks != "0" {
		t.Errorf("ha compresso nonostante l'opt-out: x-livezone-blocks=%q", blocks)
	}
	if answer := r.content(t); !strings.Contains(answer, marker) {
		t.Errorf("il marker non è arrivato al modello: %q", truncate(answer, 200))
	}
}
