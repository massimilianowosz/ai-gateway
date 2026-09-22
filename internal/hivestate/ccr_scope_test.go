package hivestate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// secretHistory builds a history the file-extraction pass will pick up: an
// assistant Read followed by the file body. The body must clear the
// extractor's 50-byte floor to be stored at all.
func secretHistory(marker string) []Message {
	path := "/srv/" + marker + "/secrets.go"
	body := "package secrets\n\n// PRIVATE KEY " + marker + "\n" +
		strings.Repeat("const filler = \"padding past the fifty byte floor\"\n", 4)
	return []Message{
		{Role: "assistant", Content: "Read(\"" + path + "\")"},
		{Role: "tool", Content: body},
	}
}

// query mentions the stored path, which is what keeps it in the working set.
func query(marker string) string {
	return "what was in /srv/" + marker + "/secrets.go again?"
}

func TestCCR_ContentNeverCrossesTenants(t *testing.T) {
	store := newCCRStore(512, 32, time.Hour)
	counter := NewTokenCounter()

	victim := Scope{TeamID: "team-a", KeyHash: "key-a", SessionID: "sess-a"}
	attacker := Scope{TeamID: "team-b", KeyHash: "key-b", SessionID: "sess-b"}

	id := store.Store(victim, secretHistory("alpha"), counter)
	if id == "" {
		t.Fatal("victim content should have been stored")
	}

	// The attacker asks precisely for the victim's content.
	if got := store.FindRelevant(attacker, query("alpha"), 4000, counter); got != nil {
		t.Fatalf("cross-tenant leak: attacker retrieved %d messages: %+v", len(got), got)
	}
	// The rightful owner still gets it.
	if got := store.FindRelevant(victim, query("alpha"), 4000, counter); len(got) == 0 {
		t.Fatal("owner lost access to its own content")
	}
}

// Same tenant, same key, different conversation: still isolated, or one
// conversation contaminates another with unrelated context.
func TestCCR_ContentNeverCrossesSessions(t *testing.T) {
	store := newCCRStore(512, 32, time.Hour)
	counter := NewTokenCounter()

	s1 := Scope{TeamID: "team-a", KeyHash: "key-a", SessionID: "conversation-1"}
	s2 := Scope{TeamID: "team-a", KeyHash: "key-a", SessionID: "conversation-2"}

	store.Store(s1, secretHistory("beta"), counter)
	if got := store.FindRelevant(s2, query("beta"), 4000, counter); got != nil {
		t.Fatalf("cross-session leak: %d messages surfaced in an unrelated conversation", len(got))
	}
}

// Same team, different API keys. Keys within a tenant can belong to different
// applications or customers, so they must not share recovered content either.
func TestCCR_ContentNeverCrossesKeys(t *testing.T) {
	store := newCCRStore(512, 32, time.Hour)
	counter := NewTokenCounter()

	k1 := Scope{TeamID: "team-a", KeyHash: "key-1", SessionID: "s"}
	k2 := Scope{TeamID: "team-a", KeyHash: "key-2", SessionID: "s"}

	store.Store(k1, secretHistory("gamma"), counter)
	if got := store.FindRelevant(k2, query("gamma"), 4000, counter); got != nil {
		t.Fatalf("cross-key leak: %d messages", len(got))
	}
}

// An under-specified scope must disable CCR, not fall back to a shared bucket.
func TestCCR_InvalidScopeStoresAndRetrievesNothing(t *testing.T) {
	store := newCCRStore(512, 32, time.Hour)
	counter := NewTokenCounter()

	cases := map[string]Scope{
		"no identity at all":   {},
		"owner but no session": {TeamID: "team-a", KeyHash: "key-a"},
		"session but no owner": {SessionID: "sess-a"},
	}
	for name, sc := range cases {
		if id := store.Store(sc, secretHistory("delta"), counter); id != "" {
			t.Errorf("%s: stored under an invalid scope (id=%q)", name, id)
		}
		if got := store.FindRelevant(sc, query("delta"), 4000, counter); got != nil {
			t.Errorf("%s: retrieved under an invalid scope", name)
		}
	}

	// And nothing an invalid scope did can be seen by a valid one.
	valid := Scope{TeamID: "team-a", KeyHash: "key-a", SessionID: "sess-a"}
	if got := store.FindRelevant(valid, query("delta"), 4000, counter); got != nil {
		t.Fatal("content from an invalid scope became visible to a valid one")
	}
}

// A scope name containing the internal separator must not be able to collide
// with a different tuple.
func TestCCR_ScopeKeysCannotBeForgedBySeparatorInjection(t *testing.T) {
	a := Scope{TeamID: "team", KeyHash: "a\x00b", SessionID: "s"}
	b := Scope{TeamID: "team", KeyHash: "a", SessionID: "b\x00s"}
	if a.key() == b.key() {
		t.Fatal("distinct scopes collided; separator is forgeable")
	}
}

// One busy tenant must not evict another's content.
func TestCCR_PerScopeQuotaIsolatesEviction(t *testing.T) {
	store := newCCRStore(512, 2, time.Hour) // 2 entries per scope
	counter := NewTokenCounter()

	quiet := Scope{TeamID: "quiet", KeyHash: "k", SessionID: "s"}
	noisy := Scope{TeamID: "noisy", KeyHash: "k", SessionID: "s"}

	store.Store(quiet, secretHistory("epsilon"), counter)

	// The noisy tenant blows well past its own quota.
	for i := 0; i < 50; i++ {
		store.Store(noisy, []Message{
			{Role: "assistant", Content: "Read(\"/noise/" + strings.Repeat("x", i+1) + ".go\")"},
			{Role: "tool", Content: strings.Repeat("noise padding past the fifty byte floor\n", 3)},
		}, counter)
	}

	if got := store.FindRelevant(quiet, query("epsilon"), 4000, counter); len(got) == 0 {
		t.Fatal("a noisy tenant evicted a quiet tenant's content")
	}
}

func TestCCR_EntriesExpire(t *testing.T) {
	store := newCCRStore(512, 32, time.Millisecond)
	counter := NewTokenCounter()
	sc := Scope{TeamID: "t", KeyHash: "k", SessionID: "s"}

	store.Store(sc, secretHistory("zeta"), counter)
	time.Sleep(5 * time.Millisecond)

	if got := store.FindRelevant(sc, query("zeta"), 4000, counter); got != nil {
		t.Fatal("expired content was still retrievable")
	}
}

func TestCCR_ForgetDropsEverythingForAScope(t *testing.T) {
	store := newCCRStore(512, 32, time.Hour)
	counter := NewTokenCounter()
	sc := Scope{TeamID: "t", KeyHash: "k", SessionID: "s"}

	store.Store(sc, secretHistory("eta"), counter)
	store.Forget(sc)

	if got := store.FindRelevant(sc, query("eta"), 4000, counter); got != nil {
		t.Fatal("Forget left content behind")
	}
}

// --- session derivation ---

func TestDeriveSessionID_ExplicitHeaderWins(t *testing.T) {
	msgs := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}
	if got := DeriveSessionID("  my-session  ", msgs); got != "my-session" {
		t.Fatalf("got %q", got)
	}
}

// Derived IDs must stay stable as the conversation grows, or CCR would file
// every turn under a new session and never retrieve anything.
func TestDeriveSessionID_StableAsConversationGrows(t *testing.T) {
	base := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "first question"}}
	grown := append(append([]Message{}, base...),
		Message{Role: "assistant", Content: "answer"},
		Message{Role: "user", Content: "second question"},
	)
	if DeriveSessionID("", base) != DeriveSessionID("", grown) {
		t.Fatal("session ID changed as the conversation grew")
	}
}

func TestDeriveSessionID_DifferentConversationsDiffer(t *testing.T) {
	a := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "question A"}}
	b := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "question B"}}
	if DeriveSessionID("", a) == DeriveSessionID("", b) {
		t.Fatal("distinct conversations produced the same session ID")
	}
}

func TestDeriveSessionID_EmptyWhenNothingToAnchorOn(t *testing.T) {
	if got := DeriveSessionID("", nil); got != "" {
		t.Fatalf("expected empty session for empty input, got %q", got)
	}
	if got := DeriveSessionID("", []Message{{Role: "assistant", Content: "orphan"}}); got != "" {
		t.Fatalf("expected empty session with no system or user anchor, got %q", got)
	}
}

// --- placement in the rewritten body ---

// Recovered context must land immediately before the final user turn. Anywhere
// earlier would sit inside the region a provider prompt cache covers.
func TestRewrite_CCRLandsAtTheTailNotTheHead(t *testing.T) {
	body := buildAnthropicBody(t, 20, false)
	result := &Result{
		StateJSON: `{"intent":"x"}`,
		CCRMessages: []Message{
			{Role: "user", Content: ccrFramingText},
			{Role: "user", Content: "RECOVERED FILE CONTENT"},
		},
	}

	out, err := rewriteAnthropicBodyWithWindow(body, result, 4)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	n := len(parsed.Messages)
	if n < 4 {
		t.Fatalf("unexpectedly short rewrite: %d messages", n)
	}
	// [..., ccr framing, ccr content, last user turn]
	if !strings.Contains(string(parsed.Messages[n-2]), "RECOVERED FILE CONTENT") {
		t.Fatalf("recovered content is not just before the last turn: %s", parsed.Messages[n-2])
	}
	if !strings.Contains(string(parsed.Messages[n-3]), "retrieved from compressed history") {
		t.Fatalf("framing message misplaced: %s", parsed.Messages[n-3])
	}
	// The state summary stays at the head; CCR must not have joined it there.
	if strings.Contains(string(parsed.Messages[0]), "RECOVERED FILE CONTENT") {
		t.Fatal("recovered content was placed at the head, inside the cacheable prefix")
	}
}

func TestRewrite_NoCCRLeavesBodyShapeUnchanged(t *testing.T) {
	body := buildAnthropicBody(t, 20, false)

	withCCR, err := rewriteAnthropicBodyWithWindow(body, &Result{StateJSON: `{"intent":"x"}`}, 4)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(withCCR, &parsed)
	for _, m := range parsed.Messages {
		if strings.Contains(string(m), "retrieved from compressed history") {
			t.Fatal("framing message appeared with no CCR content")
		}
	}
}

// Concurrent traffic from many tenants must neither race nor leak. Run under
// -race: the store is shared across every request the gateway serves.
func TestCCR_ConcurrentTenantsStayIsolated(t *testing.T) {
	store := newCCRStore(64, 8, time.Hour) // deliberately tight, to force eviction
	counter := NewTokenCounter()

	const tenants, rounds = 16, 40
	done := make(chan string, tenants)

	for i := 0; i < tenants; i++ {
		go func(i int) {
			marker := "tenant" + strings.Repeat("z", i) + "mark"
			sc := Scope{
				TeamID:    "team-" + marker,
				KeyHash:   "key-" + marker,
				SessionID: "sess-" + marker,
			}
			for r := 0; r < rounds; r++ {
				store.Store(sc, secretHistory(marker), counter)
				// Whatever comes back must belong to this tenant.
				for _, m := range store.FindRelevant(sc, query(marker), 4000, counter) {
					if !strings.Contains(m.Content, marker) && !strings.Contains(m.Content, "secrets/") {
						done <- "tenant " + marker + " saw foreign content: " + m.Content
						return
					}
					if strings.Contains(m.Content, "PRIVATE KEY ") &&
						!strings.Contains(m.Content, "PRIVATE KEY "+marker) {
						done <- "tenant " + marker + " saw another tenant's key: " + m.Content
						return
					}
				}
			}
			done <- ""
		}(i)
	}

	for i := 0; i < tenants; i++ {
		if msg := <-done; msg != "" {
			t.Fatal(msg)
		}
	}
}
