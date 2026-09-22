package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/agenttoken"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/contracts/canonicaljson"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

const runtimeSnapshotSchema = 6
const runtimeSnapshotCompiler = 1

var snapshotPolicyKinds = []string{
	"auto_recharge", "context_compression", "feedback_collection", "guardrail_set",
	"model_allowlist", "model_residency", "model_routing", "provider_allowlist",
	"rate_limit", "request_logging", "response_cache", "spend_budget",
}

type runtimeSnapshot struct {
	SchemaVersion   int `json:"schema_version"`
	CompilerVersion int `json:"compiler_version"`
	Scope           struct {
		TenantID           string  `json:"tenant_id"`
		AgentID            string  `json:"agent_id"`
		AgentEnvironmentID string  `json:"agent_environment_id"`
		Environment        *string `json:"environment"`
	} `json:"scope"`
	Policies []struct {
		Kind                 string           `json:"kind"`
		CompositionStatus    string           `json:"composition_status"`
		EffectiveRule        map[string]any   `json:"effective_rule"`
		EnforcingPolicies    []map[string]any `json:"enforcing_policies"`
		ShadowPolicies       []map[string]any `json:"shadow_policies"`
		PolicyManifestDigest string           `json:"policy_manifest_digest"`
		Source               map[string]any   `json:"source"`
	} `json:"policies"`
	Authorization struct {
		Blocked bool   `json:"blocked"`
		Reason  string `json:"reason"`
	} `json:"authorization"`
	Context struct {
		PackRefs []map[string]any `json:"pack_refs"`
		Digest   string           `json:"digest"`
	} `json:"context"`
	DefaultWorkflow any `json:"default_workflow"`
	Funding         *struct {
		WalletAllocatedBudget *json.Number `json:"wallet_allocated_budget"`
	} `json:"funding,omitempty"`
}

type governanceApplyRequest struct {
	Key           string                `json:"key"`
	Revision      int64                 `json:"revision"`
	ContentSHA256 string                `json:"content_sha256"`
	Snapshot      json.RawMessage       `json:"snapshot"`
	ContextPacks  []contextPackMaterial `json:"context_packs"`
	Envelope      string                `json:"envelope"`
}

type contextPackMaterial struct {
	PackID   string  `json:"pack_id"`
	Kind     string  `json:"kind"`
	Version  int     `json:"version"`
	Priority int     `json:"priority"`
	Body     *string `json:"body"`
	Source   any     `json:"source"`
}

func materializeContext(snapshot runtimeSnapshot, materials []contextPackMaterial) (string, string, error) {
	byID := make(map[string]contextPackMaterial, len(materials))
	for _, material := range materials {
		key := fmt.Sprintf("%s:%d", material.PackID, material.Version)
		if _, exists := byID[key]; exists {
			return "", "", errors.New("duplicate context material")
		}
		byID[key] = material
	}
	var instructions []string
	var instructionRefs []any
	for _, ref := range snapshot.Context.PackRefs {
		packID, idOK := ref["pack_id"].(string)
		kind, kindOK := ref["kind"].(string)
		versionValue, versionErr := decodeNumber(ref["version"], "context version")
		priorityValue, priorityErr := decodeNumber(ref["priority"], "context priority")
		expectedDigest, digestOK := ref["content_digest"].(string)
		if !idOK || !kindOK || versionErr != nil || priorityErr != nil || !digestOK {
			return "", "", errors.New("invalid context reference")
		}
		key := fmt.Sprintf("%s:%d", packID, int(versionValue))
		material, exists := byID[key]
		if !exists || material.Kind != kind || material.Priority != int(priorityValue) {
			return "", "", errors.New("context material does not match snapshot reference")
		}
		content := map[string]any{"kind": material.Kind, "body": nil, "source": material.Source, "priority": json.Number(strconv.Itoa(material.Priority))}
		if material.Body != nil {
			content["body"] = *material.Body
		}
		digest, err := canonicaljson.SHA256(content)
		if err != nil || digest != expectedDigest {
			return "", "", errors.New("context material digest mismatch")
		}
		delete(byID, key)
		if kind == "instructions" {
			if material.Body != nil {
				instructions = append(instructions, *material.Body)
			} else {
				instructions = append(instructions, "")
			}
			instructionRefs = append(instructionRefs, ref)
		}
	}
	if len(byID) != 0 {
		return "", "", errors.New("context material is not referenced by snapshot")
	}
	if len(snapshot.Context.PackRefs) == 0 {
		return "", "", nil
	}
	allRefs := make([]any, len(snapshot.Context.PackRefs))
	for index := range snapshot.Context.PackRefs {
		allRefs[index] = snapshot.Context.PackRefs[index]
	}
	manifestDigest, err := canonicaljson.SHA256(allRefs)
	if err != nil || manifestDigest != snapshot.Context.Digest {
		return "", "", errors.New("context manifest digest mismatch")
	}
	instructionDigest := ""
	if len(instructionRefs) > 0 {
		instructionDigest, err = canonicaljson.SHA256(instructionRefs)
		if err != nil {
			return "", "", err
		}
	}
	return strings.Join(instructions, "\n\n"), instructionDigest, nil
}

func decodeNumber(value any, name string) (float64, error) {
	switch number := value.(type) {
	case json.Number:
		result, err := strconv.ParseFloat(number.String(), 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be numeric", name)
		}
		return result, nil
	case float64:
		return number, nil
	default:
		return 0, fmt.Errorf("%s must be numeric", name)
	}
}

func stringList(rule map[string]any, name string) (store.StringList, error) {
	value, exists := rule[name]
	if !exists || value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	result := make(store.StringList, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("%s entries must be non-empty strings", name)
		}
		result[index] = text
	}
	return result, nil
}

func compileKeyProjection(snapshot runtimeSnapshot, materials []contextPackMaterial, keyHash, digest, canonical string, revision int64) (store.GovernanceApplyParams, error) {
	params := store.GovernanceApplyParams{
		KeyHash: keyHash, AgentID: snapshot.Scope.AgentID, AgentEnvironmentID: snapshot.Scope.AgentEnvironmentID,
		Digest: digest, CanonicalSnapshot: canonical, Revision: revision,
		SchemaVersion: snapshot.SchemaVersion, CompilerVersion: snapshot.CompilerVersion,
		GovernanceBlocked:     snapshot.Authorization.Blocked,
		GovernanceBlockReason: snapshot.Authorization.Reason,
	}
	if snapshot.SchemaVersion >= 6 {
		if snapshot.Funding == nil {
			return params, errors.New("schema 6 snapshot requires funding")
		}
		if snapshot.Funding.WalletAllocatedBudget != nil {
			allocation, err := decodeNumber(*snapshot.Funding.WalletAllocatedBudget, "wallet_allocated_budget")
			if err != nil || allocation < 0 {
				return params, errors.New("wallet_allocated_budget must be non-negative")
			}
			params.WalletAllocatedBudget = &allocation
		}
	}
	seen := map[string]bool{}
	if snapshot.Authorization.Blocked && snapshot.Authorization.Reason == "" {
		return params, errors.New("blocked authorization requires a reason")
	}
	if !snapshot.Authorization.Blocked && snapshot.Authorization.Reason != "" {
		return params, errors.New("allowed authorization cannot carry a block reason")
	}
	for _, policy := range snapshot.Policies {
		if seen[policy.Kind] {
			return params, fmt.Errorf("duplicate policy kind %q", policy.Kind)
		}
		seen[policy.Kind] = true
		switch policy.CompositionStatus {
		case "unbound", "enforced", "shadow_only", "invalid":
		default:
			return params, fmt.Errorf("invalid composition status for %q", policy.Kind)
		}
		if policy.CompositionStatus == "invalid" && !snapshot.Authorization.Blocked {
			return params, errors.New("invalid composition requires blocked authorization")
		}
		if policy.CompositionStatus != "enforced" {
			continue
		}
		rule := policy.EffectiveRule
		var err error
		switch policy.Kind {
		case "model_allowlist":
			if params.Models, err = stringList(rule, "allowed_models"); err != nil {
				return params, err
			}
			if params.DeniedModels, err = stringList(rule, "denied_models"); err != nil {
				return params, err
			}
		case "provider_allowlist":
			if params.AllowedProviders, err = stringList(rule, "allowed_providers"); err != nil {
				return params, err
			}
			if params.DeniedProviders, err = stringList(rule, "denied_providers"); err != nil {
				return params, err
			}
		case "rate_limit":
			value, err := decodeNumber(rule["requests_per_minute"], "requests_per_minute")
			if err != nil {
				return params, err
			}
			params.RateLimit = int(value)
		case "model_residency":
			value, ok := rule["require_eu"].(bool)
			if !ok {
				return params, errors.New("require_eu must be boolean")
			}
			params.RequireEUResidency = value
		case "spend_budget":
			value, err := decodeNumber(rule["max_budget"], "max_budget")
			if err != nil {
				return params, err
			}
			params.Budget = &value
		case "auto_recharge":
			params.AutoRechargeEnabled, _ = rule["enabled"].(bool)
			if value, ok := rule["threshold"]; ok {
				parsed, err := decodeNumber(value, "threshold")
				if err != nil {
					return params, err
				}
				params.RechargeThreshold = parsed
			}
			if value, ok := rule["amount"]; ok {
				parsed, err := decodeNumber(value, "amount")
				if err != nil {
					return params, err
				}
				params.RechargeAmount = parsed
			}
		case "model_routing":
			enabled, ok := rule["enabled"].(bool)
			if !ok {
				return params, errors.New("model_routing.enabled must be boolean")
			}
			params.RouteSettings.Enabled = &enabled
			if raw, ok := rule["levels"].([]any); ok {
				for _, item := range raw {
					level, ok := item.(map[string]any)
					if !ok {
						return params, errors.New("model_routing.levels entries must be objects")
					}
					name, nameOK := level["name"].(string)
					model, modelOK := level["model"].(string)
					if !nameOK || !modelOK {
						return params, errors.New("model_routing level requires name and model")
					}
					description, _ := level["description"].(string)
					params.RouteSettings.Levels = append(params.RouteSettings.Levels, store.RouteLevel{Name: name, Model: model, Description: description})
				}
			}
		case "feedback_collection":
			value, ok := rule["enabled"].(bool)
			if !ok {
				return params, errors.New("feedback_collection.enabled must be boolean")
			}
			params.RouteSettings.FeedbackEnabled = &value
		case "response_cache":
			value, ok := rule["enabled"].(bool)
			if !ok {
				return params, errors.New("response_cache.enabled must be boolean")
			}
			params.RouteSettings.CacheEnabled = &value
			for name, target := range map[string]**float64{"direct_threshold": &params.RouteSettings.CacheDirectThreshold, "reuse_threshold": &params.RouteSettings.CacheReuseThreshold, "tweak_threshold": &params.RouteSettings.CacheTweakThreshold} {
				if raw, ok := rule[name]; ok {
					parsed, err := decodeNumber(raw, name)
					if err != nil {
						return params, err
					}
					*target = &parsed
				}
			}
		case "context_compression":
			value, ok := rule["enabled"].(bool)
			if !ok {
				return params, errors.New("context_compression.enabled must be boolean")
			}
			params.RouteSettings.CompressionEnabled = &value
			for name, target := range map[string]**int{"threshold": &params.RouteSettings.CompressionThreshold, "token_budget": &params.RouteSettings.CompressionTokenBudget, "step_window": &params.RouteSettings.CompressionStepWindow} {
				if raw, ok := rule[name]; ok {
					parsed, err := decodeNumber(raw, name)
					if err != nil {
						return params, err
					}
					integer := int(parsed)
					*target = &integer
				}
			}
			if raw, ok := rule["append_only_state"]; ok {
				parsed, ok := raw.(bool)
				if !ok {
					return params, errors.New("append_only_state must be boolean")
				}
				params.RouteSettings.CompressionAppendOnlyState = &parsed
			}
		case "guardrail_set":
			required, err := stringList(rule, "required_guardrails")
			if err != nil {
				return params, err
			}
			nonDerogable := false
			for _, manifest := range policy.EnforcingPolicies {
				if value, ok := manifest["non_derogable"].(bool); ok && value {
					nonDerogable = true
				}
			}
			if len(required) > 0 {
				params.RouteSettings.GuardrailOverride = &store.GuardrailOverride{RequiredGuardrails: []string(required), NonDerogable: nonDerogable}
			}
		case "request_logging":
			if raw, ok := rule["retention_days"]; ok {
				parsed, err := decodeNumber(raw, "retention_days")
				if err != nil {
					return params, err
				}
				days := int(parsed)
				params.RouteSettings.LogRetentionDays = &days
			}
		}
	}
	if len(seen) != len(snapshotPolicyKinds) {
		return params, errors.New("snapshot must contain every policy kind exactly once")
	}
	for _, kind := range snapshotPolicyKinds {
		if !seen[kind] {
			return params, fmt.Errorf("snapshot missing policy kind %q", kind)
		}
	}
	var contextErr error
	params.ContextInstructions, params.ContextInstructionsDigest, contextErr = materializeContext(snapshot, materials)
	if contextErr != nil {
		return params, contextErr
	}
	return params, nil
}

// ApplyGovernanceSnapshot validates and atomically applies one immutable
// control-plane revision. It returns the persisted revision and digest as the
// ACK body consumed by the outbox worker.
func (h *Handler) ApplyGovernanceSnapshot(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request governanceApplyRequest
	if err := decoder.Decode(&request); err != nil || request.Key == "" || request.Revision < 1 || len(request.Snapshot) == 0 || request.Envelope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid governance apply request"})
		return
	}
	var canonicalValue any
	valueDecoder := json.NewDecoder(bytes.NewReader(request.Snapshot))
	valueDecoder.UseNumber()
	if err := valueDecoder.Decode(&canonicalValue); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid snapshot JSON"})
		return
	}
	digest, err := canonicaljson.SHA256(canonicalValue)
	if err != nil || digest != request.ContentSHA256 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "snapshot digest mismatch"})
		return
	}
	canonical, _ := canonicaljson.Marshal(canonicalValue)
	typedDecoder := json.NewDecoder(bytes.NewReader(request.Snapshot))
	typedDecoder.UseNumber()
	typedDecoder.DisallowUnknownFields()
	var snapshot runtimeSnapshot
	if err := typedDecoder.Decode(&snapshot); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid runtime snapshot schema"})
		return
	}
	if (snapshot.SchemaVersion != 5 && snapshot.SchemaVersion != runtimeSnapshotSchema) || snapshot.CompilerVersion != runtimeSnapshotCompiler || snapshot.Scope.AgentEnvironmentID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "unsupported runtime snapshot capability"})
		return
	}
	if h.snapshotVerifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "governance signature verification unavailable"})
		return
	}
	claims, err := h.snapshotVerifier.VerifySnapshot(r.Context(), request.Envelope)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid governance snapshot envelope"})
		return
	}
	if !snapshotClaimsMatch(claims, snapshot, request.Revision, digest) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "governance snapshot envelope does not match request"})
		return
	}
	key, err := h.resolveKeyIdentifier(r.Context(), request.Key)
	if err != nil {
		h.writeKeyResolutionError(w, "apply governance snapshot", err)
		return
	}
	params, err := compileKeyProjection(snapshot, request.ContextPacks, key.KeyHash, digest, string(canonical), request.Revision)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	applier, ok := h.store.(interface {
		ApplyGovernanceSnapshot(context.Context, store.GovernanceApplyParams) (store.GovernanceApplyResult, error)
	})
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "governance apply unavailable"})
		return
	}
	result, err := applier.ApplyGovernanceSnapshot(r.Context(), params)
	if errors.Is(err, store.ErrKeyNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "key not found"})
		return
	}
	if errors.Is(err, store.ErrStaleGovernanceRevision) || errors.Is(err, store.ErrGovernanceRevisionConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		h.logger.Error("admin: governance apply failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func snapshotClaimsMatch(claims *agenttoken.SnapshotClaims, snapshot runtimeSnapshot, revision int64, digest string) bool {
	environment := ""
	if snapshot.Scope.Environment != nil {
		environment = *snapshot.Scope.Environment
	}
	return claims.TenantID == snapshot.Scope.TenantID &&
		claims.AgentID == snapshot.Scope.AgentID &&
		claims.AgentEnvironmentID == snapshot.Scope.AgentEnvironmentID &&
		claims.Environment == environment && claims.Revision == revision &&
		claims.SchemaVersion == int64(snapshot.SchemaVersion) &&
		claims.CompilerVersion == int64(snapshot.CompilerVersion) &&
		claims.ContentSHA256 == digest
}
