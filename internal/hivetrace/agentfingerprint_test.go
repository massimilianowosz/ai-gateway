package hivetrace

import "testing"

func TestFingerprintAgentFromUserAgent(t *testing.T) {
	cases := []struct {
		product string
		app     string
		want    string
		via     string
	}{
		{product: "claude-cli", want: "claude-code", via: AgentViaUserAgent},
		{product: "codex_cli_rs", want: "codex", via: AgentViaUserAgent},
		{product: "GitHubCopilotChat", want: "copilot", via: AgentViaUserAgent},
		{product: "Cursor", want: "cursor", via: AgentViaUserAgent},
		{product: "opencode", want: "opencode", via: AgentViaUserAgent},
		{product: "python-urllib", app: "cline", want: "cline", via: AgentViaUserAgent},

		// A library is not an agent. Naming it says the traffic is bespoke.
		{product: "python-urllib", want: "python-urllib", via: AgentViaLibrary},
		{product: "openai-python", want: "openai-python", via: AgentViaLibrary},
		{product: "curl", want: "curl", via: AgentViaLibrary},

		{product: "", want: AgentUnknown, via: ""},
		{product: "SomeThingNobodyHasHeardOf", want: AgentUnknown, via: ""},
	}
	for _, c := range cases {
		got, via := fingerprintAgent(c.product, c.app, nil, nil, "")
		if got != c.want || via != c.via {
			t.Errorf("fingerprintAgent(%q, %q) = (%q, %q), want (%q, %q)",
				c.product, c.app, got, via, c.want, c.via)
		}
	}
}

func TestFingerprintAgentFromDeclaredTools(t *testing.T) {
	// A bespoke script driving Claude Code's toolset still identifies itself
	// by the toolset, which is the case a User-Agent alone cannot cover.
	body := []byte(`{"model":"claude-sonnet-5","tools":[
		{"name":"Bash"},{"name":"Read"},{"name":"TodoWrite"}]}`)
	got, via := fingerprintAgent("python-urllib", "", declaredToolNames(body), nil, "")
	if got != "claude-code" || via != AgentViaTools {
		t.Fatalf("got (%q, %q), want (claude-code, tools)", got, via)
	}

	openAIStyle := []byte(`{"tools":[
		{"type":"function","function":{"name":"run_in_terminal"}},
		{"type":"function","function":{"name":"semantic_search"}}]}`)
	got, via = fingerprintAgent("node-fetch", "", declaredToolNames(openAIStyle), nil, "")
	if got != "copilot" || via != AgentViaTools {
		t.Fatalf("got (%q, %q), want (copilot, tools)", got, via)
	}
}

func TestFingerprintAgentIgnoresMCPTools(t *testing.T) {
	// An MCP server's tools belong to the server, and any agent can mount it,
	// so they must not be read as evidence of who is calling.
	called := []ToolInvocation{
		{Tool: "TodoWrite", Source: ToolSourceMCP, Server: "impostor"},
	}
	got, _ := fingerprintAgent("python-requests", "", nil, called, "")
	if got != "python-requests" {
		t.Fatalf("MCP tool named the agent: got %q", got)
	}
}

func TestFingerprintAgentPrefersStatedIdentity(t *testing.T) {
	// Tool vocabulary is an inference; a client naming itself is a fact, and
	// the fact wins.
	body := []byte(`{"tools":[{"name":"apply_patch"},{"name":"update_plan"}]}`)
	got, via := fingerprintAgent("claude-cli", "", declaredToolNames(body), nil, "")
	if got != "claude-code" || via != AgentViaUserAgent {
		t.Fatalf("got (%q, %q), want (claude-code, user-agent)", got, via)
	}
}

func TestDeclaredToolNamesUnwrapsMCP(t *testing.T) {
	body := []byte(`{"tools":[{"name":"mcp__github__create_issue"}]}`)
	got := declaredToolNames(body)
	if len(got) != 1 || got[0] != "create_issue" {
		t.Fatalf("got %v, want [create_issue]", got)
	}
}

func TestDeclaredToolNamesTolerantOfJunk(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(""), []byte("not json"), []byte(`{"tools":"nope"}`)} {
		if got := declaredToolNames(body); got != nil && len(got) != 0 {
			t.Errorf("declaredToolNames(%q) = %v, want none", body, got)
		}
	}
}

func TestBetterAgentEvidence(t *testing.T) {
	if !betterAgentEvidence(AgentViaUserAgent, AgentViaTools) {
		t.Error("a stated identity should beat an inferred one")
	}
	if !betterAgentEvidence(AgentViaTools, AgentViaLibrary) {
		t.Error("a toolset should beat a bare library name")
	}
	if betterAgentEvidence(AgentViaLibrary, AgentViaUserAgent) {
		t.Error("a library name should not displace a stated identity")
	}
	if betterAgentEvidence("", AgentViaLibrary) {
		t.Error("no evidence should not displace evidence")
	}
}

func TestFingerprintAgentFromCredential(t *testing.T) {
	// Codex omits its User-Agent on some turns, and runs no tool on others,
	// so the subscription it presented is the only thing left to go on.
	got, via := fingerprintAgent("", "", nil, nil, "chatgpt_codex")
	if got != "codex" || via != AgentViaCredential {
		t.Fatalf("got (%q, %q), want (codex, credential)", got, via)
	}

	// An Anthropic token says nothing about which client holds it, so it is
	// deliberately not mapped.
	if got, _ := fingerprintAgent("", "", nil, nil, "anthropic"); got != AgentUnknown {
		t.Fatalf("anthropic credential claimed %q", got)
	}

	// A stated identity still wins over the credential.
	got, via = fingerprintAgent("claude-cli", "", nil, nil, "chatgpt_codex")
	if got != "claude-code" || via != AgentViaUserAgent {
		t.Fatalf("got (%q, %q), want (claude-code, user-agent)", got, via)
	}
}
