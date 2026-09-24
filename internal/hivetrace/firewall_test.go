package hivetrace

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func TestFirewall_DeniedMCPServerIsReported(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindMCPServer, Label: "Unapproved Miro", Pattern: "*miro*", Enabled: true},
	})

	found := fw.findings([]ToolInvocation{
		{Tool: "board_search_boards", Server: "claude_ai_Miro", Source: ToolSourceMCP},
	}, nil)

	require.Len(t, found, 1)
	assert.Equal(t, KindFirewall, found[0].Kind)
	assert.Equal(t, "Unapproved Miro", found[0].Type, "the finding is named after the rule, not the server")
	assert.Equal(t, OriginResponse, found[0].Origin, "the firewall only ever sees what the model decided to call")
	assert.Equal(t, 1, found[0].Occurrences)
}

func TestFirewall_DeniedNativeToolIsReported(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindTool, Label: "No shell", Pattern: "Bash", Enabled: true},
	})

	found := fw.findings([]ToolInvocation{
		{Tool: "Bash", Source: ToolSourceNative},
	}, nil)

	require.Len(t, found, 1)
	assert.Equal(t, "No shell", found[0].Type)
}

func TestFirewall_DeniedFileReadIsReported(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindFileRead, Label: "Secrets", Pattern: "**/.env", Enabled: true},
	})

	found := fw.findings(nil, []FileAccess{
		{Path: "/repo/.env", Operation: FileOpRead},
	})

	require.Len(t, found, 1)
	assert.Equal(t, "Secrets", found[0].Type)
}

// A search still discloses the file's contents, so it is checked against the
// read rules, not left uncovered because it is not literally a Read call.
func TestFirewall_FileSearchIsCheckedAgainstReadRules(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindFileRead, Label: "Secrets", Pattern: "**/.env", Enabled: true},
	})

	found := fw.findings(nil, []FileAccess{
		{Path: "/repo/.env", Operation: FileOpSearch},
	})

	require.Len(t, found, 1)
	assert.Equal(t, "Secrets", found[0].Type)
}

func TestFirewall_DeniedFileWriteIsReported(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindFileWrite, Label: "CI config", Pattern: "**/.github/workflows/*", Enabled: true},
	})

	found := fw.findings(nil, []FileAccess{
		{Path: "/repo/.github/workflows/deploy.yml", Operation: FileOpWrite},
	})

	require.Len(t, found, 1)
	assert.Equal(t, "CI config", found[0].Type)
}

// A write rule must not also catch a read of the same path, or an operator
// reading the finding would not know which direction actually happened.
func TestFirewall_ReadAndWriteRulesDoNotCrossOver(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindFileWrite, Label: "CI config", Pattern: "**/.github/workflows/*", Enabled: true},
	})

	found := fw.findings(nil, []FileAccess{
		{Path: "/repo/.github/workflows/deploy.yml", Operation: FileOpRead},
	})

	assert.Empty(t, found)
}

func TestFirewall_DisabledRuleIsIgnored(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindTool, Label: "No shell", Pattern: "Bash", Enabled: false},
	})

	found := fw.findings([]ToolInvocation{{Tool: "Bash", Source: ToolSourceNative}}, nil)
	assert.Empty(t, found)
}

func TestFirewall_RepeatedHitsAggregateOccurrences(t *testing.T) {
	fw := newFirewallEvaluator([]store.FirewallRule{
		{Kind: store.FirewallKindFileRead, Label: "Secrets", Pattern: "**/.env", Enabled: true},
	})

	found := fw.findings(nil, []FileAccess{
		{Path: "/repo/.env", Operation: FileOpRead},
		{Path: "/repo/.env", Operation: FileOpRead},
		{Path: "/repo/sub/.env", Operation: FileOpRead},
	})

	require.Len(t, found, 1)
	assert.Equal(t, 3, found[0].Occurrences)
}

func TestFirewall_NoRulesIsInert(t *testing.T) {
	fw := newFirewallEvaluator(nil)
	found := fw.findings(
		[]ToolInvocation{{Tool: "Bash", Source: ToolSourceNative}},
		[]FileAccess{{Path: "/repo/.env", Operation: FileOpRead}},
	)
	assert.Empty(t, found)
}

func TestFirewall_NilEvaluatorIsInert(t *testing.T) {
	var fw *firewallEvaluator
	found := fw.findings([]ToolInvocation{{Tool: "Bash", Source: ToolSourceNative}}, nil)
	assert.Empty(t, found)
}

// End to end: a rule seeded in the store before the recorder starts is loaded
// and reported on the very next real turn, through the same middleware path
// production traffic takes.
func TestFirewall_RuleIsEnforcedThroughTheRealPipeline(t *testing.T) {
	db, ts := openTestStore(t)
	gorm, ok := db.(*store.GormStore)
	require.True(t, ok)
	require.NoError(t, gorm.CreateFirewallRule(context.Background(), &store.FirewallRule{
		Kind: store.FirewallKindMCPServer, Label: "Unapproved Miro", Pattern: "*miro*", Enabled: true,
	}))

	body := `{"model":"claude-sonnet","messages":[{"role":"user","content":"search my miro boards"}]}`
	events := serve(t, ts, db, chatRequest(body), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[
			{"id":"c1","function":{"name":"mcp__claude_ai_Miro__board_search_boards","arguments":"{}"}}
		]}}]}`))
	})

	require.Len(t, events, 1)
	f, ok := findingFor(events[0].Findings, KindFirewall, "Unapproved Miro")
	require.True(t, ok, "a denied MCP server must be reported, got %+v", events[0].Findings)
	assert.Equal(t, OriginResponse, f.Origin)
}

