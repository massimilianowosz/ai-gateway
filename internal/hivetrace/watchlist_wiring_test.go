package hivetrace

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
)

func TestDetector_WatchlistFindingsCarryTheOperatorsLabel(t *testing.T) {
	d := newDetector()
	d.setWatchlist(guardrail.NewWatchlistScanner([]guardrail.WatchTerm{
		{Label: "CEO", Pattern: "*assimilia*"},
	}))

	found := d.findings("ho scritto a massimiliano", OriginRequest, nil, 0)

	require.Len(t, found, 1)
	assert.Equal(t, KindWatchlist, found[0].Kind)
	assert.Equal(t, "CEO", found[0].Type, "the finding is named after the term, not the value")
	assert.Equal(t, []string{"massimiliano"}, found[0].Samples,
		"a declared term keeps its value even when finding_samples is off")
}

// An appliance with no terms configured must behave exactly as before.
func TestDetector_NoWatchlistIsInert(t *testing.T) {
	d := newDetector()
	found := d.findings("ho scritto a massimiliano", OriginRequest, nil, 0)
	for _, f := range found {
		assert.NotEqual(t, KindWatchlist, f.Kind)
	}
}

func TestDetector_PIITypesFollowTheOperatorsChoice(t *testing.T) {
	d := newDetector()
	text := "mi chiamo Marco Rossi, scrivimi a marco@example.com"

	types := func() []string {
		var out []string
		for _, f := range d.findings(text, OriginRequest, nil, 0) {
			out = append(out, f.Type)
		}
		return out
	}

	assert.Equal(t, []string{"EMAIL_ADDRESS"}, types(), "PERSON is off until asked for")

	d.setReported(map[string]bool{"PERSON": true, "EMAIL_ADDRESS": false})
	assert.Equal(t, []string{"PERSON"}, types())

	out, _ := d.redact(text, nil)
	assert.NotContains(t, out, "marco@example.com", "a hidden type is still redacted")
}

func TestDetector_RedactsWatchedTerms(t *testing.T) {
	d := newDetector()
	d.setWatchlist(guardrail.NewWatchlistScanner([]guardrail.WatchTerm{
		{Label: "CEO", Pattern: "*assimilia*"},
	}))

	out, replaced := d.redact("ho scritto a massimiliano", nil)

	assert.Equal(t, "ho scritto a [CEO]", out)
	assert.Contains(t, replaced, "CEO")
}
