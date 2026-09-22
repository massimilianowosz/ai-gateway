package livezone

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
)

// RetrieveHandler serves back content a lossy transform removed.
//
// It answers the pointer the transform left in the prompt, which is what makes
// lossy compression something a caller can check rather than something it has
// to trust. An agent reaches it with the shell it already has; nothing has to
// be installed and no tool has to be injected into the request.
//
// A caller only ever sees its own scope. An identifier from another key looks
// exactly like one that expired: not found, with no hint that it exists.
func RetrieveHandler(logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RetrieveID(r.URL.Path)
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, "malformed retrieval id")
			return
		}

		scope := ""
		if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
			scope = ScopeFor(ki.TeamID, ki.KeyHash)
		}

		content, ok := defaultVault.Get(scope, id)
		if !ok {
			writeJSONError(w, http.StatusNotFound,
				"no stored content for that id: it expired, was evicted, or belongs to another key")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      id,
			"bytes":   len(content),
			"content": content,
		})
	})
}
