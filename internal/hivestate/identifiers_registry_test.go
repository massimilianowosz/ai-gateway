package hivestate

import (
	"encoding/json"
	"strings"
	"testing"
)

func registryFor(t *testing.T, msgs []Message) string {
	t.Helper()
	hs := &HiveState{counter: NewTokenCounter(), logger: mwTestLogger()}
	return hs.buildRegistryMessage(msgs).Content
}

// Anthropic has no "tool" role — tool results arrive as user messages carrying
// a ToolCallID — so nothing ever reset the active file and every identifier in
// the conversation was attributed to whichever file was mentioned first.
func TestRegistry_AnthropicToolResultsResetTheActiveFile(t *testing.T) {
	// Each tool result names its own file. Without the reset, the second
	// result's identifiers were still attributed to the first file, because
	// Anthropic tool results carry role "user" and nothing matched.
	msgs := []Message{
		{Role: "assistant", Content: "Read(/Users/me/proj/internal/auth/auth.go)"},
		{Role: "user", ToolCallID: "toolu_1", Content: "// /Users/me/proj/internal/auth/auth.go\nfunc Authenticate(ctx context.Context) error { return nil }"},
		{Role: "assistant", Content: "ora la fatturazione"},
		{Role: "user", ToolCallID: "toolu_2", Content: "func ComputeBilling(x int) int { return x }"},
	}
	got := registryFor(t, msgs)
	if !strings.Contains(got, "auth.go") {
		t.Skipf("fixture produced no file section:\n%s", got)
	}

	section := got[strings.Index(got, "auth.go"):]
	if end := strings.Index(section, "\n\n"); end >= 0 {
		section = section[:end]
	}
	if strings.Contains(section, "ComputeBilling") {
		t.Fatalf("an identifier from a later tool result was attributed to auth.go:\n%s", got)
	}
}

// The registry is cached by the provider, so identical input has to render
// identical bytes. Unstable sorts over map iteration made ties — the common
// case — order differently on every run.
func TestRegistry_OutputIsDeterministic(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", Content: "Read(/Users/me/proj/src/alpha.go)"},
		{Role: "tool", Content: "func Alpha() {}\nfunc Beta() {}\nfunc Gamma() {}\nfunc Delta() {}\nfunc Epsilon() {}"},
		{Role: "assistant", Content: "Read(/Users/me/proj/src/beta.go)"},
		{Role: "tool", Content: "func Zeta() {}\nfunc Eta() {}"},
	}
	first := registryFor(t, msgs)
	if first == "" {
		t.Skip("no registry produced for this fixture")
	}
	for i := 0; i < 50; i++ {
		if got := registryFor(t, msgs); got != first {
			t.Fatalf("run %d differs:\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

// Two projects with the same relative layout shortened to the same string and
// rendered as two sections under one heading.
func TestRegistry_CollidingShortPathsStayDistinct(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", Content: "Read(/Users/me/proj-a/src/util.go)"},
		{Role: "tool", Content: "func AlphaHelper() {}"},
		{Role: "assistant", Content: "Read(/Users/me/proj-b/src/util.go)"},
		{Role: "tool", Content: "func BetaHelper() {}"},
	}
	got := registryFor(t, msgs)
	if got == "" {
		t.Skip("no registry produced for this fixture")
	}
	if strings.Count(got, "/src/util.go") > 1 {
		t.Fatalf("two files render under the same heading:\n%s", got)
	}
}

// isNoiseFile matched its patterns as substrings, so a project directory that
// merely contained "/var/" or "vendor/" was dropped entirely.
func TestRegistry_ProjectPathsAreNotMistakenForNoise(t *testing.T) {
	keep := []string{
		"/home/me/app/var/cache/Container.php",
		"/home/me/proj/myvendor/lib.go",
		"/home/me/notgit/main.go",
	}
	for _, p := range keep {
		if isNoiseFile(p) {
			t.Errorf("%s was treated as noise", p)
		}
	}
	drop := []string{
		"/var/log/syslog",
		"/home/me/proj/vendor/dep/lib.go",
		"/home/me/proj/.git/config",
		"/home/me/proj/node_modules/x/index.js",
	}
	for _, p := range drop {
		if !isNoiseFile(p) {
			t.Errorf("%s should have been treated as noise", p)
		}
	}
}

// The registry has to reach the provider. Appended only to Result.Messages it
// never did — the rewriters build the body from the Result's own fields — while
// still inflating ResultTokens and suppressing compressions worth making.
func TestRegistry_ReachesTheProviderBody(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[
		{"role":"user","content":"uno"},
		{"role":"assistant","content":"due"},
		{"role":"user","content":"tre"},
		{"role":"assistant","content":"quattro"},
		{"role":"user","content":"cinque"},
		{"role":"assistant","content":"sei"},
		{"role":"user","content":"sette"},
		{"role":"assistant","content":"otto"},
		{"role":"user","content":"nove"},
		{"role":"assistant","content":"dieci"},
		{"role":"user","content":"undici"}
	]}`)
	result := &Result{
		StateJSON:       `{"intent":"x"}`,
		RegistryMessage: Message{Role: "user", Content: "CODE REGISTRY: Alpha(), Beta()"},
	}

	out, err := rewriteAnthropicBodyWithWindow(body, result, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "CODE REGISTRY") {
		t.Fatalf("the registry never reached the request body:\n%s", out)
	}

	// And it sits right after the state summary, not at the tail.
	var parsed struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Messages) < 2 || !strings.Contains(string(parsed.Messages[1].Content), "CODE REGISTRY") {
		t.Errorf("the registry is not immediately after the state summary")
	}
}
