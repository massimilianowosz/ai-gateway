package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/contracts/canonicaljson"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func snapshotBody(t *testing.T, mutate func(map[string]any)) map[string]any {
	t.Helper()
	policies := make([]any, 0, len(snapshotPolicyKinds))
	for _, kind := range snapshotPolicyKinds {
		status := "unbound"
		var rule any
		if kind == "model_allowlist" {
			status = "enforced"
			rule = map[string]any{"allowed_models": []any{"gpt-5"}, "denied_models": []any{"gpt-4"}}
		}
		policies = append(policies, map[string]any{
			"kind": kind, "composition_status": status, "effective_rule": rule,
			"enforcing_policies": []any{}, "shadow_policies": []any{},
			"policy_manifest_digest": "", "source": map[string]any{"origin": "gateway_default_unknown", "value": nil, "version": nil},
		})
	}
	snapshot := map[string]any{
		"schema_version": 5, "compiler_version": 1,
		"scope": map[string]any{
			"tenant_id":            "11111111-1111-4111-8111-111111111111",
			"agent_id":             "22222222-2222-4222-8222-222222222222",
			"agent_environment_id": "33333333-3333-4333-8333-333333333333",
			"environment":          "production",
		},
		"policies": policies, "authorization": map[string]any{"blocked": false, "reason": ""},
		"context": map[string]any{"pack_refs": []any{}, "digest": ""}, "default_workflow": nil,
	}
	if mutate != nil {
		mutate(snapshot)
	}
	return snapshot
}

func applyBody(t *testing.T, key string, revision int64, snapshot map[string]any) map[string]any {
	t.Helper()
	digest := canonicalDigest(t, snapshot)
	scope := snapshot["scope"].(map[string]any)
	claims, err := json.Marshal(map[string]any{
		"Contract": "ubiquum.runtime_snapshot", "EnvelopeVersion": 1,
		"TenantID": scope["tenant_id"], "AgentID": scope["agent_id"],
		"AgentEnvironmentID": scope["agent_environment_id"], "Environment": scope["environment"],
		"Revision": revision, "SchemaVersion": snapshot["schema_version"],
		"CompilerVersion": snapshot["compiler_version"], "ContentSHA256": digest,
	})
	require.NoError(t, err)
	return map[string]any{"key": key, "revision": revision, "content_sha256": digest, "snapshot": snapshot, "envelope": string(claims)}
}

func canonicalDigest(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var canonicalValue any
	require.NoError(t, decoder.Decode(&canonicalValue))
	digest, err := canonicaljson.SHA256(canonicalValue)
	require.NoError(t, err)
	return digest
}

func TestApplyGovernanceSnapshotCASAndACK(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "governed-hash", KeyPrefix: "sk-gov", Active: true, WalletAllocatedBudget: 75}
	require.NoError(t, database.CreateKey(context.Background(), key))
	snapshot := snapshotBody(t, nil)

	first := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 7, snapshot)))
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var ack store.GovernanceApplyResult
	require.NoError(t, json.NewDecoder(first.Body).Decode(&ack))
	assert.True(t, ack.Applied)
	assert.Equal(t, int64(7), ack.Revision)

	applied, err := database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Equal(t, int64(7), applied.GovernanceRevision)
	assert.Equal(t, ack.Digest, applied.GovernanceDigest)
	assert.Equal(t, store.StringList{"gpt-5"}, applied.Models)
	assert.Equal(t, store.StringList{"gpt-4"}, applied.DeniedModels)
	assert.Equal(t, 75.0, applied.Budget, "an unbound spend policy resets to the wallet allocation")
	assert.NotNil(t, applied.GovernanceAppliedAt)

	replay := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 7, snapshot)))
	require.Equal(t, http.StatusOK, replay.Code)
	require.NoError(t, json.NewDecoder(replay.Body).Decode(&ack))
	assert.False(t, ack.Applied)

	stale := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 6, snapshot)))
	assert.Equal(t, http.StatusConflict, stale.Code)

	changed := snapshotBody(t, func(value map[string]any) { value["default_workflow"] = map[string]any{"marker": "different"} })
	divergent := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 7, changed)))
	assert.Equal(t, http.StatusConflict, divergent.Code)
}

func TestApplyGovernanceSnapshotFailsClosedOnDigestAndContextCapability(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "capability-hash", KeyPrefix: "sk-cap", Active: true}
	require.NoError(t, database.CreateKey(context.Background(), key))

	badDigest := applyBody(t, key.KeyHash, 1, snapshotBody(t, nil))
	badDigest["content_sha256"] = "00"
	response := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(badDigest))
	assert.Equal(t, http.StatusUnprocessableEntity, response.Code)

	unsupported := snapshotBody(t, func(value map[string]any) {
		value["context"].(map[string]any)["pack_refs"] = []any{map[string]any{"pack_id": "44444444-4444-4444-8444-444444444444"}}
	})
	response = doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 1, unsupported)))
	assert.Equal(t, http.StatusUnprocessableEntity, response.Code)

	unchanged, err := database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Zero(t, unchanged.GovernanceRevision)
}

func TestApplyGovernanceSnapshotFailsClosedOnMissingOrMismatchedEnvelope(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "signed-hash", KeyPrefix: "sk-signed", Active: true}
	require.NoError(t, database.CreateKey(context.Background(), key))

	missing := applyBody(t, key.KeyHash, 1, snapshotBody(t, nil))
	delete(missing, "envelope")
	response := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(missing))
	assert.Equal(t, http.StatusBadRequest, response.Code)

	mismatch := applyBody(t, key.KeyHash, 1, snapshotBody(t, nil))
	var claims map[string]any
	require.NoError(t, json.Unmarshal([]byte(mismatch["envelope"].(string)), &claims))
	claims["Revision"] = 2
	raw, err := json.Marshal(claims)
	require.NoError(t, err)
	mismatch["envelope"] = string(raw)
	response = doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(mismatch))
	assert.Equal(t, http.StatusUnprocessableEntity, response.Code)

	unchanged, err := database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Zero(t, unchanged.GovernanceRevision)
}

func TestApplyGovernanceSnapshotReplacesRouteSettingsAtomically(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "route-hash", KeyPrefix: "sk-route", Active: true, TeamID: "team-1"}
	require.NoError(t, database.CreateKey(context.Background(), key))
	snapshot := snapshotBody(t, func(value map[string]any) {
		for _, raw := range value["policies"].([]any) {
			policy := raw.(map[string]any)
			switch policy["kind"] {
			case "guardrail_set":
				policy["composition_status"] = "enforced"
				policy["effective_rule"] = map[string]any{"required_guardrails": []any{"pii"}}
				policy["enforcing_policies"] = []any{map[string]any{"non_derogable": true}}
			case "response_cache":
				policy["composition_status"] = "enforced"
				policy["effective_rule"] = map[string]any{"enabled": false, "direct_threshold": 0.99}
			case "request_logging":
				policy["composition_status"] = "enforced"
				policy["effective_rule"] = map[string]any{"retention_days": 14}
			}
		}
	})
	response := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 1, snapshot)))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	route, err := database.GetKeyRouteSettings(context.Background(), key.KeyHash)
	require.NoError(t, err)
	require.NotNil(t, route)
	assert.Equal(t, int64(1), route.GovernanceRevision)
	require.NotNil(t, route.CacheEnabled)
	assert.False(t, *route.CacheEnabled)
	require.NotNil(t, route.CacheDirectThreshold)
	assert.Equal(t, 0.99, *route.CacheDirectThreshold)
	require.NotNil(t, route.GuardrailOverride)
	assert.Equal(t, []string{"pii"}, route.GuardrailOverride.RequiredGuardrails)
	assert.True(t, route.GuardrailOverride.NonDerogable)
	require.NotNil(t, route.LogRetentionDays)
	assert.Equal(t, 14, *route.LogRetentionDays)
}

func TestApplyGovernanceSnapshotClearsRemovedOverridesOnNextRevision(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "replace-hash", KeyPrefix: "sk-replace", Active: true, TeamID: "team-1"}
	require.NoError(t, database.CreateKey(context.Background(), key))
	withOverrides := snapshotBody(t, func(value map[string]any) {
		for _, raw := range value["policies"].([]any) {
			policy := raw.(map[string]any)
			switch policy["kind"] {
			case "response_cache":
				policy["composition_status"] = "enforced"
				policy["effective_rule"] = map[string]any{"enabled": false, "direct_threshold": 0.98}
			case "guardrail_set":
				policy["composition_status"] = "enforced"
				policy["effective_rule"] = map[string]any{"required_guardrails": []any{"pii"}}
				policy["enforcing_policies"] = []any{map[string]any{"non_derogable": true}}
			}
		}
	})
	first := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 1, withOverrides)))
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())

	withoutOverrides := snapshotBody(t, nil)
	second := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 2, withoutOverrides)))
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())

	route, err := database.GetKeyRouteSettings(context.Background(), key.KeyHash)
	require.NoError(t, err)
	require.NotNil(t, route)
	assert.Equal(t, int64(2), route.GovernanceRevision)
	assert.Nil(t, route.CacheEnabled)
	assert.Nil(t, route.CacheDirectThreshold)
	assert.Nil(t, route.GuardrailOverride)
	applied, err := database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Equal(t, int64(2), applied.GovernanceRevision)
}

func TestApplyGovernanceSnapshotVerifiesAndMaterializesContext(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "context-hash", KeyPrefix: "sk-context", Active: true}
	require.NoError(t, database.CreateKey(context.Background(), key))
	body := "Follow the approved release procedure."
	contentDigest := canonicalDigest(t, map[string]any{"kind": "instructions", "body": body, "source": nil, "priority": 10})
	ref := map[string]any{"pack_id": "44444444-4444-4444-8444-444444444444", "kind": "instructions", "version": 2, "priority": 10, "content_digest": contentDigest}
	snapshot := snapshotBody(t, func(value map[string]any) {
		value["context"] = map[string]any{"pack_refs": []any{ref}, "digest": canonicalDigest(t, []any{ref})}
	})
	request := applyBody(t, key.KeyHash, 1, snapshot)
	request["context_packs"] = []any{map[string]any{"pack_id": ref["pack_id"], "kind": "instructions", "version": 2, "priority": 10, "body": body, "source": nil}}
	response := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(request))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	applied, err := database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Equal(t, body, applied.ContextInstructions)
	assert.Equal(t, canonicalDigest(t, []any{ref}), applied.ContextInstructionsDigest)
}

func TestSchema6FundingUpdatesAllocationAndBoundsPolicyCeiling(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "funding-hash", KeyPrefix: "sk-funding", Active: true, WalletAllocatedBudget: 75, Budget: 75}
	require.NoError(t, database.CreateKey(context.Background(), key))
	snapshot := snapshotBody(t, func(value map[string]any) {
		value["schema_version"] = 6
		value["funding"] = map[string]any{"wallet_allocated_budget": 40}
		for _, raw := range value["policies"].([]any) {
			policy := raw.(map[string]any)
			if policy["kind"] == "spend_budget" {
				policy["composition_status"] = "enforced"
				policy["effective_rule"] = map[string]any{"max_budget": 100}
			}
		}
	})
	response := doRequest(h.ApplyGovernanceSnapshot, http.MethodPost, "/v1/governance/snapshot/apply", jsonBody(applyBody(t, key.KeyHash, 1, snapshot)))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	applied, err := database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Equal(t, 40.0, applied.WalletAllocatedBudget)
	assert.Equal(t, 40.0, applied.Budget, "a policy ceiling cannot exceed funded allocation")
}
