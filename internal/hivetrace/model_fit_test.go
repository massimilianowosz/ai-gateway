package hivetrace

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var testPrices = PriceLookup(func(model string) (float64, bool) {
	p, ok := map[string]float64{"opus": 25, "sonnet": 10, "haiku": 5}[model]
	return p, ok
})

func TestAssessModelFit(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		difficulty int
		tier, fit  string
	}{
		{"flagship on a trivial task", "opus", 1, TierTop, FitOversized},
		{"flagship on real work", "opus", 3, TierTop, ""},
		{"light model on a hard task", "haiku", 4, TierLight, FitUndersized},
		{"light model on a simple task", "haiku", 2, TierLight, ""},
		{"mid tier is never flagged", "sonnet", 1, TierMid, ""},
		{"nothing served", "opus", 0, TierTop, ""},
		{"no price, no verdict", "jev", 1, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &SessionSummary{PrimaryModel: c.model, Difficulty: c.difficulty}
			assessModelFit(s, testPrices)
			assert.Equal(t, c.tier, s.ModelTier)
			assert.Equal(t, c.fit, s.ModelFit)
		})
	}
}

// Claude Code sends side calls to a light model; the session is judged by the
// model that did the work, and failed turns did no work.
func TestSummarize_PrimaryModelIsTheOneThatServed(t *testing.T) {
	now := time.Now()
	s := Summarize("s", []Event{
		{CreatedAt: now, Model: "haiku", Status: 200},
		{CreatedAt: now, Model: "opus", Status: 200},
		{CreatedAt: now, Model: "opus", Status: 200},
		{CreatedAt: now, Model: "sonnet", Status: 429},
		{CreatedAt: now, Model: "sonnet", Status: 429},
		{CreatedAt: now, Model: "sonnet", Status: 429},
	})
	assert.Equal(t, "opus", s.PrimaryModel)
}
