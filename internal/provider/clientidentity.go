package provider

import (
	"context"
	"strings"
)

type clientIdentityKey struct{}

// ClientIdentity is what the calling agent says it is: the product name and
// version from its User-Agent, plus its entrypoint. Anthropic routes an OAuth
// subscription request on this identity, so relaying the caller's own version
// keeps the gateway from pinning one that ages out of support.
type ClientIdentity struct {
	Product string
	Version string
	App     string
}

// WithClientIdentity attaches the caller's self-reported identity.
func WithClientIdentity(ctx context.Context, id ClientIdentity) context.Context {
	if id.Product == "" && id.Version == "" && id.App == "" {
		return ctx
	}
	return context.WithValue(ctx, clientIdentityKey{}, id)
}

// ClientIdentityFrom returns the caller's self-reported identity, if any.
func ClientIdentityFrom(ctx context.Context) (ClientIdentity, bool) {
	id, ok := ctx.Value(clientIdentityKey{}).(ClientIdentity)
	return id, ok
}

// ParseClientIdentity reads a User-Agent of the "product/version (comment)"
// form that every agent CLI uses. Anything it cannot parse yields a zero value
// rather than a guess, so callers fall back to their own default.
func ParseClientIdentity(userAgent, app string) ClientIdentity {
	id := ClientIdentity{App: app}

	field := strings.Fields(userAgent)
	if len(field) == 0 {
		return id
	}
	product, version, ok := strings.Cut(field[0], "/")
	if !ok {
		return id
	}
	id.Product = product
	// A version is dotted digits; refusing anything else keeps a spoofed or
	// exotic User-Agent from reaching a provider's billing header verbatim.
	for _, part := range strings.Split(version, ".") {
		if part == "" {
			return id
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return id
			}
		}
	}
	id.Version = version
	return id
}
