package proxy

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
)

func TestSetFailureHeaders_NamesTheProvidersThatRefused(t *testing.T) {
	w := httptest.NewRecorder()
	err := &router.RouteError{
		Model:           "claude-code-sonnet-5",
		Attempts:        1,
		FailedProviders: []string{"anthropic"},
		Err:             &provider.UpstreamError{StatusCode: 429, Message: "rate limit"},
	}

	setFailureHeaders(w, err)

	assert.Equal(t, "anthropic", w.Header().Get(headerUbiquumFailed))
}

func TestSetFailureHeaders_LeavesGatewayErrorsUnattributed(t *testing.T) {
	w := httptest.NewRecorder()
	setFailureHeaders(w, assert.AnError)
	assert.Empty(t, w.Header().Get(headerUbiquumFailed))
}
