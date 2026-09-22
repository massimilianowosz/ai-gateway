package hivestate

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// CCRStore is the Compress-Cache-Retrieve store.
//
// It keeps the file contents a conversation has already read, so the ones the
// agent is still actively working with can be re-injected after HiveState
// compresses the history away. Files enter a working set when read, are
// refreshed whenever they are mentioned again, and decay out after
// decayCalls turns without a mention.
//
// That content is verbatim user data — source files, tool output — so every
// entry is filed under a Scope and a lookup only ever sees entries filed under
// the identical scope. There is no shared bucket and no global fallback: a
// request whose scope is under-specified gets no CCR at all. See ccr_scope.go.
//
// Storage is bounded twice. Each scope holds at most maxPerScope files with
// its own oldest-first eviction, so one busy tenant cannot evict another's
// content. The number of live scopes is bounded by maxScopes with
// least-recently-used eviction.
type CCRStore struct {
	mu          sync.RWMutex
	scopes      map[string]*ccrScope
	maxScopes   int
	maxPerScope int
	ttl         time.Duration
	decayCalls  int
	// bytes is the retained content across every scope. Entry counts alone do
	// not bound memory: maxScopes*maxPerScope entries of the largest content
	// extractFileReads accepts is hundreds of megabytes, and nothing else
	// applies eviction pressure.
	bytes int
}

const (
	// ccrMaxScopeBytes bounds the file content one conversation may retain.
	ccrMaxScopeBytes = 1 << 20 // 1 MiB
	// ccrMaxTotalBytes bounds what the store retains across every scope. It is
	// the real memory ceiling; the scope and entry counts only shape how that
	// budget is shared.
	ccrMaxTotalBytes = 128 << 20 // 128 MiB
)

// ccrScope is one tenant+key+session partition.
//
// callCount is per-scope, not global. Decay measures turns within *this*
// conversation; a global counter would have let unrelated traffic age a
// working set out from under an idle session.
type ccrScope struct {
	files      map[string]*ccrFileEntry
	callCount  int
	lastAccess time.Time
	bytes      int // retained content, kept in step with CCRStore.bytes
}

type ccrFileEntry struct {
	path         string
	content      string // the file content as seen in the conversation
	tokens       int    // token count, computed once at store time
	createdAt    time.Time
	lastCallSeen int // scope call count when this file was last mentioned
	inWorkingSet bool
}

func newCCRStore(maxScopes, maxPerScope int, ttl time.Duration) *CCRStore {
	if maxScopes <= 0 {
		maxScopes = 512
	}
	if maxPerScope <= 0 {
		maxPerScope = 32
	}
	return &CCRStore{
		scopes:      make(map[string]*ccrScope),
		maxScopes:   maxScopes,
		maxPerScope: maxPerScope,
		ttl:         ttl,
		decayCalls:  5, // exit working set after 5 turns without mention
	}
}

// filePathRe matches common file paths in tool outputs and messages.
// readPathPatterns are compiled once: extractReadPaths runs per assistant
// message per request, and compilation dominated the matching cost.
var readPathPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?:read_file|Read|cat|head|tail)\s*[("]*(/[a-zA-Z][a-zA-Z0-9._\-/]+\.[a-zA-Z]{1,10})`),
	regexp.MustCompile(`"filePath"\s*:\s*"(/[a-zA-Z][a-zA-Z0-9._\-/]+\.[a-zA-Z]{1,10})"`),
}

var filePathRe = regexp.MustCompile(`(?:^|[\s"'` + "`" + `])(/[a-zA-Z][a-zA-Z0-9._\-/]*\.[a-zA-Z]{1,10})`)

// scopeLocked returns the partition for key, optionally creating it.
// Caller must hold the write lock when create is true.
func (c *CCRStore) scopeLocked(key string, create bool) *ccrScope {
	if sc, ok := c.scopes[key]; ok {
		return sc
	}
	if !create {
		return nil
	}
	if len(c.scopes) >= c.maxScopes {
		var lruKey string
		var lruTime time.Time
		for k, v := range c.scopes {
			if lruKey == "" || v.lastAccess.Before(lruTime) {
				lruKey, lruTime = k, v.lastAccess
			}
		}
		if lruKey != "" {
			c.bytes -= c.scopes[lruKey].bytes
			delete(c.scopes, lruKey)
		}
	}
	sc := &ccrScope{files: make(map[string]*ccrFileEntry), lastAccess: time.Now()}
	c.scopes[key] = sc
	return sc
}

// Store extracts file reads from History messages and caches them under scope.
// Newly read files enter the working set. An invalid scope stores nothing and
// returns "" — CCR is simply off for that request.
func (c *CCRStore) Store(scope Scope, messages []Message, counter TokenCounter) string {
	if !scope.Valid() {
		return ""
	}
	files := extractFileReads(messages)

	c.mu.Lock()
	defer c.mu.Unlock()

	sc := c.scopeLocked(scope.key(), true)
	sc.lastAccess = time.Now()
	sc.callCount++

	if len(files) == 0 {
		return "none"
	}

	for path, content := range files {
		// A file read on turn 3 is still in History on turn 20, so it is
		// re-extracted every turn. Re-stamping it here is what made the TTL and
		// the decay check unreachable: createdAt and lastCallSeen were reset on
		// every call, so nothing ever aged out or left the working set, and
		// stale files were re-injected forever. An unchanged re-extraction is
		// not a new read — leave the entry alone and let GetWorkingSet
		// re-activate it if this turn actually mentions the file.
		if prev, exists := sc.files[path]; exists && prev.content == content {
			continue
		}
		// Token count is computed here, under the write lock. The read path
		// must never mutate an entry: it holds only a read lock, and a lazy
		// count there was a data race.
		c.putLocked(sc, path, &ccrFileEntry{
			path:         path,
			content:      content,
			tokens:       counter.Count(content),
			createdAt:    time.Now(),
			lastCallSeen: sc.callCount,
			inWorkingSet: true,
		})
	}

	return ccrHashPaths(files)
}

// GetWorkingSet returns the scope's active working set.
//
// Files mentioned in texts are refreshed; files unmentioned for decayCalls
// turns decay out. Results are ordered by mention first, then recency, and
// capped at maxTokens.
func (c *CCRStore) GetWorkingSet(scope Scope, texts []string, maxTokens int, counter TokenCounter) []Message {
	if !scope.Valid() {
		return nil
	}

	mentionedPaths := make(map[string]bool)
	for _, text := range texts {
		for _, match := range filePathRe.FindAllStringSubmatch(text, -1) {
			if len(match) > 1 {
				mentionedPaths[match[1]] = true
			}
		}
	}

	// One write lock for the whole operation: refresh, decay and selection all
	// touch entry state, so splitting into write-then-read reintroduced a
	// window where another request could observe a half-updated working set.
	c.mu.Lock()
	defer c.mu.Unlock()

	sc := c.scopeLocked(scope.key(), false)
	if sc == nil {
		return nil
	}
	sc.lastAccess = time.Now()

	for path, entry := range sc.files {
		if mentionedPaths[path] {
			entry.lastCallSeen = sc.callCount
			entry.inWorkingSet = true
			continue
		}
		// Basename match
		parts := strings.Split(path, "/")
		basename := parts[len(parts)-1]
		if len(basename) > 3 {
			for _, text := range texts {
				if strings.Contains(text, basename) {
					entry.lastCallSeen = sc.callCount
					entry.inWorkingSet = true
					mentionedPaths[path] = true
					break
				}
			}
		}
		// Decay
		if entry.inWorkingSet && (sc.callCount-entry.lastCallSeen) > c.decayCalls {
			entry.inWorkingSet = false
		}
	}

	type scored struct {
		entry *ccrFileEntry
		prio  int
	}
	var candidates []scored

	now := time.Now()
	for _, entry := range sc.files {
		if now.Sub(entry.createdAt) > c.ttl || !entry.inWorkingSet {
			continue
		}
		if entry.tokens == 0 {
			entry.tokens = counter.Count(entry.content)
		}
		prio := 1
		if mentionedPaths[entry.path] {
			prio = 2
		}
		candidates = append(candidates, scored{entry: entry, prio: prio})
	}

	// Mentioned first, then most recently seen, then path for a stable order.
	// Determinism matters: the same working set must serialize identically on
	// every turn, or the injected bytes change and defeat the point of placing
	// them past the cacheable prefix.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.prio != b.prio {
			return a.prio > b.prio
		}
		if a.entry.lastCallSeen != b.entry.lastCallSeen {
			return a.entry.lastCallSeen > b.entry.lastCallSeen
		}
		return a.entry.path < b.entry.path
	})

	var result []Message
	totalTokens := 0
	for _, cand := range candidates {
		if totalTokens+cand.entry.tokens > maxTokens {
			continue
		}
		result = append(result, Message{
			Role:    "user",
			Content: "[Working file: " + cand.entry.path + "]\n" + cand.entry.content,
		})
		totalTokens += cand.entry.tokens
	}

	return result
}

// FindByPaths is the legacy interface — delegates to GetWorkingSet.
func (c *CCRStore) FindByPaths(scope Scope, texts []string, maxTokens int, counter TokenCounter) []Message {
	return c.GetWorkingSet(scope, texts, maxTokens, counter)
}

// FindRelevant is the legacy interface — delegates to GetWorkingSet.
func (c *CCRStore) FindRelevant(scope Scope, query string, maxTokens int, counter TokenCounter) []Message {
	return c.GetWorkingSet(scope, []string{query}, maxTokens, counter)
}

// Forget drops every entry for a scope. Used to honour an explicit deletion.
func (c *CCRStore) Forget(scope Scope) {
	if !scope.Valid() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.scopes, scope.key())
}

// putLocked inserts an entry, evicting the scope's oldest until both the entry
// count and the byte budgets fit. Caller holds the write lock.
func (c *CCRStore) putLocked(sc *ccrScope, path string, entry *ccrFileEntry) {
	c.removeLocked(sc, path)
	for len(sc.files) > 0 &&
		(len(sc.files) >= c.maxPerScope || sc.bytes+len(entry.content) > ccrMaxScopeBytes) {
		if !c.evictOldestLocked(sc) {
			break
		}
	}
	sc.files[path] = entry
	sc.bytes += len(entry.content)
	c.bytes += len(entry.content)
	c.evictScopesToFitLocked()
}

// removeLocked drops one entry, keeping both byte counters in step.
func (c *CCRStore) removeLocked(sc *ccrScope, path string) {
	prev, ok := sc.files[path]
	if !ok {
		return
	}
	sc.bytes -= len(prev.content)
	c.bytes -= len(prev.content)
	delete(sc.files, path)
}

// evictOldestLocked removes the scope's oldest entry and reports whether
// anything was removed. Caller holds the write lock.
func (c *CCRStore) evictOldestLocked(sc *ccrScope) bool {
	var oldestKey string
	var oldestTime time.Time
	for k, v := range sc.files {
		if oldestKey == "" || v.createdAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.createdAt
		}
	}
	if oldestKey == "" {
		return false
	}
	c.removeLocked(sc, oldestKey)
	return true
}

// evictScopesToFitLocked drops least-recently-used scopes until total retained
// content is under budget. The scope being written to carries the newest
// lastAccess, and ccrMaxScopeBytes is far below ccrMaxTotalBytes, so it cannot
// evict itself. Caller holds the write lock.
func (c *CCRStore) evictScopesToFitLocked() {
	for c.bytes > ccrMaxTotalBytes && len(c.scopes) > 0 {
		var lruKey string
		var lruTime time.Time
		for k, v := range c.scopes {
			if lruKey == "" || v.lastAccess.Before(lruTime) {
				lruKey, lruTime = k, v.lastAccess
			}
		}
		if lruKey == "" {
			return
		}
		c.bytes -= c.scopes[lruKey].bytes
		delete(c.scopes, lruKey)
	}
}

// Retrieve returns nil — legacy interface, not used in file-based CCR.
func (c *CCRStore) Retrieve(Scope, string) ([]Message, bool) {
	return nil, false
}

// --- File extraction ---

// extractFileReads scans messages for patterns that indicate file content was read.
func extractFileReads(messages []Message) map[string]string {
	files := make(map[string]string)

	for i := 0; i < len(messages); i++ {
		m := messages[i]

		// Pattern 1: Assistant asks to read a file, next message has the content
		if m.Role == "assistant" {
			paths := extractReadPaths(m.Content)
			if len(paths) > 0 && i+1 < len(messages) {
				next := messages[i+1]
				if next.Role == "tool" || next.Role == "user" {
					content := next.Content
					// Only when the turn names exactly one file. Claude Code
					// issues parallel Read calls in a single assistant message,
					// and messages[i+1] is the result of just one of them —
					// assigning it to every path cached one file's body under
					// several names and later re-injected it labelled as a file
					// it is not.
					if len(paths) == 1 && len(content) > 50 && len(content) < 50000 {
						files[paths[0]] = content
					}
				}
			}
		}

		// Pattern 2: Message contains inline file content with path header
		if m.Role == "tool" || m.Role == "user" {
			if path, content := extractInlineFile(m.Content); path != "" {
				if len(content) > 50 && len(content) < 50000 {
					files[path] = content
				}
			}
		}
	}

	return files
}

// extractReadPaths finds file paths in tool-use requests
func extractReadPaths(content string) []string {
	var paths []string
	for _, re := range readPathPatterns {
		matches := re.FindAllStringSubmatch(content, -1)
		for _, match := range matches {
			if len(match) > 1 {
				paths = append(paths, match[1])
			}
		}
	}
	return paths
}

// extractInlineFile checks if a message starts with a file path indicator
func extractInlineFile(content string) (string, string) {
	lines := strings.SplitN(content, "\n", 3)
	if len(lines) >= 2 {
		firstLine := strings.TrimSpace(lines[0])
		if matches := filePathRe.FindStringSubmatch(firstLine); len(matches) > 1 {
			rest := strings.Join(lines[1:], "\n")
			rest = strings.TrimPrefix(rest, "---\n")
			return matches[1], rest
		}
	}
	return "", ""
}

func ccrHashPaths(files map[string]string) string {
	h := sha256.New()
	for p := range files {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// CCRRef is kept for backward compatibility.
type CCRRef struct {
	ID       string `json:"id"`
	Keywords string `json:"keywords"`
}
