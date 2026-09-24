package hivetrace

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellCommandsFromCodexJavaScript(t *testing.T) {
	// Codex passes JavaScript, not JSON: the command is a string literal
	// inside a call to its own exec helper.
	args := `const r = await tools.exec_command({cmd:"wc -l docs/hivestate-paper.md",` +
		`"workdir":"/Users/me/code","yield_time_ms":10000}); text(r.output);`
	got := shellCommands(args)
	if len(got) != 1 || got[0] != "wc -l docs/hivestate-paper.md" {
		t.Fatalf("got %q", got)
	}
}

func TestShellCommandsFromJSONArgument(t *testing.T) {
	got := shellCommands(`{"command":"cat internal/app.go","description":"read"}`)
	if len(got) != 1 || got[0] != "cat internal/app.go" {
		t.Fatalf("got %q", got)
	}
}

func TestShellFileAccessReads(t *testing.T) {
	got := shellFileAccess("wc -l docs/hivestate-paper.md")
	if len(got) != 1 || got[0].Path != "docs/hivestate-paper.md" || got[0].Operation != FileOpRead {
		t.Fatalf("got %+v", got)
	}
}

func TestShellFileAccessSkipsSearchPattern(t *testing.T) {
	// The first operand of grep is the regex. Counting it as a file used to
	// fill the list with patterns.
	got := shellFileAccess(`grep -rn "func main" cmd/gateway/main.go`)
	if len(got) != 1 || got[0].Path != "cmd/gateway/main.go" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Operation != FileOpSearch {
		t.Errorf("operation = %q, want search", got[0].Operation)
	}
}

func TestShellFileAccessSplitsPipeline(t *testing.T) {
	got := shellFileAccess("cat a/one.go | grep foo b/two.go")
	paths := map[string]bool{}
	for _, f := range got {
		paths[f.Path] = true
	}
	if !paths["a/one.go"] || !paths["b/two.go"] {
		t.Fatalf("got %+v", got)
	}
}

func TestShellFileAccessIgnoresUnknownCommands(t *testing.T) {
	// Deciding whether an arbitrary command writes a file is not something a
	// heuristic should claim to know, so it reports nothing.
	for _, cmd := range []string{
		"go build ./...",
		"curl https://example.com/file.json",
		"docker run -v /tmp:/tmp alpine",
	} {
		if got := shellFileAccess(cmd); len(got) != 0 {
			t.Errorf("%q produced %+v, want none", cmd, got)
		}
	}
}

func TestShellFileAccessSkipsFlagsAndBareWords(t *testing.T) {
	got := shellFileAccess("ls -la")
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestCommandNameSkipsWrappers(t *testing.T) {
	cases := map[string]string{
		"sudo cat /etc/hosts":    "cat",
		"FOO=1 head -n2 a/b.txt": "head",
		"/usr/bin/wc -l a/b.txt": "wc",
	}
	for cmd, want := range cases {
		if got := commandName(shellFields(cmd)); got != want {
			t.Errorf("%q: got %q, want %q", cmd, got, want)
		}
	}
}

// Codex writes files with a patch envelope handed to exec, not with a write
// command, so nothing in the command line names the file.
func TestPatchFileAccess(t *testing.T) {
	args := `const p = "*** Begin Patch\n*** Add File: tmp/new.txt\n+ciao\n` +
		`*** Update File: internal/app.go\n*** Delete File: old/gone.txt\n*** End Patch";`
	got := patchFileAccess(args)
	paths := map[string]string{}
	for _, f := range got {
		paths[f.Path] = f.Operation
	}
	for _, p := range []string{"tmp/new.txt", "internal/app.go", "old/gone.txt"} {
		if paths[p] != FileOpWrite {
			t.Errorf("%s: got %q, want write", p, paths[p])
		}
	}
}

func TestShellFileAccessRedirection(t *testing.T) {
	got := shellFileAccess("echo ciao > out/result.txt")
	if len(got) != 1 || got[0].Path != "out/result.txt" || got[0].Operation != FileOpWrite {
		t.Fatalf("got %+v", got)
	}
}

func TestShellFileAccessIgnoresDescriptorRedirect(t *testing.T) {
	// 2>&1 is a descriptor, not a file.
	if got := shellFileAccess("go build ./... 2>&1"); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestShellFileAccessWriteCommands(t *testing.T) {
	cases := map[string]string{
		"touch docs/new.md":      "docs/new.md",
		"cp a/src.go b/dst.go":   "b/dst.go", // only the destination is written
		"mv old/a.txt new/b.txt": "new/b.txt",
		"rm build/artifact.bin":  "build/artifact.bin",
	}
	for cmd, want := range cases {
		got := shellFileAccess(cmd)
		if len(got) != 1 || got[0].Path != want || got[0].Operation != FileOpWrite {
			t.Errorf("%q: got %+v, want write %s", cmd, got, want)
		}
	}
}

// Codex does not issue a tool call for a connector: it declares it as
// TypeScript and invokes it from inside exec, so a session that used Miro or
// SharePoint showed nothing but a shell call.
func TestBridgedToolCalls_FindsConnectorInvocations(t *testing.T) {
	args := `{"cmd":"const r = await tools.mcp__codex_apps__miro_board_search_boards({limit: 20});\n` +
		`await tools.mcp__codex_apps__sites_get_site({id: 1});"}`

	got := bridgedToolCalls(args)

	var names []ToolInvocation
	for _, c := range got {
		names = append(names, c.ToolInvocation)
	}
	assert.Equal(t, []ToolInvocation{
		{Name: "mcp__codex_apps__miro_board_search_boards", Tool: "miro_board_search_boards", Server: "miro", Source: ToolSourceMCP},
		{Name: "mcp__codex_apps__sites_get_site", Tool: "sites_get_site", Server: "sites", Source: ToolSourceMCP},
	}, names)
}

// A plain command must not be mistaken for a connector call.
func TestBridgedToolCalls_IgnoresOrdinaryCommands(t *testing.T) {
	assert.Empty(t, bridgedToolCalls(`{"cmd":"ls -la /src && cat README.md"}`))
}

// google_drive_fetch must not be attributed to Google: the plugin is Drive,
// and the console would otherwise label it with the wrong mark and name.
func TestBridgedToolCalls_NamesMultiWordPlugins(t *testing.T) {
	got := bridgedToolCalls(`{"cmd":"await tools.mcp__codex_apps__google_drive_fetch({file_name: \"eagiovani.pdf\"});"}`)

	require.Len(t, got, 1)
	assert.Equal(t, "google_drive", got[0].Server)
	assert.Contains(t, got[0].arguments, "eagiovani.pdf")
}
