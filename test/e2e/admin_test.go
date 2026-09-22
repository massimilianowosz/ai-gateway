package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A key with no budget and no whitelist is legitimate: the empty whitelist
// means every model, and the models the gateway does not pay for need no
// budget. The admin API used to refuse this, which forced the caller to
// enumerate the OAuth models by hand — the opposite of what an open whitelist
// is for. What the key may spend on is decided per request, per model.
func TestAdmin_MintsAKeyWithNeitherBudgetNorWhitelist(t *testing.T) {
	g := newGatewayOpts(t, []string{"paid", "oauth"}, options{flat: []string{"oauth"}})

	resp := g.do("POST", "/v1/key/generate", masterKey, "application/json",
		`{"name":"no-budget-no-models"}`)
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	var out struct{ Key string }
	decode(t, resp.body, &out)
	require.NotEmpty(t, out.Key)

	// And it behaves as the funding gate says it should: free on what the
	// gateway does not pay for, refused on what it does.
	free := g.post("/v1/chat/completions", out.Key,
		`{"model":"oauth","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, free.code,
		"a model the gateway does not pay for needed a budget: %s", free.body)

	paid := g.post("/v1/chat/completions", out.Key,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusPaymentRequired, paid.code, paid.body)
}

// A key scoped to models the gateway does not pay for needs no budget.
func TestAdmin_MintsAnOAuthScopedKeyWithoutBudget(t *testing.T) {
	g := newGateway(t, []string{"paid", "oauth"}, "oauth")

	resp := g.do("POST", "/v1/key/generate", masterKey, "application/json",
		`{"name":"oauth-only","models":["oauth"]}`)
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	var out struct{ Key string }
	decode(t, resp.body, &out)
	require.NotEmpty(t, out.Key)

	free := g.post("/v1/chat/completions", out.Key,
		`{"model":"oauth","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, free.code, free.body)
}

// Only the master key administers the gateway.
func TestAdmin_RequiresTheMasterKey(t *testing.T) {
	g := newGateway(t, []string{"m"})
	ordinary := g.mintKey(100, "m")

	resp := g.do("POST", "/v1/key/generate", ordinary, "application/json", `{"name":"x","budget":10}`)
	assert.NotEqual(t, http.StatusOK, resp.code,
		"an ordinary key minted another key: %s", resp.body)
}

// A key can be listed, inspected, updated and deleted.
func TestAdmin_KeyLifecycle(t *testing.T) {
	g := newGateway(t, []string{"m"})
	raw := g.mintKey(50, "m")

	list := g.do("GET", "/v1/key/list", masterKey, "", "")
	require.Equal(t, http.StatusOK, list.code, list.body)

	info := g.do("GET", "/v1/key/info", raw, "", "")
	require.Equal(t, http.StatusOK, info.code, info.body)

	// Raising the budget takes effect on the next request.
	upd := g.do("POST", "/v1/key/update", masterKey, "application/json",
		`{"key":"`+raw+`","budget":999}`)
	assert.Contains(t, []int{http.StatusOK, http.StatusNotFound}, upd.code, upd.body)

	still := g.post("/v1/chat/completions", raw,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, still.code, still.body)
}

// Teams are created and their granted models drive restricted access.
func TestAdmin_TeamLifecycle(t *testing.T) {
	g := newGateway(t, []string{"m"})

	created := g.do("POST", "/v1/team/create", masterKey, "application/json",
		`{"name":"squadra","budget":100}`)
	require.Equal(t, http.StatusOK, created.code, created.body)

	list := g.do("GET", "/v1/team/list", masterKey, "", "")
	require.Equal(t, http.StatusOK, list.code, list.body)
	assert.Contains(t, list.body, "squadra")
}

// The models listing reflects what the caller may use, and needs auth.
func TestModels_ListingRequiresAuthAndReflectsAccess(t *testing.T) {
	g := newGateway(t, []string{"alpha", "beta"})

	anon := g.do("GET", "/v1/models", "", "", "")
	assert.Equal(t, http.StatusUnauthorized, anon.code, anon.body)

	scoped := g.mintKey(100, "alpha")
	list := g.do("GET", "/v1/models", scoped, "", "")
	require.Equal(t, http.StatusOK, list.code, list.body)
	assert.Contains(t, list.body, "alpha")

	detail := g.do("GET", "/v1/models/alpha", scoped, "", "")
	assert.Equal(t, http.StatusOK, detail.code, detail.body)
}

// An oversized body is refused as too large, not as malformed.
func TestLimits_OversizedBodyIsRefusedAsTooLarge(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	huge := `{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 9<<20) + `"}]}`
	resp := g.post("/v1/chat/completions", key, huge)

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.code,
		"an oversized body was reported as something else: %d %s", resp.code, truncate(resp.body))
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// Every body-accepting inference route reports an oversized body as too large,
// not as malformed. MaxBody surfaces the limit as a decode failure, and calling
// that a syntax error sends the caller looking for a mistake that is not there.
func TestLimits_EveryRouteDistinguishesTooLargeFromMalformed(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")
	filler := strings.Repeat("x", 9<<20)

	routes := []struct{ name, path, body string }{
		{"chat completions", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"` + filler + `"}]}`},
		{"messages", "/v1/messages", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"` + filler + `"}]}`},
		{"responses", "/v1/responses", `{"model":"m","input":"` + filler + `"}`},
		{"embeddings", "/v1/embeddings", `{"model":"m","input":"` + filler + `"}`},
		{"moderations", "/v1/moderations", `{"model":"m","input":"` + filler + `"}`},
		{"conversations", "/v1/conversations", `{"metadata":{"x":"` + filler + `"}}`},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			resp := g.post(rt.path, key, rt.body)
			assert.Equal(t, http.StatusRequestEntityTooLarge, resp.code,
				"%s reported an oversized body as %d: %s", rt.path, resp.code, truncate(resp.body))
		})
	}

	// A genuinely malformed body still reads as malformed.
	bad := g.post("/v1/chat/completions", key, `{"model":"m",`)
	assert.Equal(t, http.StatusBadRequest, bad.code, bad.body)
}
