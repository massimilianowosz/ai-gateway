package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paidRoutes is every endpoint that can reach a provider, with a body each will
// parse. The funding gate lives in the handlers, where the model is known, and
// is therefore fail-open: a route added without it simply does not have it.
// Five shipped that way.
var paidRoutes = []struct{ name, path, body string }{
	{"chat completions", "/v1/chat/completions", `{"model":"paid","messages":[{"role":"user","content":"hi"}]}`},
	{"completions", "/v1/completions", `{"model":"paid","prompt":"hi"}`},
	{"messages", "/v1/messages", `{"model":"paid","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`},
	{"responses", "/v1/responses", `{"model":"paid","input":"hi"}`},
	{"embeddings", "/v1/embeddings", `{"model":"paid","input":"hi"}`},
	{"moderations", "/v1/moderations", `{"model":"paid","input":"hi"}`},
	{"images", "/v1/images/generations", `{"model":"paid","prompt":"hi"}`},
	{"audio speech", "/v1/audio/speech", `{"model":"paid","input":"hi","voice":"alloy"}`},
}

// Every paid route refuses a key with no budget, and none of them refuses a
// funded one for that reason.
func TestFundingGate_CoversEveryPaidRoute(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	funded := g.mintKey(100, "paid")
	unfunded := g.seedUnfundedKey("paid")

	for _, rt := range paidRoutes {
		t.Run(rt.name+" refuses an unfunded key", func(t *testing.T) {
			resp := g.post(rt.path, unfunded, rt.body)
			require.Equal(t, http.StatusPaymentRequired, resp.code,
				"%s served an unfunded key (body: %s)", rt.path, resp.body)
			require.Contains(t, resp.body, "no budget",
				"%s refused for some other reason than funding", rt.path)
		})
		t.Run(rt.name+" admits a funded key", func(t *testing.T) {
			resp := g.post(rt.path, funded, rt.body)
			// Any outcome but a funding refusal: a route may reject the scripted
			// model for reasons of its own, and that is not what is under test.
			if resp.code == http.StatusPaymentRequired {
				require.NotContains(t, resp.body, "no budget",
					"%s refused a funded key for lack of budget", rt.path)
			}
		})
	}
}

// A key with no budget may still call a model the gateway does not pay for.
func TestFundingGate_UnfundedKeyReachesAFlatModel(t *testing.T) {
	g := newGateway(t, []string{"paid", "oauth"}, "oauth")
	unfunded := g.seedUnfundedKey("paid", "oauth")

	free := g.post("/v1/chat/completions", unfunded,
		`{"model":"oauth","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, free.code,
		"an OAuth pass-through model costs the gateway nothing: %s", free.body)

	paid := g.post("/v1/chat/completions", unfunded,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusPaymentRequired, paid.code, paid.body)
}

// An exhausted budget is refused on a model the gateway pays for.
func TestFundingGate_ExhaustedKeyIsRefused(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	key := g.mintKey(100, "paid")
	g.setSpend(key, 100)

	resp := g.post("/v1/chat/completions", key,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusPaymentRequired, resp.code, resp.body)
	assert.Contains(t, resp.body, "spent its budget",
		"an exhausted key was told it never had a budget")
}

// A model the gateway does not pay for cannot consume a budget, so an exhausted
// one does not bar it — the same reasoning that lets an unfunded key use it.
func TestFundingGate_ExhaustedKeyMayStillUseFlatModels(t *testing.T) {
	g := newGateway(t, []string{"paid", "flat"}, "flat")
	key := g.mintKey(100, "paid", "flat")
	g.setSpend(key, 100)

	resp := g.post("/v1/chat/completions", key,
		`{"model":"flat","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, resp.code,
		"a flat model was blocked by a metered budget it cannot spend: %s", resp.body)
}

// A key that has run out must still be able to find out why. The gate used to
// sit in authentication, which sees every route, so an exhausted key could not
// read its own spend or list the models it was entitled to.
func TestFundingGate_ExhaustedKeyCanStillReadItsOwnState(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	key := g.mintKey(100, "paid")
	g.setSpend(key, 100)

	for _, path := range []string{"/v1/models", "/v1/key/info"} {
		resp := g.do("GET", path, key, "", "")
		assert.Equal(t, http.StatusOK, resp.code,
			"%s was blocked, so the key cannot see why it is stopped: %s", path, resp.body)
	}
}

// The master key administers the gateway and carries no budget of its own.
func TestFundingGate_MasterKeyIsNotGated(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	resp := g.post("/v1/chat/completions", masterKey,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, resp.code,
		"the operator was locked out of its own gateway: %s", resp.body)
}

// A key's model whitelist is enforced on the way in.
func TestAccess_KeyWhitelistIsEnforced(t *testing.T) {
	g := newGateway(t, []string{"allowed", "forbidden"})
	key := g.mintKey(100, "allowed")

	ok := g.post("/v1/chat/completions", key,
		`{"model":"allowed","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, ok.code, ok.body)

	no := g.post("/v1/chat/completions", key,
		`{"model":"forbidden","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusForbidden, no.code, no.body)
}

// An unauthenticated caller gets nowhere.
func TestAccess_NoKeyIsRefused(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	resp := g.post("/v1/chat/completions", "",
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusUnauthorized, resp.code, resp.body)
}
