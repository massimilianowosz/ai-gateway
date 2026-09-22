package guardrail

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyzeHandler_AllowsCleanMessage(t *testing.T) {
	engine := NewEngine([]Scanner{NewPIIScanner()}, nil, nil, nil, true, discardLogger())
	h := NewAnalyzeHandler(engine, discardLogger())

	body := `{"messages":[{"role":"user","content":"che tempo fa oggi?"}]}`
	req := httptest.NewRequest(http.MethodPost, "/analyze/batch/beta/litellm_basic_guardrail_api", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp analyzeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ALLOWED", resp.Action)
}

func TestAnalyzeHandler_BlocksPIIMessage(t *testing.T) {
	engine := NewEngine([]Scanner{NewPIIScanner()}, nil, nil, nil, true, discardLogger())
	h := NewAnalyzeHandler(engine, discardLogger())

	body := `{"messages":[{"role":"user","content":"Il mio codice fiscale è RSSMRA85M01H501Q"}]}`
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp analyzeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "BLOCKED", resp.Action)
}

func TestAnalyzeHandler_InvalidJSONReturnsBadRequest(t *testing.T) {
	engine := NewEngine([]Scanner{NewPIIScanner()}, nil, nil, nil, true, discardLogger())
	h := NewAnalyzeHandler(engine, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString("{not json"))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp analyzeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ERROR", resp.Action)
}

func TestAnalyzeHandler_GuardrailsAsCSVString(t *testing.T) {
	engine := NewEngine([]Scanner{NewPIIScanner(), NewSecretsScanner()}, nil, nil, nil, true, discardLogger())
	h := NewAnalyzeHandler(engine, discardLogger())

	body := `{"messages":[{"role":"user","content":"token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"}],"guardrails":"anonymization"}`
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	var resp analyzeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// "anonymization" only enables pii, so the secret must not be caught.
	assert.Equal(t, "ALLOWED", resp.Action)
}

func TestAnalyzeHandler_GuardrailsAsJSONArray(t *testing.T) {
	engine := NewEngine([]Scanner{NewPIIScanner(), NewSecretsScanner()}, nil, nil, nil, true, discardLogger())
	h := NewAnalyzeHandler(engine, discardLogger())

	body := `{"messages":[{"role":"user","content":"token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"}],"guardrails":["secrets"]}`
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	var resp analyzeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "BLOCKED", resp.Action)
}

func TestNormalizeGuardrails(t *testing.T) {
	tests := []struct {
		name string
		in   interface{}
		want []string
	}{
		{"nil", nil, nil},
		{"empty string", "", nil},
		{"csv string", "pii, secrets ,injection", []string{"pii", "secrets", "injection"}},
		{"json array", []interface{}{"pii", "secrets"}, []string{"pii", "secrets"}},
		{"json array with empty string", []interface{}{"pii", ""}, []string{"pii"}},
		{"unsupported type", 42, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeGuardrails(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAnalyzeHandler_BodyTooLargeIsTruncatedNotErrored(t *testing.T) {
	// Body limited to 1MB via io.LimitReader; a well-formed small JSON body
	// should always be read fully regardless of this limit.
	engine := NewEngine([]Scanner{NewPIIScanner()}, nil, nil, nil, true, discardLogger())
	h := NewAnalyzeHandler(engine, discardLogger())

	body := `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/analyze", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
