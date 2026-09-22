package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Authentication decides neither half of the funding gate: both depend on the
// model, which it cannot see.
func TestAuthenticate_UnfundedKeyPassesAuthAndIsDecidedPerModel(t *testing.T) {
	key := activeKey()
	key.Budget = 0
	key.Spend = 0
	m := NewMiddleware("sk-master", &mockStore{key: key})

	called := false
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal(t, BudgetUnfunded, CheckBudget(r.Context(), true), "a paid model must still be refused")
		assert.Equal(t, BudgetOK, CheckBudget(r.Context(), false), "a flat model must be allowed")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	assert.True(t, called)
}

// An exhausted key reaches the handler, where the model decides. Blocking it in
// authentication took flat models and read-only routes down with it: a key that
// had run out could not even read its own spend to find out why.
func TestAuthenticate_ExhaustedKeyIsDecidedPerModel(t *testing.T) {
	key := activeKey()
	key.Budget = 10
	key.Spend = 10
	m := NewMiddleware("sk-master", &mockStore{key: key})

	called := false
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal(t, BudgetExhausted, CheckBudget(r.Context(), true), "a paid model must be refused")
		assert.Equal(t, BudgetOK, CheckBudget(r.Context(), false), "a flat model costs nothing to serve")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.True(t, called, "authentication must not decide the funding gate")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// Spend short of the budget is not exhaustion.
func TestCheckBudget_PartiallySpentKeyMayStillCall(t *testing.T) {
	key := activeKey()
	key.Budget = 10
	key.Spend = 9.99
	m := NewMiddleware("sk-master", &mockStore{key: key})

	called := false
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal(t, BudgetOK, CheckBudget(r.Context(), true))
	}))
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	assert.True(t, called)
}
