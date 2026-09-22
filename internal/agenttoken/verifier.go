// Package agenttoken verifies the short-lived tokens the control plane issues
// to agents.
//
// The gateway holds only public keys, fetched from the control plane's JWKS
// endpoint. It can check that a token is genuine and it cannot mint one, which
// is the point of signing asymmetrically: compromising the data plane does not
// yield the ability to impersonate any agent in the fleet.
//
// A token names the virtual key it speaks for. Resolving it hands the rest of
// the gateway the same row it would have loaded for a raw key, so budget,
// model whitelist and spend attribution keep working exactly as they do —
// authentication changed, authorization did not.
package agenttoken

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// Issuer and Audience must match what the control plane mints. A token for
	// somewhere else is not a token for us.
	Issuer           = "ubiquum-control-plane"
	Audience         = "ubiquum-ai-gateway"
	SnapshotAudience = "ubiquum-ai-gateway-config"

	// How long a fetched key set is trusted before refetching. Short enough
	// that a rotation propagates on its own, long enough that the control
	// plane is not in the path of every request.
	keyCacheTTL = 5 * time.Minute
)

// Claims are the parts of an agent token the gateway acts on.
type Claims struct {
	AgentID   string
	AgentSlug string
	TeamID    string
	KeyHash   string
	CredID    string
	TokenID   string
	// Environment is asserted by the control plane when the credential was
	// minted, not read from the request. An environment a caller declares
	// about itself is not something a policy can be written against: an agent
	// avoiding production rules would simply claim to be staging.
	Environment string
}

// SnapshotClaims authenticate one immutable governance revision. The apply
// handler compares every field with the body before it mutates runtime state.
type SnapshotClaims struct {
	Contract           string
	EnvelopeVersion    int64
	TenantID           string
	AgentID            string
	AgentEnvironmentID string
	Environment        string
	Revision           int64
	SchemaVersion      int64
	CompilerVersion    int64
	ContentSHA256      string
}

// Verifier checks agent tokens against a cached JWKS.
type Verifier struct {
	jwksURL string
	client  *http.Client
	logger  *slog.Logger

	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey
	fetched  time.Time
	fetchErr error
}

// New returns a verifier, or nil when no JWKS URL is configured — the caller
// treats nil as "this gateway does not accept agent tokens", so an operator who
// has not set it up gets the previous behaviour rather than a broken one.
func New(jwksURL string, logger *slog.Logger) *Verifier {
	if strings.TrimSpace(jwksURL) == "" {
		return nil
	}
	return &Verifier{
		jwksURL: jwksURL,
		client:  &http.Client{Timeout: 5 * time.Second},
		logger:  logger,
		keys:    map[string]*rsa.PublicKey{},
	}
}

// LooksLikeToken reports whether a bearer value is worth trying as a JWT.
//
// Cheap and deliberately shallow: virtual keys are opaque strings with no dots,
// so anything with three segments is a candidate. Getting this wrong costs a
// failed parse, never an accepted token.
func LooksLikeToken(bearer string) bool {
	return strings.Count(bearer, ".") == 2
}

// Verify checks the signature, the issuer, the audience and the expiry.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	claims, err := v.verify(ctx, token, Audience)
	if err != nil {
		return nil, err
	}

	out := &Claims{
		AgentID:     stringClaim(claims, "agent_id"),
		AgentSlug:   stringClaim(claims, "agent_slug"),
		TeamID:      stringClaim(claims, "team_id"),
		KeyHash:     stringClaim(claims, "kh"),
		CredID:      stringClaim(claims, "cred_id"),
		TokenID:     stringClaim(claims, "jti"),
		Environment: stringClaim(claims, "env"),
	}
	// Without the key hash the token names no row, and a principal with no row
	// has no budget, no model list and nothing to attribute spend to.
	if out.KeyHash == "" {
		return nil, fmt.Errorf("token carries no key reference")
	}
	if out.AgentID == "" {
		return nil, fmt.Errorf("token names no agent")
	}
	return out, nil
}

// VerifySnapshot validates a signed control-plane envelope and returns only
// the claims the governance apply endpoint is allowed to act on.
func (v *Verifier) VerifySnapshot(ctx context.Context, token string) (*SnapshotClaims, error) {
	claims, err := v.verify(ctx, token, SnapshotAudience)
	if err != nil {
		return nil, err
	}
	out := &SnapshotClaims{
		Contract:           stringClaim(claims, "contract"),
		EnvelopeVersion:    integerClaim(claims, "envelope_version"),
		TenantID:           stringClaim(claims, "tenant_id"),
		AgentID:            stringClaim(claims, "agent_id"),
		AgentEnvironmentID: stringClaim(claims, "agent_environment_id"),
		Environment:        stringClaim(claims, "env"),
		Revision:           integerClaim(claims, "revision"),
		SchemaVersion:      integerClaim(claims, "schema_version"),
		CompilerVersion:    integerClaim(claims, "compiler_version"),
		ContentSHA256:      stringClaim(claims, "content_sha256"),
	}
	if out.Contract != "ubiquum.runtime_snapshot" || out.EnvelopeVersion != 1 ||
		out.TenantID == "" || out.AgentID == "" || out.AgentEnvironmentID == "" ||
		out.Revision < 1 || out.SchemaVersion < 1 || out.CompilerVersion < 1 || out.ContentSHA256 == "" {
		return nil, fmt.Errorf("snapshot envelope is missing required claims")
	}
	return out, nil
}

func (v *Verifier) verify(ctx context.Context, token, audience string) (jwt.MapClaims, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		// Only RS256. Accepting whatever the header asks for is how a token
		// signed with "none", or with the public key as an HMAC secret, gets
		// through.
		if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("token has no kid")
		}
		return v.keyFor(ctx, kid)
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithJSONNumber(),
	)
	if err != nil {
		return nil, err
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("unexpected claims type")
	}

	return claims, nil
}

func stringClaim(claims jwt.MapClaims, name string) string {
	if value, ok := claims[name].(string); ok {
		return value
	}
	return ""
}

func integerClaim(claims jwt.MapClaims, name string) int64 {
	switch value := claims[name].(type) {
	case json.Number:
		result, _ := value.Int64()
		return result
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func (v *Verifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	fresh := time.Since(v.fetched) < keyCacheTTL
	v.mu.RUnlock()
	if ok && fresh {
		return key, nil
	}

	if err := v.refresh(ctx); err != nil {
		// A key already cached still verifies while the control plane is away:
		// an unreachable issuer must not take down every agent in the fleet.
		if ok {
			return key, nil
		}
		return nil, err
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("no key %q in the published set", kid)
}

type jwksDocument struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (v *Verifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}

	var doc jwksDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}

	keys := map[string]*rsa.PublicKey{}
	for _, entry := range doc.Keys {
		if entry.Kty != "RSA" || entry.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(entry.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(entry.E)
		if err != nil {
			continue
		}
		keys[entry.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("jwks endpoint published no usable key")
	}

	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}
