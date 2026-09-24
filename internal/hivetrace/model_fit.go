package hivetrace

// PriceLookup returns a model's list output price in USD per million tokens.
type PriceLookup func(model string) (float64, bool)

// Model tiers, ranked by list output price. Within a vendor's lineup price
// follows capability, and it is the one ranking every model in the catalog
// has without an external benchmark.
const (
	TierTop   = "top"
	TierMid   = "mid"
	TierLight = "light"

	topTierFrom   = 20.0 // Opus, GPT-5.x flagship
	lightTierUpTo = 8.0  // Haiku, mini, flash
)

// Fit flags.
const (
	FitOversized  = "oversized"  // a top-tier model on a trivial session
	FitUndersized = "undersized" // a light model on a hard one
)

func tierFor(outputPerMillion float64) string {
	switch {
	case outputPerMillion >= topTierFrom:
		return TierTop
	case outputPerMillion < lightTierUpTo:
		return TierLight
	default:
		return TierMid
	}
}

// assessModelFit ranks the session's primary model and flags a mismatch with
// the measured difficulty. A model without a price gets neither: no tier is
// better than a guessed one.
func assessModelFit(s *SessionSummary, prices PriceLookup) {
	s.ModelTier, s.ModelFit = "", ""
	if prices == nil || s.PrimaryModel == "" {
		return
	}
	price, ok := prices(s.PrimaryModel)
	if !ok || price <= 0 {
		return
	}
	s.ModelTier = tierFor(price)
	switch {
	case s.ModelTier == TierTop && s.Difficulty == 1:
		s.ModelFit = FitOversized
	case s.ModelTier == TierLight && s.Difficulty >= 4:
		s.ModelFit = FitUndersized
	}
}
