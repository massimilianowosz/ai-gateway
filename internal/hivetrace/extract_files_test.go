package hivetrace

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func call(name, args string) toolCall {
	tool, server, source := parseToolName(name)
	return toolCall{
		ToolInvocation: ToolInvocation{Name: name, Tool: tool, Server: server, Source: source},
		arguments:      args,
	}
}

func TestExtractFiles_OperationsByTool(t *testing.T) {
	files := extractFiles([]toolCall{
		call("Read", `{"file_path":"/src/a.go"}`),
		call("Write", `{"file_path":"/src/b.go","content":"package main"}`),
		call("Grep", `{"pattern":"main","path":"/src"}`),
		call("NotebookEdit", `{"notebook_path":"/nb/x.ipynb"}`),
	})

	require.Len(t, files, 4)
	assert.Equal(t, FileAccess{Path: "/src/a.go", Operation: FileOpRead, Tool: "Read"}, files[0])
	assert.Equal(t, FileOpWrite, files[1].Operation)
	assert.Equal(t, FileOpSearch, files[2].Operation)
	assert.Equal(t, "/nb/x.ipynb", files[3].Path)
}

// An MCP-bridged file tool is the same capability under a different name and
// must resolve to the same operation.
func TestExtractFiles_RecognisesMCPBridgedTools(t *testing.T) {
	files := extractFiles([]toolCall{call("mcp__fs__read", `{"path":"/etc/hosts"}`)})
	require.Len(t, files, 1)
	assert.Equal(t, FileOpRead, files[0].Operation)
	assert.Equal(t, "/etc/hosts", files[0].Path)
}

// A document read off a remote MCP server is the file access that matters most
// to an auditor: it left the machine. MCP addresses resources by uri, so a
// path lookup that only knew file_path recognised the tool and recorded
// nothing.
func TestExtractFiles_RecordsMCPResourceReads(t *testing.T) {
	files := extractFiles([]toolCall{
		call("mcp__claude_ai_Microsoft_365__read_resource",
			`{"uri":"https://contoso.sharepoint.com/Shared/Budget2026.xlsx"}`),
		call("mcp__claude_ai_Microsoft_365__sharepoint_search",
			`{"uri":"https://contoso.sharepoint.com/Shared"}`),
	})

	assert.ElementsMatch(t, []FileAccess{
		{Path: "https://contoso.sharepoint.com/Shared/Budget2026.xlsx", Operation: FileOpRead, Tool: "read_resource"},
		{Path: "https://contoso.sharepoint.com/Shared", Operation: FileOpSearch, Tool: "sharepoint_search"},
	}, files)
}

// Codex names a connector tool after the plugin that owns it, so the operation
// is only visible past the prefix.
func TestExtractFiles_ResolvesServicePrefixedTools(t *testing.T) {
	files := extractFiles([]toolCall{
		call("mcp__codex_apps__sharepoint_read_resource",
			`{"uri":"https://contoso.sharepoint.com/Shared/Q3.docx"}`),
	})
	require.Len(t, files, 1)
	assert.Equal(t, FileOpRead, files[0].Operation)
	assert.Equal(t, "https://contoso.sharepoint.com/Shared/Q3.docx", files[0].Path)
}

// Codex writes its connector arguments as a JavaScript object literal, with
// unquoted keys, so a JSON parser rejects them and the document the agent
// pulled off Drive went unrecorded.
func TestExtractFiles_ReadsJavaScriptArguments(t *testing.T) {
	files := extractFiles([]toolCall{{
		ToolInvocation: ToolInvocation{
			Name: "mcp__codex_apps__google_drive_fetch", Tool: "google_drive_fetch",
			Server: "google_drive", Source: ToolSourceMCP,
		},
		arguments: `{file_name: "eagiovani.pdf", mode: "text"}`,
	}})

	require.Len(t, files, 1)
	assert.Equal(t, FileOpRead, files[0].Operation)
	assert.Equal(t, "eagiovani.pdf", files[0].Path)
}

// Every connector names the argument differently, so a document is recorded on
// its shape rather than on a key the list happens to know.
func TestExtractFiles_TakesThePathLikeArgumentWhateverItIsCalled(t *testing.T) {
	files := extractFiles([]toolCall{{
		ToolInvocation: ToolInvocation{
			Tool: "google_drive_fetch", Server: "google_drive", Source: ToolSourceMCP,
		},
		arguments: `{ref: "Report Q3.docx", mode: "text", topn: 10}`,
	}})

	require.Len(t, files, 1)
	assert.Equal(t, "Report Q3.docx", files[0].Path)
}

// A search term is not a file. Recording "eagiovani" as a document read would
// claim the agent opened something it only looked for.
func TestExtractFiles_IgnoresSearchTerms(t *testing.T) {
	files := extractFiles([]toolCall{{
		ToolInvocation: ToolInvocation{
			Tool: "google_drive_search", Server: "google_drive", Source: ToolSourceMCP,
		},
		arguments: `{query:"eagiovani",item_type:"document",topn:10}`,
	}})

	assert.Empty(t, files)
}

func TestExtractFiles_SkipsUnknownAndUnparseable(t *testing.T) {
	files := extractFiles([]toolCall{
		call("Read", `{"file_path":`), // truncated mid-stream
		call("Read", `{}`),
		call("Read", ``),
		call("Bash", `{"command":"go build ./..."}`), // no file operand to recover
	})
	assert.Empty(t, files)
}

// A terminal tool used to be skipped outright, which left agents that work
// through a shell — Codex uses nothing else — showing no file activity at all.
func TestExtractFiles_ReadsShellCommands(t *testing.T) {
	files := extractFiles([]toolCall{
		call("Bash", `{"command":"ls -la /src"}`),
		call("Bash", `{"command":"cat internal/app.go"}`),
	})
	assert.ElementsMatch(t, []FileAccess{
		{Path: "/src", Operation: FileOpSearch, Tool: "ls"},
		{Path: "internal/app.go", Operation: FileOpRead, Tool: "cat"},
	}, files)
}

func TestExtractFiles_DeduplicatesSamePathAndOperation(t *testing.T) {
	files := extractFiles([]toolCall{
		call("Read", `{"file_path":"/src/a.go"}`),
		call("View", `{"file_path":"/src/a.go"}`),
		call("Write", `{"file_path":"/src/a.go"}`),
	})
	require.Len(t, files, 2, "same path read twice is one read, but a write is a distinct access")
	assert.Equal(t, FileOpRead, files[0].Operation)
	assert.Equal(t, FileOpWrite, files[1].Operation)
}

// Only the path is retained: the rest of a write's arguments is file content,
// which this subsystem must never accumulate.
func TestExtractFiles_RetainsOnlyThePath(t *testing.T) {
	files := extractFiles([]toolCall{
		call("Write", `{"file_path":"/src/secrets.env","content":"AWS_SECRET=abc"}`),
	})
	require.Len(t, files, 1)
	assert.Equal(t, "/src/secrets.env", files[0].Path)
	assert.NotContains(t, files[0].Tool, "AWS_SECRET")
}
