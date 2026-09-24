package hivetrace

import (
	"encoding/json"
	"regexp"
	"strings"
)

// fileToolOps maps the file-touching tools coding agents ship with to the kind
// of access they represent. The names are the bare, lowercased form, so a tool
// reached through an MCP bridge is recognised the same as a native one.
//
// The set intentionally mirrors livezone's protected-tool list, which exists
// for the opposite reason — those are the tools whose output must not be
// rewritten because the agent quotes it by offset. That they coincide is not a
// coincidence: they are the tools that address real files.
var fileToolOps = map[string]string{
	"read":                        FileOpRead,
	"view":                        FileOpRead,
	"notebookread":                FileOpRead,
	"write":                       FileOpWrite,
	"create":                      FileOpWrite,
	"edit":                        FileOpWrite,
	"multiedit":                   FileOpWrite,
	"insert":                      FileOpWrite,
	"notebookedit":                FileOpWrite,
	"str_replace_editor":          FileOpWrite,
	"str_replace_based_edit_tool": FileOpWrite,
	"text_editor":                 FileOpWrite,
	"glob":                        FileOpSearch,
	"grep":                        FileOpSearch,
	"search":                      FileOpSearch,

	// MCP servers expose the same three operations under their own names. A
	// document pulled off SharePoint through read_resource is as much a file
	// access as a local Read, and an audit that showed only the local one
	// would miss exactly the reads that left the machine.
	"read_resource":         FileOpRead,
	"readresource":          FileOpRead,
	"read_file":             FileOpRead,
	"read_text_file":        FileOpRead,
	"read_media_file":       FileOpRead,
	"get_file_contents":     FileOpRead,
	"fetch_document":        FileOpRead,
	"fetch":                 FileOpRead,
	"get_document":          FileOpRead,
	"download":              FileOpRead,
	"write_file":            FileOpWrite,
	"create_or_update_file": FileOpWrite,
	"edit_file":             FileOpWrite,
	"list_directory":        FileOpSearch,
	"directory_tree":        FileOpSearch,
	"search_files":          FileOpSearch,
	"sharepoint_search":     FileOpSearch,
	"recent_documents":      FileOpSearch,
	"search_documents":      FileOpSearch,
}

// pathArgKeys are the argument names these tools use for the path they act on,
// in the order they should be preferred. Clients disagree on spelling, and a
// tool call carries exactly one of them.
//
// The MCP spelling is uri: resources are addressed by URI, not by path, so a
// list without it recognises the tool and then finds nothing to record. The
// document keys come last: a connector names the file it fetched, and an
// opaque id is a poorer answer than a name but still better than silence.
var pathArgKeys = []string{
	"file_path", "filePath", "notebook_path", "target_file",
	"path", "filename", "file", "absolute_path",
	"uri", "url", "resource_uri", "resourceUri",
	"document_name", "file_name", "fileName",
	"document_id", "documentId", "file_id", "fileId",
}

// extractFiles derives the paths a turn's tool calls addressed.
//
// Only the path is kept. The rest of the arguments — file contents on a write,
// the replacement text on an edit — is exactly the payload this subsystem must
// not accumulate.
func extractFiles(calls []toolCall) []FileAccess {
	var out []FileAccess
	seen := make(map[string]struct{})

	for _, c := range calls {
		if c.arguments == "" {
			continue
		}
		// A terminal tool names its files inside a command line, not in a
		// structured argument.
		if shellTools[strings.ToLower(c.Tool)] || strings.EqualFold(c.Tool, "apply_patch") {
			// A patch envelope names its files in its own directives, and a
			// command line names them as operands. An agent may use either
			// through the same tool, so both are read.
			accesses := patchFileAccess(c.arguments)
			for _, cmd := range shellCommands(c.arguments) {
				accesses = append(accesses, shellFileAccess(cmd)...)
			}
			for _, fa := range accesses {
				key := fa.Operation + "\x00" + fa.Path
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, fa)
			}
			continue
		}
		op, ok := fileToolOps[strings.ToLower(c.Tool)]
		if !ok {
			op, ok = suffixFileOp(c.Tool)
		}
		if !ok {
			continue
		}
		var args map[string]json.RawMessage
		var path string
		if json.Unmarshal([]byte(c.arguments), &args) == nil {
			path = firstStringArg(args, pathArgKeys)
		} else {
			// Not JSON: Codex writes its connector arguments as a JavaScript
			// object literal, with unquoted keys. Read by key instead.
			path = firstLiteralArg(c.arguments, pathArgKeys)
		}
		if path == "" {
			// Every connector spells the argument differently, and a list of
			// names will always be one service behind. The tool is already
			// known to address a document, so the value that looks like one is
			// taken whatever it was called. A search term is rejected by the
			// same rule that rejects it in a shell command: no separator, no
			// extension, not a file.
			path = pathLikeArg(c.arguments)
		}
		if path == "" {
			continue
		}
		key := op + "\x00" + path
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, FileAccess{Path: path, Operation: op, Tool: c.Tool})
	}
	return out
}

// suffixFileOp resolves a tool that arrived with its service prefixed onto it.
// Codex names a connector tool after the plugin it belongs to, so the read in
// sharepoint_read_resource is only visible past the prefix. The match is on a
// whole trailing segment, so a tool merely ending in the same letters is not
// mistaken for one.
func suffixFileOp(name string) (string, bool) {
	lower := strings.ToLower(name)
	for known, op := range fileToolOps {
		if strings.HasSuffix(lower, "_"+known) {
			return op, true
		}
	}
	return "", false
}

// literalArg matches one `key: "value"` pair of a JavaScript object literal,
// with the key quoted or bare.
var literalArg = regexp.MustCompile(`["']?([A-Za-z_][A-Za-z0-9_]*)["']?\s*:\s*["']((?:[^"'\\]|\\.)*)["']`)

// firstLiteralArg reads a value out of a JavaScript object literal, preferring
// the keys in order, the way firstStringArg does for JSON.
func firstLiteralArg(arguments string, keys []string) string {
	found := make(map[string]string)
	for _, m := range literalArg.FindAllStringSubmatch(arguments, -1) {
		if _, dup := found[m[1]]; !dup {
			found[m[1]] = unescape(m[2])
		}
	}
	for _, k := range keys {
		if v := strings.TrimSpace(found[k]); v != "" {
			return v
		}
	}
	return ""
}

// pathLikeArg returns the first argument value shaped like a file, whatever
// key it arrived under. Only called once the tool is known to address a
// document, so the question is which argument names the file, not whether the
// call touched one.
func pathLikeArg(arguments string) string {
	for _, m := range literalArg.FindAllStringSubmatch(arguments, -1) {
		v := unescape(m[2])
		if looksLikePath(v) {
			return v
		}
	}
	return ""
}

func firstStringArg(args map[string]json.RawMessage, keys []string) string {
	for _, k := range keys {
		raw, ok := args[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
