package proxy

import (
	"fmt"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
)

// budgetMessage explains which half of the funding gate refused the call. The
// two are not interchangeable: telling a key that has spent its budget it was
// never given one sends the operator looking for the wrong thing.
func budgetMessage(status auth.BudgetStatus, model string) string {
	if status == auth.BudgetExhausted {
		return fmt.Sprintf("this API key has spent its budget and %q is billed to the gateway: "+
			"raise the budget, or use a model served by an OAuth pass-through deployment", model)
	}
	return fmt.Sprintf("this API key has no budget and %q is billed to the gateway: "+
		"set a budget, or use a model served by an OAuth pass-through deployment", model)
}

// writeBudgetError refuses a call the caller cannot pay for.
//
// 402, not 429: an exhausted budget and a rate limit are different problems
// with different fixes (raise the budget vs. slow down), and a client that
// only checks the status code — the way most retry logic does — needs to
// tell them apart without parsing the body.
func writeBudgetError(w http.ResponseWriter, status auth.BudgetStatus, model string) {
	writeError(w, http.StatusPaymentRequired, "budget_exceeded", budgetMessage(status, model))
}
