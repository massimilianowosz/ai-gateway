package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func newRouteTestApp(t *testing.T, handler http.HandlerFunc) (*App, *bytes.Buffer) {
	t.Helper()
	var client *Client
	if handler != nil {
		ts := httptest.NewServer(handler)
		t.Cleanup(ts.Close)
		client = NewClient(ts.URL, "sk-ubq-test")
	}
	var buf bytes.Buffer
	return NewApp(client, &buf, false), &buf
}

func writeGatewayConfig(t *testing.T, cfg initConfig) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	data, err := yaml.Marshal(&cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway.yaml"), data, 0644))
	return filepath.Join(dir, "gateway.yaml")
}

// --- routeCmd dispatch ---

func TestRouteCmd_DefaultsToStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeCmd(context.Background(), nil))
	assert.Contains(t, buf.String(), "not configured")
}

func TestRouteCmd_UnknownSubcommand(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.routeCmd(context.Background(), []string{"bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown route subcommand")
}

func TestRouteCmd_AddRequiresArgs(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.routeCmd(context.Background(), []string{"add", "trivial"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage: route add")
}

func TestRouteCmd_RemoveRequiresArgs(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.routeCmd(context.Background(), []string{"rm"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage: route rm")
}

// --- routeStatus ---

func TestRouteStatus_NoConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeStatus())
	assert.Contains(t, buf.String(), "not configured")
}

func TestRouteStatus_Disabled(t *testing.T) {
	writeGatewayConfig(t, initConfig{HiveState: &initHiveState{HiveRoute: &initHiveRoute{Enabled: false}}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeStatus())
	assert.Contains(t, buf.String(), "hiveroute disabled")
}

func TestRouteStatus_EnabledNoLevels(t *testing.T) {
	writeGatewayConfig(t, initConfig{HiveState: &initHiveState{HiveRoute: &initHiveRoute{Enabled: true}}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeStatus())
	assert.Contains(t, buf.String(), "no levels configured")
}

func TestRouteStatus_EnabledWithLevelsAndBudgets(t *testing.T) {
	writeGatewayConfig(t, initConfig{HiveState: &initHiveState{HiveRoute: &initHiveRoute{
		Enabled: true,
		Levels: []initHiveRouteLevel{
			{Name: "trivial", Model: "gpt-4o-mini", Description: "simple asks"},
		},
		ThinkingBudgets: map[string]int{"medium": 4000},
	}}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeStatus())
	out := buf.String()
	assert.Contains(t, out, "trivial")
	assert.Contains(t, out, "gpt-4o-mini")
	assert.Contains(t, out, "Thinking Budgets")
	assert.Contains(t, out, "4000 tokens")
}

func TestRouteStatus_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway.yaml"), []byte("not: valid: yaml: [["), 0644))
	app, _ := newRouteTestApp(t, nil)
	err := app.routeStatus()
	require.Error(t, err)
}

// --- routeToggle ---

func TestRouteToggle_NoConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	app, _ := newRouteTestApp(t, nil)
	err := app.routeToggle(true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run init first")
}

func TestRouteToggle_EnableCreatesHiveRouteBlock(t *testing.T) {
	path := writeGatewayConfig(t, initConfig{})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeToggle(true))
	assert.Contains(t, buf.String(), "hiveroute enabled")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg initConfig
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	require.NotNil(t, cfg.HiveState)
	require.NotNil(t, cfg.HiveState.HiveRoute)
	assert.True(t, cfg.HiveState.HiveRoute.Enabled)
}

func TestRouteToggle_Disable(t *testing.T) {
	writeGatewayConfig(t, initConfig{HiveState: &initHiveState{HiveRoute: &initHiveRoute{Enabled: true}}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeToggle(false))
	assert.Contains(t, buf.String(), "hiveroute disabled")
}

// --- routeAddLevel / routeRemoveLevel ---

func TestRouteAddLevel_NoConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	app, _ := newRouteTestApp(t, nil)
	err := app.routeAddLevel("trivial", "gpt-4o-mini", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run init first")
}

func TestRouteAddLevel_NewLevel(t *testing.T) {
	path := writeGatewayConfig(t, initConfig{})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeAddLevel("trivial", "gpt-4o-mini", "simple"))
	assert.Contains(t, buf.String(), "added level")

	data, _ := os.ReadFile(path)
	var cfg initConfig
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	require.Len(t, cfg.HiveState.HiveRoute.Levels, 1)
	assert.Equal(t, "gpt-4o-mini", cfg.HiveState.HiveRoute.Levels[0].Model)
}

func TestRouteAddLevel_UpdatesExistingLevelCaseInsensitive(t *testing.T) {
	path := writeGatewayConfig(t, initConfig{HiveState: &initHiveState{HiveRoute: &initHiveRoute{
		Enabled: true,
		Levels:  []initHiveRouteLevel{{Name: "Trivial", Model: "old-model"}},
	}}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeAddLevel("trivial", "new-model", ""))
	assert.Contains(t, buf.String(), "updated level")

	data, _ := os.ReadFile(path)
	var cfg initConfig
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	require.Len(t, cfg.HiveState.HiveRoute.Levels, 1)
	assert.Equal(t, "new-model", cfg.HiveState.HiveRoute.Levels[0].Model)
}

func TestRouteRemoveLevel_NotConfigured(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.routeRemoveLevel("trivial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestRouteRemoveLevel_NotFound(t *testing.T) {
	writeGatewayConfig(t, initConfig{HiveState: &initHiveState{HiveRoute: &initHiveRoute{
		Levels: []initHiveRouteLevel{{Name: "trivial", Model: "gpt-4o-mini"}},
	}}})
	app, _ := newRouteTestApp(t, nil)
	err := app.routeRemoveLevel("complex")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRouteRemoveLevel_Success(t *testing.T) {
	path := writeGatewayConfig(t, initConfig{HiveState: &initHiveState{HiveRoute: &initHiveRoute{
		Levels: []initHiveRouteLevel{
			{Name: "trivial", Model: "gpt-4o-mini"},
			{Name: "complex", Model: "gpt-4o"},
		},
	}}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.routeRemoveLevel("trivial"))
	assert.Contains(t, buf.String(), "removed level")

	data, _ := os.ReadFile(path)
	var cfg initConfig
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	require.Len(t, cfg.HiveState.HiveRoute.Levels, 1)
	assert.Equal(t, "complex", cfg.HiveState.HiveRoute.Levels[0].Name)
}

// --- routeMetrics ---

func TestRouteMetrics_NoRoutedRequests(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metrics": []StateMetricResponse{{Mode: "none"}},
		})
	})
	require.NoError(t, app.routeMetrics(context.Background(), 100))
	assert.Contains(t, buf.String(), "no route decisions recorded")
}

func TestRouteMetrics_AggregatesByLevelAndModel(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metrics": []StateMetricResponse{
				{RouteLevel: "trivial", RouteModel: "gpt-4o-mini", RouteEffort: "low", RouteSavedCost: 0.01},
				{RouteLevel: "trivial", RouteModel: "gpt-4o-mini", RouteEffort: "low", RouteSavedCost: 0.02},
				{RouteLevel: "complex", RouteModel: "gpt-4o", RouteEffort: "high", RouteSavedCost: 0},
				{Mode: "none"}, // not routed, should be excluded
			},
		})
	})
	require.NoError(t, app.routeMetrics(context.Background(), 100))
	out := buf.String()
	assert.Contains(t, out, "HiveRoute Metrics")
	assert.Contains(t, out, "Total routed:    3 / 4 requests")
	assert.Contains(t, out, "trivial")
	assert.Contains(t, out, "gpt-4o-mini")
	assert.Contains(t, out, "low:2")
}

func TestRouteMetrics_ClientError(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	err := app.routeMetrics(context.Background(), 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch metrics")
}

// --- routeTeamCmd dispatch ---

func TestRouteTeamCmd_NoArgs(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.routeTeamCmd(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage: route team")
}

func TestRouteTeamCmd_UnknownSubcommand(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.routeTeamCmd(context.Background(), []string{"team-1", "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown team route subcommand")
}

func TestRouteTeamCmd_AddRequiresArgs(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.routeTeamCmd(context.Background(), []string{"team-1", "add", "trivial"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage: route team")
}

func TestRouteTeamCmd_RemoveRequiresArgs(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.routeTeamCmd(context.Background(), []string{"team-1", "rm"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage: route team")
}

// --- routeTeamStatus ---

func TestRouteTeamStatus_InheritsGlobal(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/route/settings", r.URL.Path)
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
	})
	require.NoError(t, app.routeTeamStatus(context.Background(), "team-1"))
	out := buf.String()
	assert.Contains(t, out, "inherits global")
}

func TestRouteTeamStatus_ExplicitSettings(t *testing.T) {
	enabled := true
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{
			TeamID:  "team-1",
			Enabled: &enabled,
			Levels:  []RouteSettingsLevel{{Name: "trivial", Model: "gpt-4o-mini"}},
		})
	})
	require.NoError(t, app.routeTeamStatus(context.Background(), "team-1"))
	out := buf.String()
	assert.Contains(t, out, "yes")
	assert.Contains(t, out, "trivial")
}

func TestRouteTeamStatus_ExplicitlyDisabled(t *testing.T) {
	disabled := false
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1", Enabled: &disabled})
	})
	require.NoError(t, app.routeTeamStatus(context.Background(), "team-1"))
	assert.Contains(t, buf.String(), "no")
}

func TestRouteTeamStatus_ClientError(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"team not found"}`))
	})
	err := app.routeTeamStatus(context.Background(), "team-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch settings")
}

// --- routeTeamToggle ---

func TestRouteTeamToggle_Enable(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "team-1", body["team_id"])
		assert.Equal(t, true, body["enabled"])
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
	})
	require.NoError(t, app.routeTeamToggle(context.Background(), "team-1", true))
	assert.Contains(t, buf.String(), "enabled for team")
}

func TestRouteTeamToggle_Disable(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
	})
	require.NoError(t, app.routeTeamToggle(context.Background(), "team-1", false))
	assert.Contains(t, buf.String(), "disabled for team")
}

// --- routeTeamAddLevel / routeTeamRemoveLevel / routeTeamReset ---

func TestRouteTeamAddLevel_NewLevel(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
		case http.MethodPost:
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			levels, ok := body["levels"].([]any)
			require.True(t, ok)
			require.Len(t, levels, 1)
			_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
		}
	})
	require.NoError(t, app.routeTeamAddLevel(context.Background(), "team-1", "trivial", "gpt-4o-mini", "simple"))
	assert.Contains(t, buf.String(), "added level")
}

func TestRouteTeamAddLevel_UpdatesExisting(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(RouteSettingsResponse{
				TeamID: "team-1",
				Levels: []RouteSettingsLevel{{Name: "trivial", Model: "old"}},
			})
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
		}
	})
	require.NoError(t, app.routeTeamAddLevel(context.Background(), "team-1", "trivial", "new", ""))
	assert.Contains(t, buf.String(), "updated level")
}

func TestRouteTeamAddLevel_GetError(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := app.routeTeamAddLevel(context.Background(), "team-1", "trivial", "gpt-4o-mini", "")
	require.Error(t, err)
}

func TestRouteTeamRemoveLevel_Found(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(RouteSettingsResponse{
				TeamID: "team-1",
				Levels: []RouteSettingsLevel{{Name: "trivial", Model: "gpt-4o-mini"}},
			})
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
		}
	})
	require.NoError(t, app.routeTeamRemoveLevel(context.Background(), "team-1", "trivial"))
	assert.Contains(t, buf.String(), "removed level")
}

func TestRouteTeamRemoveLevel_NotFound(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
	})
	err := app.routeTeamRemoveLevel(context.Background(), "team-1", "trivial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRouteTeamReset(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "team-1", body["team_id"])
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
	})
	require.NoError(t, app.routeTeamReset(context.Background(), "team-1"))
	assert.Contains(t, buf.String(), "reset route config")
}

func TestRouteTeamReset_Error(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := app.routeTeamReset(context.Background(), "team-1")
	require.Error(t, err)
}
