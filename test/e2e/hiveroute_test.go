package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// routeConfig sends "trivial" work to a cheap model and leaves everything else
// on whatever the caller asked for.
func routeConfig(cheap string) config.HiveRouteConfig {
	return config.HiveRouteConfig{
		Enabled: true,
		Levels: []config.HiveRouteLevel{
			{Name: "trivial", Model: cheap, Description: "simple questions"},
		},
	}
}

// The extracted difficulty picks the model, and the provider is the one that
// says whether the routing actually happened.
func TestHiveRoute_RoutesToTheLevelModel(t *testing.T) {
	hs := config.HiveStateConfig{
		Enabled: true, Threshold: 1, StepWindow: 2,
		Model: "state-model", HiveRoute: routeConfig("cheap"),
	}
	g := newGatewayOpts(t, []string{"expensive", "cheap", "state-model"}, options{hiveState: &hs})
	g.scripts.set("state-model", &script{complete: textAnswer(
		`{"intent":"saluto","conversation_status":"in_progress","difficulty":"trivial"}`)})
	g.scripts.set("cheap", &script{complete: textAnswer("risposta economica")})
	g.scripts.set("expensive", &script{complete: textAnswer("risposta costosa")})

	key := g.mintKey(100, "expensive", "cheap")
	resp := g.post("/v1/chat/completions", key, agenticBody("expensive"))
	require.Equal(t, http.StatusOK, resp.code, resp.body)

	require.NotEmpty(t, resp.hdr.Get("x-hiveroute-model"), "the request was never routed")
	assert.Equal(t, "cheap", resp.hdr.Get("x-hiveroute-model"))
	assert.Equal(t, "trivial", resp.hdr.Get("x-hiveroute-level"))

	seen := g.scripts.lastRequest()
	require.NotNil(t, seen)
	assert.Equal(t, "cheap", seen.Model,
		"the header claimed a route the provider never saw")
}

// Routing must not send a caller to a model it may not use: the handler would
// then refuse a request that was valid as sent.
func TestHiveRoute_NeverRoutesOutsideTheWhitelist(t *testing.T) {
	hs := config.HiveStateConfig{
		Enabled: true, Threshold: 1, StepWindow: 2,
		Model: "state-model", HiveRoute: routeConfig("cheap"),
	}
	g := newGatewayOpts(t, []string{"expensive", "cheap", "state-model"}, options{hiveState: &hs})
	g.scripts.set("state-model", &script{complete: textAnswer(
		`{"intent":"saluto","conversation_status":"in_progress","difficulty":"trivial"}`)})
	g.scripts.set("expensive", &script{complete: textAnswer("risposta costosa")})

	// The key may use only the expensive model, which is what it asked for.
	key := g.mintKey(100, "expensive")
	resp := g.post("/v1/chat/completions", key, agenticBody("expensive"))

	require.Equal(t, http.StatusOK, resp.code,
		"a valid request was refused after being routed somewhere it may not go: %s", resp.body)
	assert.Empty(t, resp.hdr.Get("x-hiveroute-model"),
		"the request was routed to a model the caller may not use")
}

// The caller can turn routing off per request. Two places read the header —
// the middleware, which then skips extraction, and applyHiveRoute itself —
// and either alone is enough, so this pins the behaviour, not one guard.
func TestHiveRoute_OptOutHeaderIsHonoured(t *testing.T) {
	hs := config.HiveStateConfig{
		Enabled: true, Threshold: 1, StepWindow: 2,
		Model: "state-model", HiveRoute: routeConfig("cheap"),
	}
	g := newGatewayOpts(t, []string{"expensive", "cheap", "state-model"}, options{hiveState: &hs})
	g.scripts.set("state-model", &script{complete: textAnswer(
		`{"intent":"saluto","conversation_status":"in_progress","difficulty":"trivial"}`)})
	g.scripts.set("expensive", &script{complete: textAnswer("risposta costosa")})

	key := g.mintKey(100, "expensive", "cheap")
	req := g.request("POST", "/v1/chat/completions", key, "application/json", agenticBody("expensive"))
	req.Header.Set("x-ubiquum-route", "false")
	resp := g.send(req)

	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assert.Empty(t, resp.hdr.Get("x-hiveroute-model"), "routing ran despite the opt-out")
}

// A difficulty with no level configured leaves the model alone.
func TestHiveRoute_UnknownDifficultyDoesNotRoute(t *testing.T) {
	hs := config.HiveStateConfig{
		Enabled: true, Threshold: 1, StepWindow: 2,
		Model: "state-model", HiveRoute: routeConfig("cheap"),
	}
	g := newGatewayOpts(t, []string{"expensive", "cheap", "state-model"}, options{hiveState: &hs})
	g.scripts.set("state-model", &script{complete: textAnswer(
		`{"intent":"x","conversation_status":"in_progress","difficulty":"impossibile"}`)})
	g.scripts.set("expensive", &script{complete: textAnswer("risposta costosa")})

	key := g.mintKey(100, "expensive", "cheap")
	resp := g.post("/v1/chat/completions", key, agenticBody("expensive"))

	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assert.Empty(t, resp.hdr.Get("x-hiveroute-model"),
		"an unmapped difficulty routed somewhere")
}

// agenticBody is a conversation long enough for hivestate to consider.
func agenticBody(model string) string {
	msgs := []any{}
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": "domanda numero " + string(rune('a'+i)) + " con abbastanza testo da contare qualcosa"},
			map[string]any{"role": "assistant", "content": "risposta numero " + string(rune('a'+i)) + " altrettanto lunga per fare volume"},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "ciao"})
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs})
	return string(body)
}

// HiveRoute injects the extracted reasoning effort into the request it rewrites.
// The model it picks is not the one the caller asked for, so the caller cannot
// know to avoid a deployment that rejects the field rather than ignoring it —
// Ollama answers `"llama3.2:1b" does not support thinking` with a 400, which the
// router then retries into a 502.
func TestHiveRoute_InjectsReasoningEffortIntoTheRoutedModel(t *testing.T) {
	seen := routeAndCaptureUpstream(t, nil)
	require.NotNil(t, seen)
	assert.Equal(t, "cheap", seen.Model)
	assert.Equal(t, "low", seen.Extra["reasoning_effort"],
		"the routed request lost the effort HiveRoute decided on")
}

// drop_params is the escape hatch, and it has to reach a routed request too:
// the deployment that cannot take the field is chosen by HiveRoute, after the
// caller has stopped having a say.
func TestHiveRoute_DropParamsRemovesReasoningEffortFromTheRoutedModel(t *testing.T) {
	seen := routeAndCaptureUpstream(t, map[string][]string{"cheap": {"reasoning_effort"}})
	require.NotNil(t, seen)
	assert.Equal(t, "cheap", seen.Model, "the request was not routed, so nothing was proven")
	assert.NotContains(t, seen.Extra, "reasoning_effort",
		"a deployment that rejects reasoning_effort was sent it anyway")
}

// routeAndCaptureUpstream drives one request that HiveRoute sends to "cheap"
// with a "low" effort, and returns what the provider was actually handed.
func routeAndCaptureUpstream(t *testing.T, drop map[string][]string) *provider.CompletionRequest {
	t.Helper()
	hs := config.HiveStateConfig{
		Enabled: true, Threshold: 1, StepWindow: 2,
		Model: "state-model", HiveRoute: routeConfig("cheap"),
	}
	g := newGatewayOpts(t, []string{"expensive", "cheap", "state-model"},
		options{hiveState: &hs, dropParams: drop})
	g.scripts.set("state-model", &script{complete: textAnswer(
		`{"intent":"saluto","conversation_status":"in_progress","difficulty":"trivial","reasoning_effort":"low"}`)})
	g.scripts.set("cheap", &script{complete: textAnswer("risposta economica")})

	key := g.mintKey(100, "expensive", "cheap")
	resp := g.post("/v1/chat/completions", key, agenticBody("expensive"))
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	require.Equal(t, "cheap", resp.hdr.Get("x-hiveroute-model"), "the request was never routed")
	return g.scripts.lastRequest()
}
