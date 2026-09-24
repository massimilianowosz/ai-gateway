package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHiveTraceConfig_ApplyDefaults(t *testing.T) {
	var c HiveTraceConfig
	c.ApplyDefaults()

	assert.Equal(t, "sql", c.Backend)
	assert.InDelta(t, 1.0, c.SampleRate, 1e-9)
	assert.Equal(t, 256*1024, c.MaxBodyBytes)
	assert.Equal(t, 4096, c.MaxQueue)
	assert.Equal(t, 5*time.Second, c.FlushInterval)
	assert.Equal(t, time.Minute, c.AnalyzeInterval)
	assert.Equal(t, 30*24*time.Hour, c.Retention)
	assert.Equal(t, time.Hour, c.CleanupInterval)
	assert.True(t, c.ShouldRedact(), "redaction is on unless explicitly disabled")
}

// A negative duration reaches time.NewTicker, which panics at startup; the
// defaults have to correct it rather than pass it through.
func TestHiveTraceConfig_ApplyDefaultsRejectsNonPositiveDurations(t *testing.T) {
	c := HiveTraceConfig{
		FlushInterval:   -time.Second,
		AnalyzeInterval: -time.Second,
		Retention:       -time.Hour,
		CleanupInterval: -time.Hour,
		SampleRate:      -1,
		MaxBodyBytes:    -1,
		MaxQueue:        -1,
	}
	c.ApplyDefaults()

	assert.Positive(t, c.FlushInterval)
	assert.Positive(t, c.AnalyzeInterval)
	assert.Positive(t, c.Retention)
	assert.Positive(t, c.CleanupInterval)
	assert.Positive(t, c.SampleRate)
	assert.Positive(t, c.MaxBodyBytes)
	assert.Positive(t, c.MaxQueue)
}

func TestHiveTraceConfig_ApplyDefaultsKeepsExplicitValues(t *testing.T) {
	c := HiveTraceConfig{
		Backend:       "clickhouse",
		SampleRate:    0.25,
		MaxBodyBytes:  1024,
		FlushInterval: time.Second,
	}
	c.ApplyDefaults()

	assert.Equal(t, "clickhouse", c.Backend)
	assert.InDelta(t, 0.25, c.SampleRate, 1e-9)
	assert.Equal(t, 1024, c.MaxBodyBytes)
	assert.Equal(t, time.Second, c.FlushInterval)
}

func TestHiveTraceConfig_ShouldRedact(t *testing.T) {
	on, off := true, false
	assert.True(t, (&HiveTraceConfig{}).ShouldRedact())
	assert.True(t, (&HiveTraceConfig{Redact: &on}).ShouldRedact())
	assert.False(t, (&HiveTraceConfig{Redact: &off}).ShouldRedact())
}

func TestValidateHiveTrace_DisabledIsAlwaysValid(t *testing.T) {
	require.NoError(t, validateHiveTrace(&HiveTraceConfig{Backend: "nonsense"}))
}

func TestValidateHiveTrace_BackendSelection(t *testing.T) {
	require.NoError(t, validateHiveTrace(&HiveTraceConfig{Enabled: true, Backend: "sql"}))

	err := validateHiveTrace(&HiveTraceConfig{Enabled: true, Backend: "mongo"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hivetrace.backend")

	err = validateHiveTrace(&HiveTraceConfig{Enabled: true, Backend: "clickhouse"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clickhouse_url")

	require.NoError(t, validateHiveTrace(&HiveTraceConfig{
		Enabled: true, Backend: "clickhouse", ClickHouseURL: "http://clickhouse:8123/ubiquum",
	}))
}

// Storing prompts unredacted is the one setting that can turn the trace store
// into a secret store, so it takes two deliberate keys.
func TestValidateHiveTrace_UnredactedBodiesNeedASecondOptIn(t *testing.T) {
	off := false

	err := validateHiveTrace(&HiveTraceConfig{
		Enabled: true, Backend: "sql", CaptureBodies: true, Redact: &off,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_unredacted")

	require.NoError(t, validateHiveTrace(&HiveTraceConfig{
		Enabled: true, Backend: "sql", CaptureBodies: true, Redact: &off, AllowUnredacted: true,
	}))

	require.NoError(t, validateHiveTrace(&HiveTraceConfig{
		Enabled: true, Backend: "sql", CaptureBodies: true,
	}), "redaction on by default needs no extra opt-in")
}

func TestLoad_HiveTraceIsOffByDefault(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	assert.False(t, cfg.HiveTrace.Enabled)
	assert.Equal(t, "sql", cfg.HiveTrace.Backend, "defaults still apply to the disabled section")
}
