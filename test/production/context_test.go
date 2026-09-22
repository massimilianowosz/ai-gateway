package production

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// CCR keeps recovered context under a scope of (team_id, key_hash,
// session_id), with no shared bucket and no global fallback. That claim is
// only worth anything if a second scope cannot read the first one's context,
// and the failure mode it guards against — one caller seeing another's data —
// is the kind that does not announce itself. So the assertions here are
// negative on purpose: what must NOT come back.
func TestProdContextIsolation(t *testing.T) {
	admin := masterClient(t)
	suffix := uniqueSuffix()

	// A token that cannot plausibly be in a model's weights or in any other
	// conversation, so recalling it can only mean the context leaked.
	secret := "ZQ7-" + suffix + "-XKD"
	planted := fmt.Sprintf(
		"Memorizza questo identificativo interno per il resto della conversazione: %s. "+
			"È l'unico dato che conta.", secret)
	probe := "Qual è l'identificativo interno che ti ho dato prima? " +
		"Se non lo conosci rispondi esattamente: SCONOSCIUTO"

	alice := ephemeralKey(t, admin, "e2e-ccr-alice-"+suffix, "", 5)
	bob := ephemeralKey(t, admin, "e2e-ccr-bob-"+suffix, "", 5)

	sessionA := "e2e-session-a-" + suffix
	sessionB := "e2e-session-b-" + suffix

	// Plant the secret in Alice's session A, with the state machinery on.
	plant := alice.chat(t, planted, map[string]string{
		"x-ubiquum-state":   "true",
		"x-ubiquum-session": sessionA,
		"x-ubiquum-guard":   "none",
	})
	requireStatus(t, plant, http.StatusOK, "impianto del contesto")

	leaked := func(t *testing.T, r response, who string) {
		t.Helper()
		requireStatus(t, r, http.StatusOK, who)
		if strings.Contains(strings.ToUpper(r.content(t)), strings.ToUpper(secret)) {
			t.Fatalf("%s ha recuperato un contesto che non è suo: %q", who, truncate(r.content(t), 200))
		}
	}

	t.Run("another key cannot recall it", func(t *testing.T) {
		// Same session id, different key: the scope must not collapse onto the
		// session alone.
		r := bob.chat(t, probe, map[string]string{
			"x-ubiquum-state":   "true",
			"x-ubiquum-session": sessionA,
			"x-ubiquum-guard":   "none",
		})
		leaked(t, r, "una seconda chiave")
	})

	t.Run("another session of the same key cannot recall it", func(t *testing.T) {
		r := alice.chat(t, probe, map[string]string{
			"x-ubiquum-state":   "true",
			"x-ubiquum-session": sessionB,
			"x-ubiquum-guard":   "none",
		})
		leaked(t, r, "un'altra sessione della stessa chiave")
	})

	t.Run("a request with no session cannot recall it", func(t *testing.T) {
		// With no session header the session is derived from the conversation
		// anchor, which differs from the planted one: still a different scope.
		r := alice.chat(t, probe, map[string]string{
			"x-ubiquum-state": "true",
			"x-ubiquum-guard": "none",
		})
		leaked(t, r, "una richiesta senza sessione")
	})
}

// The cache is the one component whose whole job is to answer without asking
// the provider, so it is also the one whose failure is invisible in the body:
// the answer looks right either way. The headers are the evidence.
func TestProdSemanticCache(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-cache-"+uniqueSuffix(), "", 5)

	// Distinctive enough that no earlier run seeded it.
	prompt := "Rispondi con la sola parola CIAO. Riferimento interno " + uniqueSuffix()
	on := map[string]string{"x-ubiquum-cache": "true", "x-ubiquum-guard": "none", "x-ubiquum-state": "false"}

	first := caller.chat(t, prompt, on)
	requireStatus(t, first, http.StatusOK, "prima richiesta")

	second := caller.chat(t, prompt, on)
	requireStatus(t, second, http.StatusOK, "seconda richiesta identica")

	// Not asserted as a MISS: the cache matches on meaning, not on bytes, so a
	// prompt shaped like the previous run's can legitimately hit on its first
	// send. Recorded rather than judged.
	t.Logf("prima richiesta: status=%q score=%q",
		first.headers.Get("x-hivecache-status"), first.headers.Get("x-hivecache-score"))

	status := second.headers.Get("x-hivecache-status")
	if status == "MISS" {
		t.Fatalf("la seconda richiesta identica è di nuovo un MISS: score=%q",
			second.headers.Get("x-hivecache-score"))
	}
	if saved := second.headers.Get("x-hivecache-tokens-saved"); saved == "" || saved == "0" {
		t.Errorf("un colpo di cache che non risparmia token: status=%q saved=%q", status, saved)
	}
}

// HiveState rewrites the history of a long conversation before it reaches the
// provider. Below its threshold it must leave the request alone, which is the
// half that protects short conversations from being paid for twice.
func TestProdHiveStateLeavesShortConversationsAlone(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-state-"+uniqueSuffix(), "", 5)

	r := caller.chat(t, "Rispondi con la sola parola: ciao", map[string]string{
		"x-ubiquum-state": "true",
		"x-ubiquum-guard": "none",
	})
	requireStatus(t, r, http.StatusOK, "conversazione breve")

	// "none" is what the deployment reports when it left the history alone.
	if mode := r.headers.Get("x-hivestate-mode"); mode != "" && mode != "none" && mode != "passthrough" && mode != "skip" {
		t.Errorf("una conversazione breve è stata compressa: x-hivestate-mode=%q, tokens=%q",
			mode, r.headers.Get("x-hivestate-tokens"))
	}
}

// Turning a feature off through its header has to actually turn it off:
// otherwise a caller who opts out is still paying for the extraction model.
func TestProdFeatureHeadersOptOut(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-headers-"+uniqueSuffix(), "", 5)

	r := caller.chat(t, "Rispondi con la sola parola: ciao", map[string]string{
		"x-ubiquum-state": "false",
		"x-ubiquum-cache": "false",
		"x-ubiquum-route": "false",
		"x-ubiquum-guard": "none",
	})
	requireStatus(t, r, http.StatusOK, "tutto disattivato")

	for _, header := range []string{"x-hivestate-mode", "x-hivestate-tokens", "x-hivestate-ratio"} {
		if value := r.headers.Get(header); value != "" {
			t.Errorf("hivestate ha lavorato nonostante x-ubiquum-state: false — %s=%q", header, value)
		}
	}
}
