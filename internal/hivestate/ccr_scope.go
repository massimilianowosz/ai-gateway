package hivestate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// Scope isolates CCR content. Everything the store holds is filed under a
// scope, and a lookup only ever sees entries filed under the identical scope.
//
// This is a hard security boundary, not a partitioning convenience: CCR holds
// verbatim original messages — file contents, tool output, whatever the
// conversation carried — and re-injects them into a later prompt. A retrieval
// that crossed scopes would hand one tenant another tenant's source code.
type Scope struct {
	// TeamID is the tenant. Empty for keys with no team.
	TeamID string
	// KeyHash identifies the API key, and stands in as the tenant boundary
	// when TeamID is empty.
	KeyHash string
	// SessionID separates conversations belonging to the same key. Without it
	// one tenant's unrelated conversations would contaminate each other.
	SessionID string
}

// Valid reports whether the scope is specific enough to store or retrieve.
//
// Both an owner (team or key) and a session are required. An under-specified
// scope disables CCR for that request rather than falling back to a shared
// bucket: no memory at all is the safe failure, shared memory is not.
func (s Scope) Valid() bool {
	hasOwner := s.TeamID != "" || s.KeyHash != ""
	return hasOwner && s.SessionID != ""
}

// key derives the opaque partition key.
//
// Each component is length-prefixed rather than delimited. A delimiter — even
// NUL — is forgeable when a component can contain it, and SessionID comes
// straight from a client header: with "a\x00b"+"s" and "a"+"b\x00s" hashing
// identically, one caller could land in another's partition by choosing its
// session name. A length prefix makes the encoding injective for any input.
func (s Scope) key() string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, part := range []string{s.TeamID, s.KeyHash, s.SessionID} {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(part)))
		h.Write(lenBuf[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SessionHeader is the explicit conversation identifier a client may send.
const SessionHeader = "x-ubiquum-session"

// DeriveSessionID resolves the session for a request.
//
// An explicit header wins. Otherwise the session is derived from the opening
// of the conversation — the system messages plus the first user turn — which
// stays byte-stable for the life of a conversation while differing between
// conversations. That lets CCR work for clients that send no session header,
// without ever pooling unrelated conversations.
//
// Returns an empty string when neither source is usable, which disables CCR.
func DeriveSessionID(header string, messages []Message) string {
	if h := strings.TrimSpace(header); h != "" {
		return h
	}
	h := sha256.New()
	wroteAnchor := false
	for _, m := range messages {
		switch m.Role {
		case "system", "developer":
			h.Write([]byte(m.Role))
			h.Write([]byte{0})
			h.Write([]byte(m.Content))
			h.Write([]byte{0})
			wroteAnchor = true
		case "user":
			// The first user turn closes the anchor: everything after it
			// varies as the conversation grows.
			h.Write([]byte("user"))
			h.Write([]byte{0})
			h.Write([]byte(m.Content))
			return hex.EncodeToString(h.Sum(nil))[:32]
		}
	}
	if !wroteAnchor {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
