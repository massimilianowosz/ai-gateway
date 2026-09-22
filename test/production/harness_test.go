// Package production drives the deployed gateway over its public API.
//
// These tests talk to a real deployment and spend real money, so they never
// run by accident: without UBIQUUM_MASTER_KEY every test skips. Point them
// at an environment with UBIQUUM_PROD_URL.
//
//	UBIQUUM_MASTER_KEY=... go test ./test/production -v
//
// What they are for is the half that unit and local end-to-end tests cannot
// reach: that the thing actually running enforces what the code says, with the
// config it was deployed with, against the providers it really calls.
//
// Every test provisions its own API keys through the admin API and deletes
// them afterwards, so a run leaves nothing behind and does not depend on how
// any particular tenant happens to be configured today.
package production

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	defaultBaseURL = "https://api.ubiquum.ai"
	// A cheap model that is always in the deployed config. Overridable for an
	// environment that carries a different catalogue.
	defaultModel = "gpt-oss:20b"
)

func baseURL() string {
	if v := strings.TrimRight(os.Getenv("UBIQUUM_PROD_URL"), "/"); v != "" {
		return v
	}
	return defaultBaseURL
}

func model() string {
	if v := os.Getenv("UBIQUUM_PROD_MODEL"); v != "" {
		return v
	}
	return defaultModel
}

// masterKey returns the admin credential, or skips: a missing key means the
// caller did not intend to run against a deployment.
func masterKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("UBIQUUM_MASTER_KEY")
	if key == "" {
		t.Skip("UBIQUUM_MASTER_KEY unset: skipping the production suite")
	}
	return key
}

// client is one caller identity against the deployment.
type client struct {
	base string
	key  string
	name string
}

func masterClient(t *testing.T) client {
	return client{base: baseURL(), key: masterKey(t), name: "master"}
}

type response struct {
	status  int
	headers http.Header
	body    []byte
}

// json decodes the body, failing the test when it is not the shape expected.
func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("risposta non JSON (HTTP %d): %s", r.status, truncate(string(r.body), 300))
	}
	return out
}

// content returns the assistant text of a chat completion.
func (r response) content(t *testing.T) string {
	t.Helper()
	parsed := r.json(t)
	choices, ok := parsed["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("nessuna choice nella risposta: %s", truncate(string(r.body), 300))
	}
	first, _ := choices[0].(map[string]any)
	message, _ := first["message"].(map[string]any)
	text, _ := message["content"].(string)
	return text
}

// errorMessage returns the error message of a refusal.
func (r response) errorMessage(t *testing.T) string {
	t.Helper()
	parsed := r.json(t)
	wrapper, ok := parsed["error"].(map[string]any)
	if !ok {
		return ""
	}
	message, _ := wrapper["message"].(string)
	return message
}

var httpClient = &http.Client{Timeout: 180 * time.Second}

func (c client) post(t *testing.T, path string, payload any, headers map[string]string) response {
	t.Helper()
	return c.request(t, http.MethodPost, path, payload, headers)
}

func (c client) get(t *testing.T, path string, headers map[string]string) response {
	t.Helper()
	return c.request(t, http.MethodGet, path, nil, headers)
}

func (c client) request(t *testing.T, method, path string, payload any, headers map[string]string) response {
	t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("payload non serializzabile: %v", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		t.Fatalf("richiesta non costruibile: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	// Nothing in this suite wants a cached answer: a hit would prove the cache,
	// not the behaviour under test. The cache test turns this back on itself.
	req.Header.Set("x-ubiquum-cache", "false")
	for name, value := range headers {
		if value == "" {
			req.Header.Del(name)
			continue
		}
		req.Header.Set(name, value)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s non ha risposto: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("corpo della risposta illeggibile: %v", err)
	}
	return response{status: resp.StatusCode, headers: resp.Header, body: raw}
}

// chat sends one user turn and returns the whole response.
func (c client) chat(t *testing.T, prompt string, headers map[string]string) response {
	t.Helper()
	return c.chatMessages(t, []map[string]any{{"role": "user", "content": prompt}}, headers)
}

func (c client) chatMessages(t *testing.T, messages []map[string]any, headers map[string]string) response {
	t.Helper()
	return c.post(t, "/v1/chat/completions", map[string]any{
		"model":    model(),
		"messages": messages,
		// Generous on purpose: the default model reasons before it answers, and
		// a tight ceiling is spent on reasoning tokens, returning empty content
		// with finish_reason "length" — which reads like a gateway fault and is
		// not one.
		"max_tokens":  600,
		"temperature": 0,
	}, headers)
}

// ephemeralKey provisions an API key for one test and deletes it afterwards.
// Tests that need two distinct identities — the only way to prove isolation —
// call this twice.
//
// teamID is left out unless given: api_keys_local carries a foreign key to
// tenants, so a made-up team is rejected. Team-less keys fall back to the
// gateway's own guardrail configuration, which is what these tests want
// anyway — they drive the policies explicitly per request.
func ephemeralKey(t *testing.T, admin client, name, teamID string, budget float64) client {
	t.Helper()

	payload := map[string]any{"name": name, "budget": budget}
	if teamID != "" {
		payload["team_id"] = teamID
	}
	created := admin.post(t, "/v1/key/generate", payload, nil)
	if created.status != http.StatusOK && created.status != http.StatusCreated {
		t.Fatalf("creazione chiave %q fallita (HTTP %d): %s", name, created.status, truncate(string(created.body), 300))
	}

	parsed := created.json(t)
	raw, _ := parsed["key"].(string)
	if raw == "" {
		t.Fatalf("la chiave creata non contiene il valore: %s", truncate(string(created.body), 300))
	}

	t.Cleanup(func() {
		deleted := admin.post(t, "/v1/key/delete", map[string]any{"key": raw}, nil)
		if deleted.status != http.StatusOK {
			t.Logf("attenzione: la chiave effimera %q non è stata cancellata (HTTP %d)", name, deleted.status)
		}
	})

	return client{base: admin.base, key: raw, name: name}
}

// uniqueSuffix keeps concurrent or repeated runs from colliding on names,
// team ids and session ids.
func uniqueSuffix() string {
	return fmt.Sprintf("%d%04d", time.Now().UnixNano()%1e9, rand.Intn(10000))
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// requireStatus fails with the body attached, which is what one actually needs
// when a production assertion breaks.
func requireStatus(t *testing.T, r response, want int, what string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("%s: HTTP %d, atteso %d — %s", what, r.status, want, truncate(string(r.body), 300))
	}
}
