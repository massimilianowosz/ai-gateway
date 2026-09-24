package guardrail

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func matchFor(matches []DetectorMatch, name string) (DetectorMatch, bool) {
	for _, m := range matches {
		if m.Name == name {
			return m, true
		}
	}
	return DetectorMatch{}, false
}

// Scan stops at the first hit because blocking needs one reason; Findings has
// to return the whole inventory for traffic analysis.
func TestSecretsScanner_FindingsReportsEveryDetector(t *testing.T) {
	s := NewSecretsScanner()

	matches := s.Findings("AKIAIOSFODNN7EXAMPLE and ghp_abcdefghijklmnopqrstuvwxyz0123456789")

	_, hasAWS := matchFor(matches, "AWS_ACCESS_KEY")
	_, hasGitHub := matchFor(matches, "GITHUB_TOKEN")
	assert.True(t, hasAWS)
	assert.True(t, hasGitHub)
}

func TestSecretsScanner_FindingsCountsOccurrences(t *testing.T) {
	s := NewSecretsScanner()

	matches := s.Findings("AKIAIOSFODNN7EXAMPLE AKIAIOSFODNN7EXAMPLF")

	m, ok := matchFor(matches, "AWS_ACCESS_KEY")
	require.True(t, ok)
	assert.Equal(t, 2, m.Count)
}

func TestSecretsScanner_FindingsOnCleanText(t *testing.T) {
	s := NewSecretsScanner()
	assert.Empty(t, s.Findings("refactor the retry loop"))
	assert.Empty(t, s.Findings(""))
}

func TestSecretsScanner_Redact(t *testing.T) {
	s := NewSecretsScanner()

	out, replaced := s.Redact("deploy with AKIAIOSFODNN7EXAMPLE now")

	assert.Equal(t, "deploy with [AWS_ACCESS_KEY] now", out)
	assert.Equal(t, []string{"AWS_ACCESS_KEY"}, replaced)
}

func TestSecretsScanner_RedactHandlesRepeatsAndMultipleKinds(t *testing.T) {
	s := NewSecretsScanner()

	out, replaced := s.Redact("AKIAIOSFODNN7EXAMPLE then ghp_abcdefghijklmnopqrstuvwxyz0123456789 then AKIAIOSFODNN7EXAMPLF")

	assert.NotContains(t, out, "AKIAIOSFODNN7EXAMPLE")
	assert.NotContains(t, out, "AKIAIOSFODNN7EXAMPLF")
	assert.NotContains(t, out, "ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	assert.Contains(t, replaced, "AWS_ACCESS_KEY")
	assert.Contains(t, replaced, "GITHUB_TOKEN")
}

func TestSecretsScanner_RedactLeavesCleanTextAlone(t *testing.T) {
	s := NewSecretsScanner()

	out, replaced := s.Redact("nothing sensitive here")

	assert.Equal(t, "nothing sensitive here", out)
	assert.Empty(t, replaced)
}

func TestSecretsScanner_ShapedCredentials(t *testing.T) {
	s := NewSecretsScanner()
	cases := []struct {
		name, text, want string
	}{
		{"jwt", "cookie eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk here", "JWT"},
		{"postgres dsn", "postgres://svc_reporting:Tr0ub4dor%263@db-prod-reader.internal:5432/analytics", "CREDENTIAL_URL"},
		{"mongo srv", "mongodb+srv://appuser:QmFzZTY0aXNOb3RFbmNyeXB0aW9u@cluster0.ab1cd.mongodb.net/orders", "CREDENTIAL_URL"},
		{"curl -u", "curl -u admin:S3cur3-Adm1n-P4ss! https://grafana.internal", "BASIC_AUTH"},
		{"auth header", "Authorization: Basic YWRtaW46UzNjdXIzLUFkbTFu", "AUTH_HEADER"},
		{"prose", "ssh as deploy and the password is hunter2-correct-horse-staging. Do not change it", "PASSWORD"},
		{"italian prose", "la password è Estate2024! per il wifi", "PASSWORD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := matchFor(s.Findings(c.text), c.want)
			assert.True(t, ok, "expected %s in %q", c.want, c.text)
		})
	}
}

// Templates and docs carry the shape of a credential without one; flagging
// them is what made the findings list noise.
func TestSecretsScanner_ShapedCredentialPlaceholders(t *testing.T) {
	s := NewSecretsScanner()
	for _, text := range []string{
		"postgres://user:${DB_PASSWORD}@localhost:5432/app",
		"https://user:password@example.com",
		"mysql://root:<pass>@db/app",
		"redis://default:xxxxxxxx@cache:6379",
		"postgres://localhost:5432/app",
		`curl -u "$USER:$TOKEN" https://api`,
		"the password is incorrect",
		"password is required",
		"eyJhbGciOiJub25lIn0.eyJzdWIiOiIxMjM0In0.",
	} {
		assert.Empty(t, s.Findings(text), text)
	}
}

func TestSecretsScanner_PasswordSampleStopsAtTheSentence(t *testing.T) {
	s := NewSecretsScanner()
	m, ok := matchFor(s.FindingsWithSamples("the password is hunter2-horse. Next", 5), "PASSWORD")
	require.True(t, ok)
	assert.Equal(t, []string{"password is hunter2-horse"}, m.Samples)
}

// These shapes also appear in compose files and code an agent reads while
// working, so blocking on them would stop legitimate sessions.
func TestSecretsScanner_ShapedCredentialsDoNotBlock(t *testing.T) {
	s := NewSecretsScanner()
	res, err := s.Scan(context.Background(), "", "DATABASE_URL=postgres://app:Tr0ub4dor%263@db:5432/app")
	require.NoError(t, err)
	assert.False(t, res.Blocked)

	out, _ := s.Redact("DATABASE_URL=postgres://app:Tr0ub4dor%263@db:5432/app")
	assert.Equal(t, "DATABASE_URL=[CREDENTIAL_URL]/app", out)
}

func TestPIIScanner_FindingsReportsEveryEntity(t *testing.T) {
	s := NewPIIScanner()

	matches := s.Findings("write to ada@example.com or ada2@example.com", nil)

	m, ok := matchFor(matches, "EMAIL_ADDRESS")
	require.True(t, ok)
	assert.Equal(t, 2, m.Count)
}

func TestPIIScanner_FindingsHonoursTenantSelection(t *testing.T) {
	s := NewPIIScanner()
	only := []string{"PHONE_NUMBER"}

	matches := s.Findings("ada@example.com", &only)

	_, ok := matchFor(matches, "EMAIL_ADDRESS")
	assert.False(t, ok, "a detector the tenant switched off must not be reported")
}

// An always-on detector runs regardless of the portal selection, and Findings
// must keep that behaviour rather than inventing its own.
func TestPIIScanner_FindingsKeepsAlwaysOnDetectors(t *testing.T) {
	s := NewPIIScanner()
	none := []string{}

	withSelection := s.Findings("RSSMRA85T10A562S", &none)
	withoutSelection := s.Findings("RSSMRA85T10A562S", nil)

	_, onWithout := matchFor(withoutSelection, "CODICE_FISCALE")
	require.True(t, onWithout, "the detector must fire with no selection at all")
	_, onWith := matchFor(withSelection, "CODICE_FISCALE")
	assert.True(t, onWith, "an always-on detector survives an empty selection")
}

func TestPIIScanner_FindingsOnCleanText(t *testing.T) {
	s := NewPIIScanner()
	assert.Empty(t, s.Findings("", nil))
}
