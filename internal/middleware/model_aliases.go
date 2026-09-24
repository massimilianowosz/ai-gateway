package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
)

type requestedModelContextKey struct{}

// RequestedModelFromContext returns the client-supplied alias when a request
// was canonicalized. It is intentionally diagnostic only; routing, access
// control and accounting use the canonical model.
func RequestedModelFromContext(ctx context.Context) string {
	model, _ := ctx.Value(requestedModelContextKey{}).(string)
	return model
}

// ContextWithRequestedModel records the client's own spelling. Exported for
// tests and for handlers that resolve an alias themselves.
func ContextWithRequestedModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, requestedModelContextKey{}, model)
}

// ModelAliases canonicalizes the model before cache, guardrail, routing and
// authorization consume it. It handles the JSON APIs and the X-Ubiquum-Model
// Files API header, while leaving invalid bodies untouched for the endpoint's
// existing validation to report.
//
// resolveRequest settles the model a request asked for and may depend on who
// is calling: the same name means a different deployment to a caller paying
// with their own subscription. canonicalize stays context-free because it also
// normalizes the model lists on a key and a team, which are configuration
// rather than anything the request says.
func ModelAliases(canonicalize func(string) string, resolveRequest func(context.Context, string) string) Middleware {
	if canonicalize == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if resolveRequest == nil {
		resolveRequest = func(_ context.Context, model string) string { return canonicalize(model) }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.ContextWithCanonicalModels(r.Context(), canonicalize)

			if requested := strings.TrimSpace(r.Header.Get("X-Ubiquum-Model")); requested != "" {
				if canonical := resolveRequest(ctx, requested); canonical != requested {
					r.Header.Set("X-Ubiquum-Model", canonical)
					ctx = context.WithValue(ctx, requestedModelContextKey{}, requested)
				}
			}

			if isJSONRequest(r) && r.Body != nil && r.Body != http.NoBody {
				body, err := io.ReadAll(r.Body)
				if err == nil {
					_ = r.Body.Close()
					body, requested := canonicalizeJSONModel(body, func(model string) string {
						return resolveRequest(ctx, model)
					})
					r.Body = io.NopCloser(bytes.NewReader(body))
					r.ContentLength = int64(len(body))
					if requested != "" {
						ctx = context.WithValue(ctx, requestedModelContextKey{}, requested)
					}
				}
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isJSONRequest(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func canonicalizeJSONModel(body []byte, canonicalize func(string) string) ([]byte, string) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return body, ""
	}
	rawModel, exists := object["model"]
	if !exists {
		return body, ""
	}
	var requested string
	if err := json.Unmarshal(rawModel, &requested); err != nil || requested == "" {
		return body, ""
	}
	canonical := canonicalize(requested)
	if canonical == requested {
		return body, ""
	}
	rewrittenModel, _ := json.Marshal(canonical)
	object["model"] = rewrittenModel
	rewritten, err := json.Marshal(object)
	if err != nil {
		return body, ""
	}
	return rewritten, requested
}
