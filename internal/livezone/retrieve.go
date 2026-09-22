package livezone

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// RetrievePath is where a caller asks for content a lossy transform removed.
const RetrievePath = "/v1/hive/retrieve/"

// defaultVault is the store the middleware fills and the retrieval handler
// reads. It is package state because the two are registered from different
// places and must agree on one instance; nothing else may reach into it.
var defaultVault *Vault

// DefaultVault returns the store the middleware is filling, or nil when live
// compression is off or retrieval is not configured.
func DefaultVault() *Vault { return defaultVault }

// retrieveNote is appended to content a lossy transform shortened, naming
// where the full version can be fetched.
//
// No MCP server and no injected tool: an agent already has a shell, and one
// line of text is enough to tell it where to look. A human reading the
// transcript can follow the same pointer, which is what makes a lossy
// transform auditable rather than a claim.
func retrieveNote(id string, originalBytes int) string {
	return fmt.Sprintf("\n\n[hive: shortened from %d bytes — full output: GET %s%s]",
		originalBytes, RetrievePath, id)
}

// reversible stores the original of a lossy transform and appends the note
// that points at it.
//
// It returns the content unchanged when there is nothing to point at, and —
// importantly — when the note would eat the saving. A transform that shed 90
// bytes has not earned a 70-byte footnote.
func reversible(pol Policy, res Result, original string) (string, bool) {
	if !res.Lossy || pol.Vault == nil || pol.Scope == "" {
		return res.Content, false
	}
	id, ok := pol.Vault.Put(pol.Scope, original)
	if !ok {
		return res.Content, false
	}
	note := retrieveNote(id, len(original))
	if len(res.Content)+len(note) >= len(original) {
		return res.Content, false
	}
	return res.Content + note, true
}

// ScopeFor derives the partition a caller's removed content is filed under.
//
// Each component is length-prefixed rather than delimited: the parts come from
// authentication rather than from the request, but an injective encoding costs
// nothing and removes the question entirely.
func ScopeFor(teamID, keyHash string) string {
	if teamID == "" && keyHash == "" {
		return ""
	}
	h := sha256.New()
	for _, part := range []string{teamID, keyHash} {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// RetrieveID extracts the identifier from a retrieval request path.
func RetrieveID(path string) string {
	id := strings.TrimPrefix(path, RetrievePath)
	if id == path || id == "" || strings.Contains(id, "/") {
		return ""
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return id
}
