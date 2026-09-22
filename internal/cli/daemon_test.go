package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Path helpers ---

func TestUbiquumHome_FromEnv(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", "/custom/home")
	assert.Equal(t, "/custom/home", UbiquumHome())
}

func TestUbiquumHome_Default(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".ubiquum"), UbiquumHome())
}

func TestDerivedPaths(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", "/custom/home")
	assert.Equal(t, "/custom/home/gateway.yaml", ConfigFilePath())
	assert.Equal(t, "/custom/home/gateway.pid", pidFilePath())
	assert.Equal(t, "/custom/home/gateway.log", logFilePath())
}

// --- PID file round trip ---

func TestPIDFile_WriteReadRemove(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	require.NoError(t, writePID(4242))

	pid, err := readPID()
	require.NoError(t, err)
	assert.Equal(t, 4242, pid)

	removePID()
	_, err = readPID()
	assert.Error(t, err)
}

func TestReadPID_Missing(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	_, err := readPID()
	assert.Error(t, err)
}

func TestReadPID_Invalid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway.pid"), []byte("not-a-pid"), 0644))
	_, err := readPID()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pid file")
}

// --- isProcessAlive ---

func TestIsProcessAlive_CurrentProcess(t *testing.T) {
	assert.True(t, isProcessAlive(os.Getpid()))
}

func TestIsProcessAlive_FinishedProcess(t *testing.T) {
	cmd := exec.Command("true")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", "exit 0")
	}
	require.NoError(t, cmd.Run())
	assert.False(t, isProcessAlive(cmd.Process.Pid))
}

// --- runOrchestratorCommand ---

func TestRunOrchestratorCommand_EnvUnset(t *testing.T) {
	t.Setenv("UBIQUUM_TEST_ORCH_CMD", "")
	app, buf := newRouteTestApp(t, nil)
	handled, err := app.runOrchestratorCommand(context.Background(), "UBIQUUM_TEST_ORCH_CMD", "start")
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Empty(t, buf.String())
}

func TestRunOrchestratorCommand_Success(t *testing.T) {
	t.Setenv("UBIQUUM_TEST_ORCH_CMD", "echo orchestrated")
	app, buf := newRouteTestApp(t, nil)
	handled, err := app.runOrchestratorCommand(context.Background(), "UBIQUUM_TEST_ORCH_CMD", "start")
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Contains(t, buf.String(), "orchestrated")
}

func TestRunOrchestratorCommand_Failure(t *testing.T) {
	t.Setenv("UBIQUUM_TEST_ORCH_CMD", "exit 7")
	app, _ := newRouteTestApp(t, nil)
	handled, err := app.runOrchestratorCommand(context.Background(), "UBIQUUM_TEST_ORCH_CMD", "start")
	assert.True(t, handled)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start command failed")
}

// --- stop(): safe because we only ever signal a subprocess we spawned ourselves ---

func TestStop_NoPIDFile(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	app, _ := newRouteTestApp(t, nil)
	err := app.stop(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pid file")
}

func TestStop_StalePID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	// A finished process's PID is guaranteed not alive.
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	require.NoError(t, writePID(cmd.Process.Pid))

	app, _ := newRouteTestApp(t, nil)
	err := app.stop(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale pid")

	_, err = readPID()
	assert.Error(t, err, "stale pid file should be cleaned up")
}

func TestStop_GracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)

	// Spawn a real, harmless long-lived process we fully control, so
	// sending it a real SIGTERM (what stop() does) is safe.
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	require.NoError(t, writePID(cmd.Process.Pid))
	// Reap concurrently: otherwise the process lingers as a zombie after
	// SIGTERM, which isProcessAlive still reports as alive, and stop()
	// would spuriously fall through to the force-kill path.
	go func() { _ = cmd.Wait() }()

	app, buf := newRouteTestApp(t, nil)
	err := app.stop(context.Background())
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "gateway stopped")

	_, err = readPID()
	assert.Error(t, err, "pid file should be removed after stop")
}

// --- printDaemonStatus ---

func TestPrintDaemonStatus_NotRunning(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	var buf strings.Builder
	printDaemonStatus(&buf)
	assert.Contains(t, buf.String(), "not running")
}

func TestPrintDaemonStatus_StalePidCleaned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	require.NoError(t, writePID(cmd.Process.Pid))

	var buf strings.Builder
	printDaemonStatus(&buf)
	assert.Contains(t, buf.String(), "stale pid cleaned")
}

func TestPrintDaemonStatus_Running(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	require.NoError(t, writePID(cmd.Process.Pid))
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	var buf strings.Builder
	printDaemonStatus(&buf)
	assert.Contains(t, buf.String(), "gateway running")
}

// --- logs() / logsLast / logsFollow / logsRemote ---

func TestLogsLast(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	logPath := filepath.Join(dir, "gateway.log")
	require.NoError(t, os.WriteFile(logPath, []byte("line1\nline2\nline3\n"), 0644))

	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.logsLast(logPath, 2))
	out := buf.String()
	assert.Contains(t, out, "line2")
	assert.Contains(t, out, "line3")
	assert.NotContains(t, out, "line1")
}

func TestLogsLast_MissingFile(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.logsLast("/no/such/file.log", 10)
	require.Error(t, err)
}

func TestLogsFollow_ReturnsImmediatelyOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gateway.log")
	require.NoError(t, os.WriteFile(logPath, []byte("existing\n"), 0644))

	app, _ := newRouteTestApp(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := app.logsFollow(ctx, logPath)
	require.NoError(t, err)
}

func TestLogsRemote_ReturnsImmediatelyOnCancelledContext(t *testing.T) {
	app, _ := newRouteTestApp(t, nil) // nil client is fine: never reached
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := app.logsRemote(ctx)
	require.NoError(t, err)
}

func TestLogsRemote_FetchesAndPrintsThenStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("line1\nline2\n"))
	})
	// Cancel shortly after the first (fast, local) round trip completes, so
	// the next loop iteration's ctx.Done() check — reached after the 2s
	// inter-poll sleep — ends the loop instead of a second fetch.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := app.logsRemote(ctx)
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "line1")
	assert.Contains(t, out, "line2")
}

func TestLogsRemote_FetchError(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := app.logsRemote(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch remote logs")
}

func TestLogs_FollowFalseUsesLogsLast(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway.log"), []byte("a\nb\n"), 0644))

	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.logs(context.Background(), []string{"-n", "1"}))
	assert.Contains(t, buf.String(), "b")
}

func TestLogs_UsesOrchestratorCommand(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	t.Setenv("UBIQUUM_LOGS_CMD", "echo docker-gateway-log")

	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.logs(context.Background(), nil))
	assert.Contains(t, buf.String(), "docker-gateway-log")
}

func TestLogs_InvalidNValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway.log"), []byte("a\n"), 0644))

	app, _ := newRouteTestApp(t, nil)
	err := app.logs(context.Background(), []string{"-n", "abc"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid -n value")
}

// --- printLogLine ---

func TestPrintLogLine_Empty(t *testing.T) {
	app, buf := newRouteTestApp(t, nil)
	app.printLogLine("")
	assert.Empty(t, buf.String())
}

func TestPrintLogLine_NonJSON(t *testing.T) {
	app, buf := newRouteTestApp(t, nil)
	app.printLogLine("plain text log line")
	assert.Contains(t, buf.String(), "plain text log line")
}

func TestPrintLogLine_RequestFormat(t *testing.T) {
	app, buf := newRouteTestApp(t, nil)
	entry := map[string]any{
		"time": "2026-01-01T10:00:00Z", "msg": "request",
		"method": "GET", "path": "/v1/models", "status": 200.0, "duration_ms": 12.0,
	}
	data, _ := json.Marshal(entry)
	app.printLogLine(string(data))
	out := buf.String()
	assert.Contains(t, out, "GET")
	assert.Contains(t, out, "/v1/models")
	assert.Contains(t, out, "12ms")
}

func TestPrintLogLine_ErrorLevelWithExtras(t *testing.T) {
	app, buf := newRouteTestApp(t, nil)
	entry := map[string]any{
		"time": "not-rfc3339", "level": "ERROR", "msg": "boom", "extra_field": "value",
	}
	data, _ := json.Marshal(entry)
	app.printLogLine(string(data))
	out := buf.String()
	assert.Contains(t, out, "boom")
	assert.Contains(t, out, "value")
}

func TestPrintLogLine_UnknownLevel(t *testing.T) {
	app, buf := newRouteTestApp(t, nil)
	entry := map[string]any{"msg": "hi", "level": "TRACE"}
	data, _ := json.Marshal(entry)
	app.printLogLine(string(data))
	assert.Contains(t, buf.String(), "hi")
}

// --- getFloat / fmtExtraValues ---

func TestGetFloat(t *testing.T) {
	m := map[string]any{"n": 3.5, "s": "not a number"}
	assert.Equal(t, 3.5, getFloat(m, "n"))
	assert.Equal(t, 0.0, getFloat(m, "s"))
	assert.Equal(t, 0.0, getFloat(m, "missing"))
}

func TestFmtExtraValues(t *testing.T) {
	entry := map[string]any{
		"time": "t", "level": "l", "msg": "m", "method": "GET", "path": "/x",
		"status": 200.0, "duration_ms": 1.0, "custom": "shown",
	}
	assert.Equal(t, "shown", fmtExtraValues(entry))
}

func TestFmtExtraValues_NoExtras(t *testing.T) {
	entry := map[string]any{"time": "t", "level": "l", "msg": "m"}
	assert.Equal(t, "", fmtExtraValues(entry))
}

// --- valueOrDefault ---

func TestValueOrDefault(t *testing.T) {
	assert.Equal(t, "x", valueOrDefault("x", "default"))
	assert.Equal(t, "default", valueOrDefault("", "default"))
}

// --- cacheCmd / cacheStatus / cacheSetField ---

func TestCacheCmd_DefaultsToStatus(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.cacheCmd(context.Background(), nil))
	assert.Contains(t, buf.String(), "not configured")
}

func TestCacheCmd_InitRequiresInteractive(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.cacheCmd(context.Background(), []string{"init"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "interactive mode")
}

func TestCacheCmd_UnknownSubcommand(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.cacheCmd(context.Background(), []string{"bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown cache subcommand")
}

func TestCacheCmd_SetRequiresArgs(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.cacheCmd(context.Background(), []string{"set", "backend"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage: cache set")
}

func TestCacheCmd_EnableDisable(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	require.NoError(t, app.cacheCmd(context.Background(), []string{"enable"}))
	require.NoError(t, app.cacheCmd(context.Background(), []string{"disable"}))
}

func TestCacheCmd_SetDispatches(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	require.NoError(t, app.cacheCmd(context.Background(), []string{"set", "backend", "redis"}))
}

func TestCacheCmd_FlushDispatches(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, app.cacheCmd(context.Background(), []string{"flush"}))
	assert.Contains(t, buf.String(), "cache flushed")
}

func TestCacheCmd_MetricsDispatchesWithLimit(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.RawQuery, "10")
		_ = json.NewEncoder(w).Encode(map[string]any{"metrics": []CacheMetricResponse{}})
	})
	require.NoError(t, app.cacheCmd(context.Background(), []string{"metrics", "10"}))
}

func TestCacheStatus_Disabled(t *testing.T) {
	writeGatewayConfig(t, initConfig{Cache: &initCache{Enabled: false}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.cacheStatus())
	assert.Contains(t, buf.String(), "cache disabled")
}

func TestCacheStatus_EnabledWithBackends(t *testing.T) {
	writeGatewayConfig(t, initConfig{Cache: &initCache{
		Enabled: true, Backend: "redis", EmbeddingModel: "text-embed", RedisURL: "redis://x",
	}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.cacheStatus())
	out := buf.String()
	assert.Contains(t, out, "redis")
	assert.Contains(t, out, "text-embed")
}

func TestCacheSetField_NoConfig(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	app, _ := newRouteTestApp(t, nil)
	err := app.cacheSetField("backend", "redis")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run init first")
}

func TestCacheSetField_EnabledBooleanVariants(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.cacheSetField("enabled", "yes"))
	out := buf.String()
	assert.Contains(t, out, "cache.")
	assert.Contains(t, out, "enabled")
	assert.Contains(t, out, "= yes")
}

func TestCacheSetField_InvalidBackend(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.cacheSetField("backend", "mongodb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown backend")
}

func TestCacheSetField_UnknownKey(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.cacheSetField("bogus", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown cache key")
}

func TestCacheSetField_AllFields(t *testing.T) {
	path := writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	require.NoError(t, app.cacheSetField("embedding_model", "m1"))
	require.NoError(t, app.cacheSetField("tweak_model", "m2"))
	require.NoError(t, app.cacheSetField("qdrant_url", "http://q"))

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "m1")
	assert.Contains(t, string(data), "m2")
	assert.Contains(t, string(data), "http://q")
}

// --- cacheFlush / cacheMetrics ---

func TestCacheFlush_Success(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, app.cacheFlush(context.Background()))
	assert.Contains(t, buf.String(), "cache flushed")
}

func TestCacheFlush_Error(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := app.cacheFlush(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flush failed")
}

func TestCacheMetrics_Empty(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"metrics": []CacheMetricResponse{}})
	})
	require.NoError(t, app.cacheMetrics(context.Background(), 50))
	assert.Contains(t, buf.String(), "no cache metrics")
}

func TestCacheMetrics_Aggregates(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metrics": []CacheMetricResponse{
				{Band: "DIRECT", Score: 0.98, TokensSaved: 100, LatencyMs: 5},
				{Band: "MISS", Score: 0.1, TokensSaved: 0, LatencyMs: 8},
				{Band: "CUSTOM", Score: 0.5, TokensSaved: 10, LatencyMs: 3},
			},
		})
	})
	require.NoError(t, app.cacheMetrics(context.Background(), 50))
	out := buf.String()
	assert.Contains(t, out, "Cache Metrics Summary")
	assert.Contains(t, out, "Hit rate")
	assert.Contains(t, out, "CUSTOM")
}

func TestCacheMetrics_Error(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := app.cacheMetrics(context.Background(), 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch metrics")
}

// --- stateCmd / stateStatus / stateSetField / stateMetrics ---

func TestStateCmd_DefaultsToStatus(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.stateCmd(context.Background(), nil))
	assert.Contains(t, buf.String(), "not configured")
}

func TestStateCmd_InitRequiresInteractive(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.stateCmd(context.Background(), []string{"init"})
	require.Error(t, err)
}

func TestStateCmd_UnknownSubcommand(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.stateCmd(context.Background(), []string{"bogus"})
	require.Error(t, err)
}

func TestStateCmd_EnableDisable(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	require.NoError(t, app.stateCmd(context.Background(), []string{"enable"}))
	require.NoError(t, app.stateCmd(context.Background(), []string{"disable"}))
}

func TestStateCmd_SetDispatches(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	require.NoError(t, app.stateCmd(context.Background(), []string{"set", "model", "gpt-4o-mini"}))
}

func TestStateCmd_MetricsDispatchesWithLimit(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.RawQuery, "10")
		_ = json.NewEncoder(w).Encode(map[string]any{"metrics": []StateMetricResponse{}})
	})
	require.NoError(t, app.stateCmd(context.Background(), []string{"metrics", "10"}))
}

func TestStateCmd_SetRequiresArgs(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.stateCmd(context.Background(), []string{"set", "model"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage: state set")
}

func TestStateStatus_EnabledWithFields(t *testing.T) {
	writeGatewayConfig(t, initConfig{HiveState: &initHiveState{
		Enabled: true, Model: "qwen2.5:3b", Threshold: 400, MaxLatencyMs: 5000,
	}})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.stateStatus())
	out := buf.String()
	assert.Contains(t, out, "qwen2.5:3b")
	assert.Contains(t, out, "400 tokens")
}

func TestStateSetField_ThresholdValidation(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.stateSetField("threshold", "not-a-number")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threshold must be")
}

func TestStateSetField_MaxLatencyValidation(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.stateSetField("max_latency_ms", "-1")
	require.Error(t, err)
}

func TestStateSetField_UnknownKey(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.stateSetField("bogus", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown hivestate key")
}

func TestStateSetField_Model(t *testing.T) {
	path := writeGatewayConfig(t, initConfig{})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.stateSetField("model", "gpt-4o-mini"))
	out := buf.String()
	assert.Contains(t, out, "hivestate.")
	assert.Contains(t, out, "model")
	assert.Contains(t, out, "gpt-4o-mini")

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "gpt-4o-mini")
}

func TestStateMetrics_Empty(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"metrics": []StateMetricResponse{}})
	})
	require.NoError(t, app.stateMetrics(context.Background(), 50))
	assert.Contains(t, buf.String(), "no state metrics")
}

func TestStateMetrics_AggregatesModesAndIntents(t *testing.T) {
	app, buf := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metrics": []StateMetricResponse{
				{Mode: "state", OriginalTokens: 1000, ResultTokens: 200, LatencyMs: 50, Intent: "billing_question"},
				{Mode: "state", OriginalTokens: 800, ResultTokens: 150, LatencyMs: 40, Intent: "billing_question", FallbackReason: ""},
				{Mode: "none"},
				{Mode: "state", FallbackReason: "timeout"},
			},
		})
	})
	require.NoError(t, app.stateMetrics(context.Background(), 50))
	out := buf.String()
	assert.Contains(t, out, "HiveState Metrics Summary")
	assert.Contains(t, out, "Reduction")
	assert.Contains(t, out, "billing_question")
	assert.Contains(t, out, "Fallbacks")
}

func TestStateMetrics_Error(t *testing.T) {
	app, _ := newRouteTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := app.stateMetrics(context.Background(), 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch state metrics")
}

// --- configCmd / configSet ---

func TestConfigCmd_ReadsGivenPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 4000\n"), 0644))

	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.configCmd(context.Background(), []string{path}))
	assert.Contains(t, buf.String(), "config loaded")
}

func TestConfigCmd_MissingFile(t *testing.T) {
	app, _ := newRouteTestApp(t, nil)
	err := app.configCmd(context.Background(), []string{"/no/such/config.yaml"})
	require.Error(t, err)
}

func TestConfigCmd_Set(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.configCmd(context.Background(), []string{"set", "port", "5000"}))
	out := buf.String()
	assert.Contains(t, out, "port")
	assert.Contains(t, out, "= 5000")
}

func TestConfigSet_NoConfig(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())
	app, _ := newRouteTestApp(t, nil)
	err := app.configSet("port", "5000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run init first")
}

func TestConfigSet_InvalidPort(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.configSet("port", "not-a-port")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid port")
}

func TestConfigSet_Strategy(t *testing.T) {
	path := writeGatewayConfig(t, initConfig{})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.configSet("strategy", "latency"))
	out := buf.String()
	assert.Contains(t, out, "strategy")
	assert.Contains(t, out, "= latency")
	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "latency")
}

func TestConfigSet_InvalidStrategy(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.configSet("strategy", "random")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown strategy")
}

func TestConfigSet_Retries(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, buf := newRouteTestApp(t, nil)
	require.NoError(t, app.configSet("retries", "5"))
	out := buf.String()
	assert.Contains(t, out, "retries")
	assert.Contains(t, out, "= 5")
}

func TestConfigSet_InvalidRetries(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.configSet("retries", "abc")
	require.Error(t, err)
}

func TestConfigSet_SwaggerVariants(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	require.NoError(t, app.configSet("swagger", "true"))
	require.NoError(t, app.configSet("swagger", "off"))
	err := app.configSet("swagger", "maybe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid swagger value")
}

func TestConfigSet_MasterKeyReloadsClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway.yaml"), []byte(""), 0644))

	client := NewClient("http://example.com", "old-key")
	var buf strings.Builder
	app := NewApp(client, &buf, false)
	require.NoError(t, app.configSet("master_key", "new-key"))
	assert.Contains(t, buf.String(), "master_key")
}

func TestConfigSet_UnknownKey(t *testing.T) {
	writeGatewayConfig(t, initConfig{})
	app, _ := newRouteTestApp(t, nil)
	err := app.configSet("bogus", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown setting")
}

// sanity: make sure time formatting in printLogLine doesn't panic on odd input.
func TestPrintLogLine_MalformedTimeFallsBackToRaw(t *testing.T) {
	app, buf := newRouteTestApp(t, nil)
	entry := map[string]any{"time": 12345, "msg": "weird time type"}
	data, _ := json.Marshal(entry)
	app.printLogLine(string(data))
	assert.Contains(t, buf.String(), "weird time type")
	_ = time.Now() // keep time import used if formatting branch is skipped
}
