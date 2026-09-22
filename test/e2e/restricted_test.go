package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A restricted model is reachable only by a team that was granted it. Before
// this was enforced, the flag hid the model from the listing and nothing more:
// naming it in a request was enough to be served.
func TestRestricted_OnlyAGrantedTeamMayCallIt(t *testing.T) {
	g := newGatewayWith(t, []string{"open", "secret"}, nil, []string{"secret"})
	g.scripts.set("secret", &script{complete: textAnswer("riservato")})

	g.seedTeam("team-granted", "secret")
	g.seedTeam("team-plain")

	granted := g.mintTeamKey("team-granted", 100)
	plain := g.mintTeamKey("team-plain", 100)

	ok := g.post("/v1/chat/completions", granted,
		`{"model":"secret","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, ok.code, "the granted team was refused: %s", ok.body)

	no := g.post("/v1/chat/completions", plain,
		`{"model":"secret","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusForbidden, no.code,
		"a team with no grant reached a restricted model: %s", no.body)
}

// An unrestricted model is unaffected by any of this.
func TestRestricted_OpenModelsStayOpen(t *testing.T) {
	g := newGatewayWith(t, []string{"open", "secret"}, nil, []string{"secret"})
	g.seedTeam("team-plain")
	plain := g.mintTeamKey("team-plain", 100)

	resp := g.post("/v1/chat/completions", plain,
		`{"model":"open","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, resp.code, resp.body)
}

// The listing hides what the caller may not use, and the detail endpoint does
// not confirm it exists either — answering there gives the same fact away one
// request later.
func TestRestricted_IsHiddenFromListingAndDetail(t *testing.T) {
	g := newGatewayWith(t, []string{"open", "secret"}, nil, []string{"secret"})
	g.seedTeam("team-plain")
	plain := g.mintTeamKey("team-plain", 100)

	list := g.do("GET", "/v1/models", plain, "", "")
	require.Equal(t, http.StatusOK, list.code, list.body)
	assert.Contains(t, list.body, "open")
	assert.NotContains(t, list.body, "secret", "a restricted model was listed to a team without it")

	detail := g.do("GET", "/v1/models/secret", plain, "", "")
	assert.Equal(t, http.StatusNotFound, detail.code,
		"the detail endpoint confirmed a model the listing hides: %s", detail.body)
}
