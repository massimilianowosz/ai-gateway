package agenttoken

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type issuer struct {
	key *rsa.PrivateKey
	kid string
	srv *httptest.Server
	// hits counts JWKS fetches, so a test can prove the set is cached.
	hits int
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	iss := &issuer{key: key, kid: "test-kid"}
	iss.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		iss.hits++
		pub := key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": iss.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	}))
	t.Cleanup(iss.srv.Close)
	return iss
}

func (i *issuer) verifier() *Verifier { return New(i.srv.URL, discard()) }

func (i *issuer) sign(t *testing.T, claims jwt.MapClaims, method jwt.SigningMethod, key any) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = i.kid
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": Issuer, "aud": Audience, "sub": "agent:a1",
		"exp":      time.Now().Add(10 * time.Minute).Unix(),
		"iat":      time.Now().Unix(),
		"agent_id": "a1", "agent_slug": "sales-agent",
		"team_id": "team-1", "kh": "keyhash", "cred_id": "c1", "jti": "j1",
		"env": "production",
	}
}

func validSnapshotClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": Issuer, "aud": SnapshotAudience,
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
		"contract": "ubiquum.runtime_snapshot", "envelope_version": 1,
		"tenant_id": "t1", "agent_id": "a1", "agent_environment_id": "ae1",
		"env": "production", "revision": 7, "schema_version": 6,
		"compiler_version": 1, "content_sha256": "digest",
	}
}

func TestVerifySnapshot_AcceptsOnlyTheConfigAudienceAndRequiredClaims(t *testing.T) {
	iss := newIssuer(t)
	token := iss.sign(t, validSnapshotClaims(), jwt.SigningMethodRS256, iss.key)
	claims, err := iss.verifier().VerifySnapshot(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, int64(7), claims.Revision)
	assert.Equal(t, "ae1", claims.AgentEnvironmentID)
	assert.Equal(t, "digest", claims.ContentSHA256)

	wrongAudience := validSnapshotClaims()
	wrongAudience["aud"] = Audience
	_, err = iss.verifier().VerifySnapshot(context.Background(), iss.sign(t, wrongAudience, jwt.SigningMethodRS256, iss.key))
	assert.Error(t, err)

	missingDigest := validSnapshotClaims()
	delete(missingDigest, "content_sha256")
	_, err = iss.verifier().VerifySnapshot(context.Background(), iss.sign(t, missingDigest, jwt.SigningMethodRS256, iss.key))
	assert.ErrorContains(t, err, "missing required claims")
}

func TestVerify_AcceptsAGenuineToken(t *testing.T) {
	iss := newIssuer(t)
	token := iss.sign(t, validClaims(), jwt.SigningMethodRS256, iss.key)

	claims, err := iss.verifier().Verify(context.Background(), token)

	require.NoError(t, err)
	assert.Equal(t, "keyhash", claims.KeyHash)
	assert.Equal(t, "a1", claims.AgentID)
	assert.Equal(t, "team-1", claims.TeamID)
	// Asserted at issuance, so a policy can be written against it.
	assert.Equal(t, "production", claims.Environment)
}

func TestVerify_EnvironmentComesFromTheTokenNotTheRequest(t *testing.T) {
	// A credential minted without one carries none, and nothing the caller
	// sends can supply it.
	iss := newIssuer(t)
	claims := validClaims()
	delete(claims, "env")
	token := iss.sign(t, claims, jwt.SigningMethodRS256, iss.key)

	verified, err := iss.verifier().Verify(context.Background(), token)

	require.NoError(t, err)
	assert.Empty(t, verified.Environment)
}

func TestVerify_RejectsTheNoneAlgorithm(t *testing.T) {
	// The classic JWT hole: a token that asks to be trusted without a signature.
	iss := newIssuer(t)
	token := iss.sign(t, validClaims(), jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType)

	_, err := iss.verifier().Verify(context.Background(), token)

	assert.Error(t, err)
}

func TestVerify_RejectsAnHMACTokenSignedWithThePublicKey(t *testing.T) {
	// Algorithm confusion: the attacker knows the public key because it is
	// published, and uses it as an HMAC secret. Only accepting RS256 stops it.
	iss := newIssuer(t)
	pubDER, err := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims()).
		SignedString([]byte(iss.srv.URL))
	require.NoError(t, err)

	_, err = iss.verifier().Verify(context.Background(), pubDER)

	assert.Error(t, err)
}

func TestVerify_RejectsAnotherIssuersSignature(t *testing.T) {
	iss := newIssuer(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	token := iss.sign(t, validClaims(), jwt.SigningMethodRS256, other)

	_, err = iss.verifier().Verify(context.Background(), token)

	assert.Error(t, err)
}

func TestVerify_RejectsAnExpiredToken(t *testing.T) {
	iss := newIssuer(t)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Minute).Unix()
	token := iss.sign(t, claims, jwt.SigningMethodRS256, iss.key)

	_, err := iss.verifier().Verify(context.Background(), token)

	assert.Error(t, err)
}

func TestVerify_RejectsATokenForSomewhereElse(t *testing.T) {
	iss := newIssuer(t)
	for _, claim := range []string{"iss", "aud"} {
		claims := validClaims()
		claims[claim] = "somewhere-else"
		token := iss.sign(t, claims, jwt.SigningMethodRS256, iss.key)

		_, err := iss.verifier().Verify(context.Background(), token)
		assert.Error(t, err, "a token whose %s is not ours must not be accepted", claim)
	}
}

func TestVerify_RejectsATokenThatNamesNoKey(t *testing.T) {
	// Without a key reference the principal has no budget, no model list and
	// nothing to attribute spend to.
	iss := newIssuer(t)
	claims := validClaims()
	delete(claims, "kh")
	token := iss.sign(t, claims, jwt.SigningMethodRS256, iss.key)

	_, err := iss.verifier().Verify(context.Background(), token)

	assert.ErrorContains(t, err, "no key reference")
}

func TestVerify_CachesTheKeySet(t *testing.T) {
	iss := newIssuer(t)
	v := iss.verifier()
	token := iss.sign(t, validClaims(), jwt.SigningMethodRS256, iss.key)

	for i := 0; i < 3; i++ {
		_, err := v.Verify(context.Background(), token)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, iss.hits, "the control plane must not be in the path of every request")
}

func TestVerify_KeepsWorkingWhileTheControlPlaneIsAway(t *testing.T) {
	// An unreachable issuer must not take down every agent in the fleet.
	iss := newIssuer(t)
	v := iss.verifier()
	token := iss.sign(t, validClaims(), jwt.SigningMethodRS256, iss.key)
	require.NoError(t, func() error { _, err := v.Verify(context.Background(), token); return err }())

	iss.srv.Close()
	v.mu.Lock()
	v.fetched = time.Time{} // force a refresh attempt
	v.mu.Unlock()

	_, err := v.Verify(context.Background(), token)
	assert.NoError(t, err)
}

func TestNew_WithoutAnEndpointIsDisabled(t *testing.T) {
	assert.Nil(t, New("", discard()))
	assert.Nil(t, New("   ", discard()))
}

func TestLooksLikeToken(t *testing.T) {
	assert.True(t, LooksLikeToken("aaa.bbb.ccc"))
	assert.False(t, LooksLikeToken("sk-ubq-abcdef"), "a virtual key must not be parsed as a token")
	assert.False(t, LooksLikeToken("aaa.bbb"))
}
