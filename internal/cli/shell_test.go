package cli

import (
	"bufio"
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitCommandLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "slash command with flags",
			in:   `/tenant create acme budget 100 models "gpt-4o,claude-sonnet"`,
			want: []string{"tenant", "create", "acme", "budget", "100", "models", "gpt-4o,claude-sonnet"},
		},
		{
			name: "quoted prompt",
			in:   `/test gpt-4o "say hello world"`,
			want: []string{"test", "gpt-4o", "say hello world"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := shellArgs(tt.in)
			if err != nil {
				t.Fatalf("shellArgs() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("shellArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestExtractConnectionArgsKeepsSpendKeyFilter(t *testing.T) {
	got, baseURL, apiKey, err := extractConnectionArgs([]string{
		"spend",
		"--key", "sk-ubq-virtual",
		"--admin-key", "sk-master",
		"--url", "http://localhost:4000",
	})
	if err != nil {
		t.Fatalf("extractConnectionArgs() error = %v", err)
	}

	want := []string{"spend", "--key", "sk-ubq-virtual"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	if baseURL != "http://localhost:4000" {
		t.Fatalf("baseURL = %q", baseURL)
	}
	if apiKey != "sk-master" {
		t.Fatalf("apiKey = %q", apiKey)
	}
}

func TestShellInitReloadsMasterKey(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", t.TempDir())

	client := NewClient("", "")
	shell := NewShell(client, "test", strings.NewReader(""), io.Discard)
	scanner := bufio.NewScanner(strings.NewReader("\nsk-master\n\n"))
	reader := &scanReader{sc: scanner, out: io.Discard}

	shell.handleCommand(context.Background(), "init", reader)

	if client.apiKey != "sk-master" {
		t.Fatalf("client apiKey = %q, want %q", client.apiKey, "sk-master")
	}
}

func TestSuggestionSuffix(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "/sta", want: "tus"},
		{in: "mod", want: "el"},
		{in: "models r", want: "m"},
		{in: "key a", want: "dd"},
		{in: "status ", want: ""},
	}

	for _, tt := range tests {
		if got := suggestionSuffix(tt.in, nil); got != tt.want {
			t.Fatalf("suggestionSuffix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestKeyIdentifiersUsePrefixesAndOnlyUniqueNames(t *testing.T) {
	source := &completionSource{
		fetched: time.Now(),
		keys: []APIKey{
			{KeyPrefix: "sk-ubq-1111", Name: "duplicate"},
			{KeyPrefix: "sk-ubq-2222", Name: "duplicate"},
			{KeyPrefix: "sk-ubq-3333", Name: "unique"},
		},
	}

	got := keyIdentifiers(source)
	want := []string{"sk-ubq-1111", "sk-ubq-2222", "unique"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keyIdentifiers() = %#v, want %#v", got, want)
	}
}

func TestMaskSecret(t *testing.T) {
	got := maskSecret("sk-ubq-1234567890abcdef")
	if strings.Contains(got, "567890ab") {
		t.Fatalf("maskSecret leaked middle of key: %q", got)
	}
	if !strings.HasPrefix(got, "sk-ubq-1234") || !strings.HasSuffix(got, "cdef") {
		t.Fatalf("maskSecret = %q", got)
	}
}

func TestResolveKeyReference(t *testing.T) {
	keys := []APIKey{
		{ID: "1", KeyPrefix: "sk-ubq-1111", Name: "duplicate"},
		{ID: "2", KeyPrefix: "sk-ubq-2222", Name: "duplicate"},
		{ID: "3", KeyPrefix: "sk-ubq-3333", Name: "unique"},
	}

	got, found, err := resolveKeyReference(keys, "unique")
	if err != nil || !found || got.KeyPrefix != "sk-ubq-3333" {
		t.Fatalf("resolve unique = (%+v, %t, %v)", got, found, err)
	}

	got, found, err = resolveKeyReference(keys, "sk-ubq-1111")
	if err != nil || !found || got.ID != "1" {
		t.Fatalf("resolve prefix = (%+v, %t, %v)", got, found, err)
	}

	got, found, err = resolveKeyReference(keys, "duplicate")
	if err == nil || found {
		t.Fatalf("resolve duplicate = (%+v, %t, %v), want ambiguity", got, found, err)
	}
}

func TestCanonicalCommandKeepsLegacyAliases(t *testing.T) {
	tests := map[string]string{
		"key":     "keys",
		"keys":    "keys",
		"model":   "models",
		"models":  "models",
		"tenant":  "teams",
		"tenants": "teams",
		"status":  "status",
	}

	for in, want := range tests {
		if got := canonicalCommand(in); got != want {
			t.Fatalf("canonicalCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseModelSetArgsSupportsPriceAndAliases(t *testing.T) {
	name, updates, err := parseModelSetArgs([]string{
		"gpt-4o",
		"price", "2.5", "10",
		"provider-model", "gpt-4o-2024-11-20",
		"drop-params", "temperature,top_p",
	})
	if err != nil {
		t.Fatalf("parseModelSetArgs() error = %v", err)
	}
	if name != "gpt-4o" {
		t.Fatalf("name = %q", name)
	}
	want := []modelUpdate{
		{Field: "input_cost_per_million", Value: "2.5"},
		{Field: "output_cost_per_million", Value: "10"},
		{Field: "provider_model", Value: "gpt-4o-2024-11-20"},
		{Field: "drop_params", Value: "temperature,top_p"},
	}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("updates = %#v, want %#v", updates, want)
	}
}

func TestModelConfigMutationsPreserveOtherFields(t *testing.T) {
	path := t.TempDir() + "/gateway.yaml"
	input := `server:
  port: 4000
  master_key: sk-master
  max_concurrent: 42
database:
  driver: sqlite
  url: ubiquum.db
router:
  strategy: shuffle
  retries: 3
  retry_delay: 5s
models:
  - name: gpt-4o
    provider: openai
    provider_model: gpt-4o
    api_key: ${OPENAI_API_KEY}
    drop_params:
      - temperature
    input_cost_per_token: 0.000001
`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	total, err := appendModelToConfig(path, initModel{
		Name:          "claude",
		Provider:      "anthropic",
		ProviderModel: "claude-sonnet-4-20250514",
		APIKey:        "${ANTHROPIC_API_KEY}",
	})
	if err != nil {
		t.Fatalf("appendModelToConfig() error = %v", err)
	}
	if total != 2 {
		t.Fatalf("appendModelToConfig() total = %d, want 2", total)
	}

	remaining, err := removeModelFromConfig(path, "gpt-4o")
	if err != nil {
		t.Fatalf("removeModelFromConfig() error = %v", err)
	}
	if remaining != 1 {
		t.Fatalf("removeModelFromConfig() remaining = %d, want 1", remaining)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"database:", "max_concurrent: 42", "retry_delay: 5s", "claude", "anthropic"} {
		if !strings.Contains(got, want) {
			t.Fatalf("mutated config lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "gpt-4o") || strings.Contains(got, "drop_params:") {
		t.Fatalf("removed model fields still present:\n%s", got)
	}
}

func TestProviderAndModelConfigMutations(t *testing.T) {
	path := t.TempDir() + "/gateway.yaml"
	input := `server:
  port: 4000
  master_key: sk-master
router:
  strategy: shuffle
  retries: 3
`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	totalProviders, err := upsertProviderToConfig(path, "openai", initProvider{
		Type:   "openai",
		APIKey: "${OPENAI_API_KEY}",
	})
	if err != nil {
		t.Fatalf("upsertProviderToConfig() error = %v", err)
	}
	if totalProviders != 1 {
		t.Fatalf("upsertProviderToConfig() total = %d, want 1", totalProviders)
	}

	totalModels, err := appendModelToConfig(path, initModel{
		Name:          "gpt-4o",
		Provider:      "openai",
		ProviderModel: "gpt-4o",
	})
	if err != nil {
		t.Fatalf("appendModelToConfig() error = %v", err)
	}
	if totalModels != 1 {
		t.Fatalf("appendModelToConfig() total = %d, want 1", totalModels)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"providers:", "openai:", "type: openai", "api_key: ${OPENAI_API_KEY}", "provider: openai", "provider_model: gpt-4o"} {
		if !strings.Contains(got, want) {
			t.Fatalf("mutated config missing %q:\n%s", want, got)
		}
	}
}

func TestUpdateModelsInConfigUpdatesMatchingDeployments(t *testing.T) {
	path := t.TempDir() + "/gateway.yaml"
	input := `server:
  port: 4000
  master_key: sk-master
router:
  strategy: shuffle
  retries: 3
models:
  - name: gpt-4o
    provider: openai
    provider_model: gpt-4o
    input_cost_per_token: 0.000001
  - name: gpt-4o
    provider: azure
    provider_model: gpt-4o-2024-11-20
  - name: claude
    provider: anthropic
    provider_model: claude-sonnet-4-20250514
`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, err := updateModelsInConfig(path, "gpt-4o", []modelUpdate{
		{Field: "provider_model", Value: "gpt-4o-updated"},
		{Field: "drop_params", Value: "temperature,top_p"},
		{Field: "input_cost_per_million", Value: "2.5"},
		{Field: "output_cost_per_million", Value: "10"},
	})
	if err != nil {
		t.Fatalf("updateModelsInConfig() error = %v", err)
	}
	if updated != 2 {
		t.Fatalf("updateModelsInConfig() updated = %d, want 2", updated)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"claude-sonnet-4-20250514", "drop_params:", "- temperature", "- top_p"} {
		if !strings.Contains(got, want) {
			t.Fatalf("updated config missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "provider_model: gpt-4o-updated") != 2 {
		t.Fatalf("provider_model was not updated on both deployments:\n%s", got)
	}
	if strings.Count(got, "input_cost_per_million: 2.5") != 2 || strings.Count(got, "output_cost_per_million: 10.0") != 2 {
		t.Fatalf("price fields were not updated on both deployments:\n%s", got)
	}
	if strings.Contains(got, "input_cost_per_token") {
		t.Fatalf("legacy price field still present:\n%s", got)
	}

	updated, err = updateModelsInConfig(path, "gpt-4o", []modelUpdate{
		{Field: "input_cost_per_million", Value: "0.00"},
		{Field: "output_cost_per_million", Value: "0"},
	})
	if err != nil {
		t.Fatalf("clear updateModelsInConfig() error = %v", err)
	}
	if updated != 2 {
		t.Fatalf("clear updateModelsInConfig() updated = %d, want 2", updated)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got = string(data)
	if strings.Contains(got, "input_cost_per_million") || strings.Contains(got, "output_cost_per_million") {
		t.Fatalf("zero price should clear override fields:\n%s", got)
	}
}

func TestUnsetModelFieldsInConfigClearsPriceAliases(t *testing.T) {
	path := t.TempDir() + "/gateway.yaml"
	input := `server:
  port: 4000
  master_key: sk-master
router:
  strategy: shuffle
  retries: 3
models:
  - name: gpt-4o
    provider: openai
    provider_model: gpt-4o
    drop_params:
      - temperature
    input_cost_per_million: 2.5
    output_cost_per_million: 10
    input_cost_per_token: 0.000001
    output_cost_per_token: 0.000002
`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, err := unsetModelFieldsInConfig(path, "gpt-4o", []string{"price", "drop_params"})
	if err != nil {
		t.Fatalf("unsetModelFieldsInConfig() error = %v", err)
	}
	if updated != 1 {
		t.Fatalf("unsetModelFieldsInConfig() updated = %d, want 1", updated)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, removed := range []string{"drop_params", "input_cost_per_million", "output_cost_per_million", "input_cost_per_token", "output_cost_per_token"} {
		if strings.Contains(got, removed) {
			t.Fatalf("field %q still present:\n%s", removed, got)
		}
	}
	if !strings.Contains(got, "provider_model: gpt-4o") {
		t.Fatalf("unrelated fields were lost:\n%s", got)
	}
}
