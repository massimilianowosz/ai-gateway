package hivestate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// --- Multi-language regex patterns for identifier extraction ---

// methodDeclPattern captures receiver.method pairs (Go-specific).
var methodDeclPattern = regexp.MustCompile(`\bfunc\s+\(\w+\s+\*?([A-Z]\w*)\)\s+([A-Za-z_]\w*)\s*\(`)

var funcDeclPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bfunc\s+([A-Za-z_]\w*)\s*\(`),
	regexp.MustCompile(`\bdef\s+([A-Za-z_]\w*)\s*\(`),
	regexp.MustCompile(`\b(?:async\s+)?function\s+([A-Za-z_$]\w*)\s*[(<]`),
	regexp.MustCompile(`\bexport\s+(?:default\s+)?(?:function|class)\s+([A-Za-z_$]\w*)`),
	regexp.MustCompile(`\b(?:pub(?:\([^)]*\))?\s+)?fn\s+([A-Za-z_]\w*)\s*[(<]`),
	regexp.MustCompile(`\bdef\s+(?:self\.)?([A-Za-z_]\w*[!?]?)\b`),
	regexp.MustCompile(`\bfun\s+([A-Za-z_]\w*)\s*[(<]`),
	regexp.MustCompile(`\b(?:public|private|protected|internal)\s+(?:(?:static|override|abstract|virtual|final|synchronized|async|suspend|inline)\s+)*(?:\w+(?:<[^>]*>)?(?:\[\])?)\s+([a-z_]\w*)\s*\(`),
	regexp.MustCompile(`\b(?:void|int|char|bool|auto|float|double|long|unsigned|size_t|string|vector|shared_ptr)\s+\*?\s*([A-Za-z_]\w*)\s*\(`),
	regexp.MustCompile(`\b(?:public|private|protected)\s+(?:static\s+)?function\s+([A-Za-z_]\w*)\s*\(`),
}

var funcRefPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.([a-zA-Z_]\w{2,})\s*\(`),
	regexp.MustCompile(`\b[A-Z]\w+\.([A-Z]\w{2,})\s*\(`),
	regexp.MustCompile(`\w+::([a-zA-Z_]\w{2,})\s*\(`),
	regexp.MustCompile(`\b([a-z_][a-zA-Z_]\w{2,})\(\)`),
}

// qualifiedRefPattern captures Package.Method or object.Method calls with both parts.
var qualifiedRefPattern = regexp.MustCompile(`\b([a-zA-Z_]\w+)\.([A-Z]\w{2,})\s*\(`)

var classDeclPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\btype\s+([A-Z]\w*)\s+(?:struct|interface)\b`),
	regexp.MustCompile(`\bclass\s+([A-Z]\w*)`),
	regexp.MustCompile(`\b(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|impl)\s+([A-Z]\w*)`),
	regexp.MustCompile(`\bmodule\s+([A-Z]\w*)`),
	regexp.MustCompile(`\b(?:interface|protocol)\s+([A-Z]\w*)`),
	regexp.MustCompile(`\b(?:object|data\s+class|sealed\s+class|case\s+class|enum\s+class)\s+([A-Z]\w*)`),
	regexp.MustCompile(`\b(?:struct|enum|actor)\s+([A-Z]\w*)`),
	regexp.MustCompile(`\b(?:namespace)\s+([A-Z]\w*)`),
	regexp.MustCompile(`\b(?:trait)\s+([A-Z]\w*)`),
}

var constDeclPattern = regexp.MustCompile(`(?m)^[[:blank:]]*(?:(?:const|let|var|val|static|final)\s+)?([A-Z][A-Z_0-9]{2,})\s*=`)

// importPatterns extract imported module/package names.
var importPatterns = []*regexp.Regexp{
	regexp.MustCompile(`"(github\.com/[^"]+)"`),
	regexp.MustCompile(`\b(?:from|import)\s+([a-zA-Z_]\w*(?:\.[a-zA-Z_]\w*)*)`),
	regexp.MustCompile(`\bfrom\s+['"]([^'"]+)['"]`),
	regexp.MustCompile(`\brequire\s*\(\s*['"]([^'"]+)['"]\s*\)`),
	regexp.MustCompile(`\buse\s+((?:crate|std|super)\w*(?:::\w+)+)`),
	regexp.MustCompile(`\buse\s+([A-Z]\w*(?:\\[A-Z]\w*)+)`),
}

var skipIdentifiers = map[string]bool{
	"main": true, "init": true, "new": true, "test": true,
	"get": true, "set": true, "run": true, "setup": true,
	"self": true, "this": true, "super": true, "cls": true,
	"True": true, "False": true, "None": true,
	"null": true, "undefined": true, "Object": true,
	"String": true, "Integer": true, "Boolean": true,
	"Error": true, "Exception": true, "Array": true,
	"Map": true, "List": true, "Set": true,
	"Test": true, "Module": true, "Kernel": true,
	"TODO": true, "FIXME": true, "NOTE": true,
	"EOF": true, "NIL": true, "NULL": true,
	"toString": true, "valueOf": true, "equals": true,
	"append": true, "println": true, "printf": true,
	"print": true, "len": true, "make": true,
	"close": true, "open": true, "read": true, "write": true,
	"log": true, "warn": true, "info": true, "debug": true,
	"err": true, "error": true, "fatal": true,
	"push": true, "pop": true, "shift": true,
	"map": true, "filter": true, "reduce": true,
	"then": true, "catch": true, "finally": true,
	"apply": true, "call": true, "bind": true,
	"next": true, "done": true, "resolve": true, "reject": true,
	"Lock": true, "Unlock": true, "RLock": true, "RUnlock": true,
}

var skipMethodCalls = map[string]bool{
	"String": true, "Error": true, "Format": true,
	"Marshal": true, "Unmarshal": true,
	"Sprintf": true, "Errorf": true, "Printf": true, "Println": true,
	"Write": true, "Read": true, "Close": true, "Open": true,
	"Lock": true, "Unlock": true, "Load": true, "Store": true,
	"Add": true, "Sub": true, "Mul": true, "Div": true,
	"Get": true, "Set": true, "Put": true, "Delete": true, "Post": true,
	"Len": true, "Cap": true, "Append": true,
	"Contains": true, "HasPrefix": true, "HasSuffix": true,
	"TrimSpace": true, "ToLower": true, "ToUpper": true, "Split": true, "Join": true,
	"Now": true, "Since": true, "After": true, "Before": true,
	"WithContext": true, "Background": true, "TODO": true,
	"Fatal": true, "Fatalf": true, "Logf": true,
	"WriteHeader": true, "Header": true, "Body": true,
	"ServeHTTP": true, "HandleFunc": true,
	"Run": true, "Start": true, "Stop": true, "Serve": true,
	"Info": true, "Debug": true, "Warn": true,
	"New": true, "Make": true, "Init": true,
	"Parse": true, "Encode": true, "Decode": true,
	"Scan": true, "Next": true, "Err": true, "Rows": true,
	"Query": true, "Exec": true, "Prepare": true,
	"NewReader": true, "NewWriter": true, "NewBuffer": true, "NewScanner": true,
	"NewRequest": true, "NewRecorder": true, "NewServer": true,
	"NopCloser": true, "ReadAll": true, "Copy": true,
	"FindSubmatch": true, "FindAllStringSubmatch": true, "MatchString": true,
	"MustCompile": true, "Compile": true,
	"HandlerFunc": true, "Handler": true, "Middleware": true,
	"Context": true, "Value": true, "WithValue": true,
	"Equal": true, "NotEqual": true, "NotEmpty": true, "NotNil": true, "Nil": true,
	"True": true, "False": true, "Require": true, "Assert": true,
}

// FileEntry groups identifiers found in the context of a specific file.
type FileEntry struct {
	Path       string
	Declared   []string // "funcName()", "Type", "Type.method()"
	Referenced []string // "pkg.Function()", "obj.method()"
	Imports    []string // project-local imports (shortened)
	LastSeen   int
}

// CodeRegistry holds file-indexed code identifiers extracted from conversation history.
type CodeRegistry struct {
	FileEntries []FileEntry
	Loose       LooseIdentifiers
}

// LooseIdentifiers are identifiers found without file context.
type LooseIdentifiers struct {
	Functions []string
	Types     []string
	Constants []string
}

// ExtractCodeRegistry scans History zone messages for code identifiers, grouped by file.
func ExtractCodeRegistry(messages []Message) *CodeRegistry {
	fileMap := make(map[string]*fileCollector)
	fileLastSeen := make(map[string]int)

	looseFuncs := make(map[string]int)
	looseTypes := make(map[string]int)
	looseConsts := make(map[string]int)

	var activeFile string

	for i, m := range messages {
		content := m.Content
		if content == "" {
			continue
		}

		// Find file paths in this message
		var msgFiles []string
		for _, match := range filePathRe.FindAllStringSubmatch(content, -1) {
			if len(match) > 1 {
				path := match[1]
				if !isNoiseFile(path) {
					msgFiles = append(msgFiles, path)
					fileLastSeen[path] = i
					if _, ok := fileMap[path]; !ok {
						fileMap[path] = newFileCollector()
					}
				}
			}
		}

		if len(msgFiles) > 0 {
			activeFile = msgFiles[0]
		}

		// Extract receiver.method pairs (Go)
		for _, match := range methodDeclPattern.FindAllStringSubmatch(content, -1) {
			if len(match) > 2 {
				receiver, method := match[1], match[2]
				if !isNoise(method) && !isNoise(receiver) {
					qualified := receiver + "." + method
					if activeFile != "" {
						fileMap[activeFile].declared[qualified+"()"] = true
					} else {
						looseFuncs[qualified] = i
					}
				}
			}
		}

		// Extract standalone function declarations
		for _, re := range funcDeclPatterns {
			for _, match := range re.FindAllStringSubmatch(content, -1) {
				if len(match) > 1 {
					name := match[1]
					if !isNoise(name) {
						if activeFile != "" {
							fileMap[activeFile].declared[name+"()"] = true
						} else {
							looseFuncs[name] = i
						}
					}
				}
			}
		}

		// Extract function/method references
		for _, match := range qualifiedRefPattern.FindAllStringSubmatch(content, -1) {
			if len(match) > 2 {
				pkg, method := match[1], match[2]
				if !skipMethodCalls[method] && !isNoise(method) && !isNoise(pkg) {
					qualified := pkg + "." + method + "()"
					if activeFile != "" {
						fileMap[activeFile].referenced[qualified] = true
					} else {
						looseFuncs[pkg+"."+method] = i
					}
				}
			}
		}
		for _, re := range funcRefPatterns {
			for _, match := range re.FindAllStringSubmatch(content, -1) {
				if len(match) > 1 {
					name := match[1]
					if !isNoise(name) && !skipMethodCalls[name] && !isFileExtension(name) {
						if activeFile != "" {
							fileMap[activeFile].referenced[name+"()"] = true
						} else {
							looseFuncs[name] = i
						}
					}
				}
			}
		}

		// Extract types
		for _, re := range classDeclPatterns {
			for _, match := range re.FindAllStringSubmatch(content, -1) {
				if len(match) > 1 {
					name := match[1]
					if !isNoise(name) {
						if activeFile != "" {
							fileMap[activeFile].declared[name] = true
						} else {
							looseTypes[name] = i
						}
					}
				}
			}
		}

		// Extract constants (always loose — not file-specific enough)
		for _, match := range constDeclPattern.FindAllStringSubmatch(content, -1) {
			if len(match) > 1 {
				name := match[1]
				if !isNoise(name) {
					looseConsts[name] = i
				}
			}
		}

		// Extract imports — only for files
		if activeFile != "" {
			for _, re := range importPatterns {
				for _, match := range re.FindAllStringSubmatch(content, -1) {
					if len(match) > 1 {
						imp := match[1]
						if isProjectImport(imp) {
							fileMap[activeFile].imports[shortenImport(imp)] = true
						}
					}
				}
			}
		}

		// Reset the active file after a tool response.
		//
		// Anthropic has no "tool" role: it delivers tool results inside user
		// messages, which parseAnthropicMessages marks with a ToolCallID. Role
		// alone therefore never matched on that path, so nothing was ever reset
		// and every identifier in the conversation was attributed to the first
		// file mentioned. ccr.go makes the same distinction the same way.
		if m.Role == "tool" || m.Role == "function" || m.ToolCallID != "" {
			activeFile = ""
		}
	}

	// Build sorted FileEntries
	shortCollisions := collidingShortPaths(fileMap)
	var entries []FileEntry
	for path, fc := range fileMap {
		entry := FileEntry{
			// The short form is only a display convenience; two projects with
			// the same relative layout shorten to the same string and rendered
			// as two sections under one heading. Disambiguate when that happens.
			Path:       displayPath(path, shortCollisions),
			Declared:   sortedKeys(fc.declared),
			Referenced: sortedKeys(fc.referenced),
			Imports:    sortedKeys(fc.imports),
			LastSeen:   fileLastSeen[path],
		}
		if len(entry.Declared) > 0 || len(entry.Referenced) > 0 {
			entries = append(entries, entry)
		}
	}
	// Ties are the common case — every identifier in one message shares an
	// index — and an unstable sort over map iteration ordered them differently
	// on every run. That is fatal for a message the provider caches: identical
	// input has to produce identical bytes. Tie-break on the path.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].LastSeen != entries[j].LastSeen {
			return entries[i].LastSeen < entries[j].LastSeen
		}
		return entries[i].Path < entries[j].Path
	})

	return &CodeRegistry{
		FileEntries: entries,
		Loose: LooseIdentifiers{
			Functions: sortByLastSeen(looseFuncs),
			Types:     sortByLastSeen(looseTypes),
			Constants: sortByLastSeen(looseConsts),
		},
	}
}

const maxReferencedPerFile = 10
const maxLooseReferenced = 15

// FormatRegistryMessage produces the compact registry string for injection.
func (r *CodeRegistry) FormatRegistryMessage() string {
	if len(r.FileEntries) == 0 && len(r.Loose.Functions) == 0 && len(r.Loose.Types) == 0 && len(r.Loose.Constants) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[Code Registry]\n")

	for _, fe := range r.FileEntries {
		funcs, types := splitDeclared(fe.Declared)
		if len(funcs) == 0 && len(types) == 0 && len(fe.Referenced) == 0 {
			continue
		}

		b.WriteByte('\n')
		b.WriteString(fe.Path)
		b.WriteByte('\n')
		if len(funcs) > 0 {
			fmt.Fprintf(&b, "  Funcs[%d]: %s\n", len(funcs), strings.Join(funcs, ", "))
		}
		if len(types) > 0 {
			fmt.Fprintf(&b, "  Types[%d]: %s\n", len(types), strings.Join(types, ", "))
		}
		if len(fe.Referenced) > 0 {
			refs := fe.Referenced
			truncated := false
			if len(refs) > maxReferencedPerFile {
				refs = refs[:maxReferencedPerFile]
				truncated = true
			}
			suffix := ""
			if truncated {
				suffix = ", ..."
			}
			fmt.Fprintf(&b, "  Refs[%d]: %s%s\n", len(fe.Referenced), strings.Join(refs, ", "), suffix)
		}
		if len(fe.Imports) > 0 {
			b.WriteString("  Imports: ")
			b.WriteString(strings.Join(fe.Imports, ", "))
			b.WriteByte('\n')
		}
	}

	if len(r.Loose.Functions) > 0 || len(r.Loose.Types) > 0 || len(r.Loose.Constants) > 0 {
		b.WriteString("\nReferenced:\n")
		if len(r.Loose.Functions) > 0 {
			funcs := r.Loose.Functions
			suffix := ""
			if len(funcs) > maxLooseReferenced {
				funcs = funcs[len(funcs)-maxLooseReferenced:]
				suffix = ", ..."
			}
			fmt.Fprintf(&b, "  Funcs[%d]: %s%s\n", len(r.Loose.Functions), strings.Join(funcs, ", "), suffix)
		}
		if len(r.Loose.Types) > 0 {
			fmt.Fprintf(&b, "  Types[%d]: %s\n", len(r.Loose.Types), strings.Join(r.Loose.Types, ", "))
		}
		if len(r.Loose.Constants) > 0 {
			fmt.Fprintf(&b, "  Consts[%d]: %s\n", len(r.Loose.Constants), strings.Join(r.Loose.Constants, ", "))
		}
	}

	return b.String()
}

// splitDeclared separates declared identifiers into functions (ending with "()") and types.
func splitDeclared(declared []string) (funcs, types []string) {
	for _, d := range declared {
		if strings.HasSuffix(d, "()") {
			funcs = append(funcs, d)
		} else {
			types = append(types, d)
		}
	}
	return
}

const registryTokenBudget = 500

// TruncateToTokenBudget trims the registry to fit within token budget.
func (r *CodeRegistry) TruncateToTokenBudget(counter TokenCounter) {
	for {
		msg := r.FormatRegistryMessage()
		if msg == "" || counter.Count(msg) <= registryTokenBudget {
			return
		}
		if len(r.FileEntries) > 8 {
			r.FileEntries = r.FileEntries[len(r.FileEntries)-8:]
		} else if len(r.Loose.Functions) > 10 {
			r.Loose.Functions = r.Loose.Functions[len(r.Loose.Functions)-10:]
		} else if len(r.FileEntries) > 4 {
			r.FileEntries = r.FileEntries[len(r.FileEntries)-4:]
		} else {
			for i := range r.FileEntries {
				if len(r.FileEntries[i].Declared) > 6 {
					r.FileEntries[i].Declared = r.FileEntries[i].Declared[len(r.FileEntries[i].Declared)-6:]
				}
				if len(r.FileEntries[i].Referenced) > 4 {
					r.FileEntries[i].Referenced = r.FileEntries[i].Referenced[len(r.FileEntries[i].Referenced)-4:]
				}
				if len(r.FileEntries[i].Imports) > 3 {
					r.FileEntries[i].Imports = r.FileEntries[i].Imports[len(r.FileEntries[i].Imports)-3:]
				}
			}
			if len(r.Loose.Functions) > 5 {
				r.Loose.Functions = r.Loose.Functions[len(r.Loose.Functions)-5:]
			}
			if len(r.Loose.Types) > 5 {
				r.Loose.Types = r.Loose.Types[len(r.Loose.Types)-5:]
			}
			if len(r.FileEntries) <= 4 && len(r.Loose.Functions) <= 5 {
				return
			}
		}
	}
}

// --- Helpers ---

type fileCollector struct {
	declared   map[string]bool
	referenced map[string]bool
	imports    map[string]bool
}

func newFileCollector() *fileCollector {
	return &fileCollector{
		declared:   make(map[string]bool),
		referenced: make(map[string]bool),
		imports:    make(map[string]bool),
	}
}

func isNoise(name string) bool {
	if len(name) <= 2 {
		return true
	}
	if skipIdentifiers[name] {
		return true
	}
	if strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") || strings.HasPrefix(name, "Example") {
		return true
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "mock") || strings.HasPrefix(lower, "fake") || strings.HasPrefix(lower, "stub") {
		return true
	}
	return false
}

func isFileExtension(name string) bool {
	exts := []string{"go", "py", "js", "ts", "tsx", "jsx", "rs", "rb", "java", "kt", "scala", "swift", "php", "css", "html", "json", "yaml", "yml", "toml", "xml", "sql", "md", "txt", "cfg", "ini", "env", "mod", "sum", "lock"}
	for _, ext := range exts {
		if name == ext {
			return true
		}
	}
	return false
}

func isNoiseFile(path string) bool {
	// Anchored at the start of the path: these are system locations, and a
	// substring match dropped any project that happened to contain one of them
	// deeper down — /home/me/app/var/cache on a Symfony project, say.
	systemRoots := []string{
		"/dev/null", "/tmp/", "/var/", "/etc/", "/proc/",
		"/usr/lib/", "/usr/share/",
	}
	for _, root := range systemRoots {
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	// Matched as whole path segments, so "myvendor/" and "notgit/" are not
	// mistaken for "vendor/" and ".git/".
	noiseDirs := map[string]bool{
		"node_modules": true, ".git": true, "__pycache__": true,
		".venv": true, "vendor": true, ".claude": true,
	}
	for _, segment := range strings.Split(path, "/") {
		if noiseDirs[segment] {
			return true
		}
	}
	if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/test/") || strings.Contains(path, "/__tests__/") || strings.HasSuffix(path, ".test.ts") || strings.HasSuffix(path, ".test.js") || strings.HasSuffix(path, ".spec.ts") || strings.HasSuffix(path, ".spec.js") {
		return true
	}
	return false
}

// isProjectImport returns true for non-stdlib imports worth tracking.
func isProjectImport(imp string) bool {
	if strings.Contains(imp, "/") || strings.Contains(imp, "\\") {
		// Skip standard Go packages
		stdPrefixes := []string{"fmt", "os", "io", "net", "sync", "time", "math", "sort", "strings", "strconv", "context", "errors", "testing", "bytes", "bufio", "encoding", "reflect", "unsafe", "runtime", "crypto", "database", "html", "log", "path", "regexp", "unicode", "archive", "compress", "debug", "embed", "go/", "hash", "image", "index", "mime", "plugin", "text"}
		for _, prefix := range stdPrefixes {
			if imp == prefix || strings.HasPrefix(imp, prefix+"/") {
				return false
			}
		}
		return true
	}
	// Dotted imports (e.g., sqlalchemy.orm) are real module paths
	if strings.Contains(imp, ".") {
		return true
	}
	// Single bare words are too unreliable — often English words after "from"/"import"
	// Real project imports always have a path separator or dot
	return false
}

// shortenImport extracts the meaningful last segment(s) of an import path.
func shortenImport(imp string) string {
	// github.com/org/repo/internal/store → store
	// github.com/org/repo/pkg/auth → auth
	parts := strings.Split(imp, "/")
	if len(parts) <= 2 {
		return imp
	}
	// Return last segment, or last two if last is generic
	last := parts[len(parts)-1]
	generic := map[string]bool{"v1": true, "v2": true, "v3": true, "api": true, "pkg": true, "lib": true, "src": true, "cmd": true}
	if generic[last] && len(parts) > 2 {
		return parts[len(parts)-2] + "/" + last
	}
	return last
}

// collidingShortPaths reports which shortened forms more than one full path
// maps to, so those entries can keep enough of the path to stay distinct.
func collidingShortPaths(fileMap map[string]*fileCollector) map[string]bool {
	count := make(map[string]int, len(fileMap))
	for path := range fileMap {
		count[shortenPath(path)]++
	}
	collisions := make(map[string]bool)
	for short, n := range count {
		if n > 1 {
			collisions[short] = true
		}
	}
	return collisions
}

// displayPath is shortenPath unless the short form is ambiguous in this
// registry, in which case the full path is used.
func displayPath(path string, collisions map[string]bool) string {
	short := shortenPath(path)
	if collisions[short] {
		return path
	}
	return short
}

func shortenPath(path string) string {
	if idx := strings.Index(path, "/internal/"); idx >= 0 {
		return path[idx:]
	}
	if idx := strings.Index(path, "/src/"); idx >= 0 {
		return path[idx:]
	}
	if idx := strings.Index(path, "/app/"); idx >= 0 {
		return path[idx:]
	}
	if idx := strings.Index(path, "/lib/"); idx >= 0 {
		return path[idx:]
	}
	if idx := strings.Index(path, "/pkg/"); idx >= 0 {
		return path[idx:]
	}
	if idx := strings.Index(path, "/cmd/"); idx >= 0 {
		return path[idx:]
	}
	parts := strings.Split(path, "/")
	if len(parts) > 4 {
		return ".../" + strings.Join(parts[len(parts)-3:], "/")
	}
	return path
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortByLastSeen(seen map[string]int) []string {
	if len(seen) == 0 {
		return nil
	}
	type entry struct {
		name string
		pos  int
	}
	entries := make([]entry, 0, len(seen))
	for name, pos := range seen {
		entries = append(entries, entry{name, pos})
	}
	// Same reasoning as the file ordering above: tie-break on the name so the
	// same input always renders the same bytes.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].pos != entries[j].pos {
			return entries[i].pos < entries[j].pos
		}
		return entries[i].name < entries[j].name
	})
	result := make([]string, len(entries))
	for i, e := range entries {
		result[i] = e.name
	}
	return result
}
