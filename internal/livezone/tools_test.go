package livezone

import "testing"

// Compressing these corrupts the agent's next action rather than merely
// costing accuracy: line numbers, match offsets and file bodies are quoted
// back verbatim.
func TestProtectedTools_FileAndSearchToolsAreNeverCompressed(t *testing.T) {
	for _, name := range []string{
		"Read", "read", "Glob", "Grep", "Write", "Edit", "MultiEdit",
		"WebSearch", "WebFetch", "web_search", "str_replace_editor",
	} {
		if !IsProtected(name, nil) {
			t.Errorf("%s must be protected", name)
		}
	}
}

// Shell output is the target: large, repetitive, read for conclusions.
func TestProtectedTools_ShellOutputIsCompressible(t *testing.T) {
	for _, name := range []string{"Bash", "bash", "run_command", "shell"} {
		if IsProtected(name, nil) {
			t.Errorf("%s should be compressible by default", name)
		}
	}
}

// An operator who disagrees can protect it.
func TestProtectedTools_OperatorCanExtendTheList(t *testing.T) {
	if !IsProtected("Bash", []string{"bash"}) {
		t.Error("operator override was ignored")
	}
	if !IsProtected("mcp__internal__query_db", []string{"mcp__internal__*"}) {
		t.Error("prefix pattern should protect a whole MCP server")
	}
	if IsProtected("mcp__other__query_db", []string{"mcp__internal__*"}) {
		t.Error("prefix pattern matched the wrong server")
	}
}

// A protected tool reached through an MCP bridge is still that tool.
func TestProtectedTools_MCPWrappersResolveToTheRealTool(t *testing.T) {
	for _, name := range []string{
		"mcp__filesystem__read", "mcp_filesystem_read", "mcp__fs__grep",
	} {
		if !IsProtected(name, nil) {
			t.Errorf("%s unwraps to a protected tool and must be protected", name)
		}
	}
}

// An unnamed result cannot be checked, so it is left alone. Skipping a
// compression costs nothing; corrupting a Read costs the task.
func TestProtectedTools_UnknownNameFailsSafe(t *testing.T) {
	if !IsProtected("", nil) {
		t.Error("an unnamed tool result must be treated as protected")
	}
}
