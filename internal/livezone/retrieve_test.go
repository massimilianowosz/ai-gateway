package livezone

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testVault(t *testing.T) *Vault {
	t.Helper()
	v := NewVault(8, 4096, time.Minute)
	if v == nil {
		t.Fatal("NewVault returned nil for valid bounds")
	}
	return v
}

// The identifier ends up inside the prompt, so it has to come out the same on
// every turn. A random token would change the bytes the provider sees and cost
// the prefix cache the compression was meant to protect.
func TestVaultIDIsContentAddressed(t *testing.T) {
	if ID("hello") != ID("hello") {
		t.Error("same content produced two different ids")
	}
	if ID("hello") == ID("hellp") {
		t.Error("different content produced the same id")
	}

	v := testVault(t)
	first, _ := v.Put("scope-a", "the original output")
	second, _ := v.Put("scope-a", "the original output")
	if first != second {
		t.Errorf("storing twice produced %q then %q", first, second)
	}
}

// What the vault holds is verbatim tool output. A lookup that crossed scopes
// would hand one tenant another tenant's data.
func TestVaultIsolatesScopes(t *testing.T) {
	v := testVault(t)
	id, ok := v.Put("tenant-a", "tenant a's source code")
	if !ok {
		t.Fatal("put failed")
	}
	if _, ok := v.Get("tenant-b", id); ok {
		t.Error("another scope read the content back")
	}
	if got, ok := v.Get("tenant-a", id); !ok || got != "tenant a's source code" {
		t.Errorf("owner could not read its own content back: %q %v", got, ok)
	}
}

func TestVaultRefusesUnusableConfiguration(t *testing.T) {
	for _, tt := range []struct {
		ent, bytes int
		ttl        time.Duration
	}{
		{0, 4096, time.Minute}, {8, 0, time.Minute}, {8, 4096, 0},
	} {
		if NewVault(tt.ent, tt.bytes, tt.ttl) != nil {
			t.Errorf("NewVault(%d, %d, %v) returned a store that cannot forget", tt.ent, tt.bytes, tt.ttl)
		}
	}
	var nilVault *Vault
	if _, ok := nilVault.Put("s", "c"); ok {
		t.Error("a nil vault claimed to store")
	}
	if _, ok := nilVault.Get("s", "id"); ok {
		t.Error("a nil vault returned content")
	}
}

// A store that cannot forget is a leak.
func TestVaultEvictsPastItsBudget(t *testing.T) {
	v := NewVault(3, 4096, time.Minute)
	var ids []string
	for _, s := range []string{"one", "two", "three", "four", "five"} {
		id, _ := v.Put("scope", strings.Repeat(s, 20))
		ids = append(ids, id)
	}
	if entries, _ := v.Stats(); entries > 3 {
		t.Errorf("held %d entries, budget was 3", entries)
	}
	if _, ok := v.Get("scope", ids[0]); ok {
		t.Error("the oldest entry survived eviction")
	}
	if _, ok := v.Get("scope", ids[len(ids)-1]); !ok {
		t.Error("the newest entry was evicted")
	}
}

func TestVaultExpires(t *testing.T) {
	v := NewVault(8, 4096, time.Millisecond)
	id, _ := v.Put("scope", "short lived")
	time.Sleep(5 * time.Millisecond)
	if _, ok := v.Get("scope", id); ok {
		t.Error("content outlived its TTL")
	}
}

// A lossy transform that shed 90 bytes has not earned a 70-byte footnote.
func TestReversibleSkipsWhenTheNoteEatsTheSaving(t *testing.T) {
	pol := DefaultPolicy()
	pol.Vault = testVault(t)
	pol.Scope = "scope"

	original := strings.Repeat("x", 200)
	res := Result{Content: strings.Repeat("x", 180), Lossy: true, Applied: true}
	out, noted := reversible(pol, res, original)
	if noted || out != res.Content {
		t.Errorf("appended a note that cost more than the transform saved: %q", out)
	}
}

func TestReversibleAttachesRetrievableNote(t *testing.T) {
	pol := DefaultPolicy()
	pol.Vault = testVault(t)
	pol.Scope = "scope"

	original := strings.Repeat("original content line\n", 40)
	res := Result{Content: "[shortened]", Lossy: true, Applied: true}
	out, noted := reversible(pol, res, original)
	if !noted {
		t.Fatal("no note attached")
	}
	if !strings.Contains(out, RetrievePath) {
		t.Errorf("the note does not say where to look: %q", out)
	}
	if got, ok := pol.Vault.Get("scope", ID(original)); !ok || got != original {
		t.Error("the original was not stored under the advertised id")
	}
}

// A lossless transform has nothing to retrieve.
func TestReversibleIgnoresLosslessTransforms(t *testing.T) {
	pol := DefaultPolicy()
	pol.Vault = testVault(t)
	pol.Scope = "scope"

	res := Result{Content: "compact", Lossy: false, Applied: true}
	if out, noted := reversible(pol, res, "compact  "); noted || out != "compact" {
		t.Errorf("offered retrieval for content that was never removed: %q", out)
	}
}

func TestRetrieveIDRejectsAnythingButAnIdentifier(t *testing.T) {
	tests := map[string]string{
		RetrievePath + "a1b2c3d4e5f6": "a1b2c3d4e5f6",
		RetrievePath + "../secrets":   "",
		RetrievePath + "a/b":          "",
		RetrievePath:                  "",
		"/other/path":                 "",
		RetrievePath + "NOTHEX":       "",
	}
	for in, want := range tests {
		if got := RetrieveID(in); got != want {
			t.Errorf("RetrieveID(%q) = %q, want %q", in, got, want)
		}
	}
}

// An id from another key must look exactly like one that expired: no hint that
// it exists at all.
func TestRetrieveHandlerHidesOtherScopes(t *testing.T) {
	defaultVault = testVault(t)
	t.Cleanup(func() { defaultVault = nil })
	id, _ := defaultVault.Put(ScopeFor("team-a", "key-a"), "secret build output")

	// No key on the request, so the caller's scope cannot match the owner's.
	req := httptest.NewRequest("GET", RetrievePath+id, nil)
	w := httptest.NewRecorder()
	RetrieveHandler(nil).ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("status %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret build output") {
		t.Error("content leaked to a caller outside its scope")
	}
}

func TestRetrieveHandlerRejectsMalformedID(t *testing.T) {
	defaultVault = testVault(t)
	t.Cleanup(func() { defaultVault = nil })

	req := httptest.NewRequest("GET", RetrievePath+"not-hex", nil)
	w := httptest.NewRecorder()
	RetrieveHandler(nil).ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status %d, want 400", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Errorf("error body is not JSON: %v", err)
	}
}

func TestScopeForIsEmptyWithoutIdentity(t *testing.T) {
	if ScopeFor("", "") != "" {
		t.Error("an unidentified caller got a partition to store in")
	}
	if ScopeFor("a", "bc") == ScopeFor("ab", "c") {
		t.Error("scope encoding is not injective")
	}
}
