package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func ctxWithTeam(t *store.Team) context.Context {
	return context.WithValue(context.Background(), teamInfoKey, t)
}

// restricted:true used to affect only what GET /v1/models listed. A team with
// an empty allowed_models fell through to "allowed", so it could simply name
// the model in a request and be served.
func TestIsModelAllowed_RestrictedNeedsAnExplicitGrant(t *testing.T) {
	openTeam := &store.Team{ID: "team-b"} // no allowed_models, no granted_models
	granted := &store.Team{ID: "team-a", GrantedModels: store.StringList{"gpt-5-internal"}}

	assert.False(t, IsModelAllowed(ctxWithTeam(openTeam), "gpt-5-internal", true),
		"an unrestricted-by-default team must not reach a restricted model")
	assert.True(t, IsModelAllowed(ctxWithTeam(granted), "gpt-5-internal", true),
		"the team it was granted to must reach it")

	// A key with no team holds no grant.
	assert.False(t, IsModelAllowed(context.Background(), "gpt-5-internal", true))

	// Unrestricted models are unaffected.
	assert.True(t, IsModelAllowed(ctxWithTeam(openTeam), "gpt-4o", false))
}

// An allowed_models whitelist still applies to a restricted model that was
// granted: the grant answers "may this team see it", not "bypass everything".
func TestIsModelAllowed_UnrestrictedStillHonoursWhitelist(t *testing.T) {
	team := &store.Team{ID: "team-c", AllowedModels: store.StringList{"gpt-4o"}}
	assert.True(t, IsModelAllowed(ctxWithTeam(team), "gpt-4o", false))
	assert.False(t, IsModelAllowed(ctxWithTeam(team), "claude-sonnet-4", false))
}

// The master key is the operator and a pass-through caller carries its own
// upstream credentials. Requiring a team grant locked both out of restricted
// models — the master key could see one listed and not call it.
func TestIsModelAllowed_PrivilegedPrincipalsReachRestricted(t *testing.T) {
	master := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "master", Name: "master", Active: true})
	passthrough := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "passthrough", Name: "passthrough", Active: true})

	assert.True(t, IsModelAllowed(master, "gpt-5-internal", true), "the master key must not be gated")
	assert.True(t, IsModelAllowed(passthrough, "gpt-5-internal", true), "pass-through is granted upstream")

	// An ordinary team-less key still is.
	ordinary := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true})
	assert.False(t, IsModelAllowed(ordinary, "gpt-5-internal", true))
}

// Revoking every model produced an empty allowed_models, which read as "no
// whitelist" and therefore allowed everything — the exact opposite of what the
// operator asked for. NULL and [] must stay distinguishable.
func TestIsModelAllowed_EmptyWhitelistDeniesEverything(t *testing.T) {
	revoked := &store.Team{ID: "team-d", AllowedModels: store.StringList{}}
	assert.False(t, IsModelAllowed(ctxWithTeam(revoked), "gpt-4o", false),
		"a team whose whitelist was emptied must reach nothing")

	unconfigured := &store.Team{ID: "team-e"} // allowed_models NULL
	assert.True(t, IsModelAllowed(ctxWithTeam(unconfigured), "gpt-4o", false),
		"a team that never had a whitelist keeps open access")

	// granted_models is a separate axis and still wins.
	granted := &store.Team{ID: "team-f", AllowedModels: store.StringList{}, GrantedModels: store.StringList{"gpt-4o"}}
	assert.True(t, IsModelAllowed(ctxWithTeam(granted), "gpt-4o", false))
}

// Canonicalization rebuilt every list, turning a nil whitelist into an empty
// one. With [] now meaning "deny all", that alone would have revoked access
// for every tenant that had never configured a whitelist.
func TestCanonicalModelList_PreservesTheAbsenceOfAWhitelist(t *testing.T) {
	identity := func(s string) string { return s }

	assert.Nil(t, canonicalModelList(nil, identity),
		"an absent whitelist must not become an empty one")
	assert.NotNil(t, canonicalModelList(store.StringList{}, identity),
		"an empty whitelist must stay empty, not become absent")

	team := &store.Team{ID: "team-g"} // no whitelist at all
	ctx := ContextWithCanonicalModels(ctxWithTeam(team), identity)
	assert.True(t, IsModelAllowed(ctx, "gpt-4o", false))
}
