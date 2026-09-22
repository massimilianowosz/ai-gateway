package hivestate

import "testing"

// breakEvenTurns is the number a decision can be checked against instead of
// just accepted or rejected. Anchored against the same input the guard uses:
// solving transition + steady*(R-1) = passthrough*R for R by hand must match
// what EvaluateGuard reports.
func TestEvaluateGuard_ReportsTheBreakEvenItActuallyComputed(t *testing.T) {
	in := GuardInput{
		Prefix:               PrefixState{FrozenTokens: 60000},
		OriginalTokens:       61000,
		PredictedTokens:      12000,
		ReusableAfterRewrite: 10000,
		CacheReadRatio:       0.1,
		CacheWriteRatio:      1.25,
		Horizon:              50, // large enough that Skip itself doesn't gate the check
	}
	d := EvaluateGuard(in)

	passthrough := 0.1*60000 + (61000.0 - 60000)
	transition := (12000.0 - 10000) + 1.25*10000
	steady := 0.1*10000 + (12000.0 - 10000)
	want := (transition - steady) / (passthrough - steady)

	if diff := d.BreakEvenTurns - want; diff > 0.01 || diff < -0.01 {
		t.Fatalf("BreakEvenTurns = %.4f, want %.4f", d.BreakEvenTurns, want)
	}
	if d.BreakEvenTurns <= 0 {
		t.Fatal("a rewrite this much cheaper per turn must have a positive, finite break-even")
	}
}

// A rewrite whose steady-state cost is not even lower than passthrough never
// pays off — no horizon, however generous, changes that arithmetic. Reporting
// a break-even turn count here would imply a horizon exists that helps, when
// none does.
func TestEvaluateGuard_NoBreakEvenWhenSteadyStateNeverWins(t *testing.T) {
	in := GuardInput{
		Prefix:          PrefixState{FrozenTokens: 60000},
		OriginalTokens:  61000,
		PredictedTokens: 55000, // barely shrinks, and nothing of it is reusable
		CacheReadRatio:  0.1,
		CacheWriteRatio: 1.25,
		Horizon:         1000,
	}
	d := EvaluateGuard(in)
	if d.BreakEvenTurns != 0 {
		t.Fatalf("BreakEvenTurns = %v, want 0 when steady-state cost never improves on passthrough", d.BreakEvenTurns)
	}
}

// A rewrite always loses on the turn it happens: it throws away a warm cache
// to build a smaller one. Judged a single turn at a time it can never be
// chosen, however much smaller the prompt becomes — which is why rewrites that
// cut a prompt to a fifth were still being refused.
func TestEvaluateGuard_SingleTurnNeverPaysForACacheableRewrite(t *testing.T) {
	in := GuardInput{
		Prefix:               PrefixState{FrozenTokens: 60000},
		OriginalTokens:       61000,
		PredictedTokens:      12000,
		ReusableAfterRewrite: 10000,
		CacheReadRatio:       0.1,
		CacheWriteRatio:      1.25,
		Horizon:              1,
	}

	d := EvaluateGuard(in)
	if !d.Skip {
		t.Fatalf("a one-turn horizon accepted a rewrite: %+v", d)
	}

	in.Horizon = 10
	d = EvaluateGuard(in)
	if d.Skip {
		t.Fatalf("over ten turns the rewrite should win: passthrough=%.0f rewrite=%.0f",
			d.PassthroughCost, d.RewriteCost)
	}
	if d.Reason != "rewrite_cheaper" {
		t.Errorf("reason = %q, want rewrite_cheaper", d.Reason)
	}
}

// Without a cacheable state region the rewrite is billed in full every turn,
// so no horizon should rescue it.
func TestEvaluateGuard_UncacheableRewriteLosesAtAnyHorizon(t *testing.T) {
	in := GuardInput{
		Prefix:          PrefixState{FrozenTokens: 60000},
		OriginalTokens:  61000,
		PredictedTokens: 30000,
		CacheReadRatio:  0.1,
		CacheWriteRatio: 1.25,
		Horizon:         50,
	}

	if d := EvaluateGuard(in); !d.Skip {
		t.Fatalf("an uncached rewrite at 30k beat passthrough at 6.1k: %+v", d)
	}
}

// The horizon must not turn the guard off: a rewrite that saves nothing is
// still refused.
func TestEvaluateGuard_NoShrinkIsStillRefused(t *testing.T) {
	in := GuardInput{
		Prefix:               PrefixState{FrozenTokens: 60000},
		OriginalTokens:       61000,
		PredictedTokens:      61000,
		ReusableAfterRewrite: 55000,
		CacheReadRatio:       0.1,
		CacheWriteRatio:      1.25,
		Horizon:              20,
	}

	if d := EvaluateGuard(in); !d.Skip {
		t.Fatalf("a rewrite that shrinks nothing was accepted: %+v", d)
	}
}

// The old entry point must keep behaving as it did: one turn, nothing reusable.
func TestEvaluatePrefixCacheGuard_StillJudgesASingleTurn(t *testing.T) {
	ps := PrefixState{FrozenTokens: 180000}
	d := EvaluatePrefixCacheGuard(ps, 200000, 42000, DefaultAnthropicCacheReadRatio, DefaultGuardMargin)
	if !d.Skip {
		t.Fatalf("expected the single-turn verdict to be unchanged: %+v", d)
	}
}
