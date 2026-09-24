package proxy

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
)

const (
	headerContentType        = "Content-Type"
	headerUbiquumProvider    = "X-Ubiquum-Provider"
	headerUbiquumBillingMode = "X-Ubiquum-Billing-Mode"
	headerUbiquumFailed      = "X-Ubiquum-Failed"
	contentTypeJSON          = "application/json"
)

// setDeploymentHeaders reports which deployment served a request.
//
// The billing mode travels with the provider because a zero cost means two
// different things: nothing was charged, or the caller's own subscription paid
// and the appliance was never going to charge. Only the deployment knows which.
func setDeploymentHeaders(w http.ResponseWriter, dep *provider.Deployment) {
	if dep == nil {
		return
	}
	w.Header().Set(headerUbiquumProvider, dep.ProviderName)
	if dep.BillingMode != "" {
		w.Header().Set(headerUbiquumBillingMode, string(dep.BillingMode))
	}
}

// setFailureHeaders names the providers that refused a request no deployment
// could serve. Without it a failed turn is recorded with no provider, and a
// 429 cannot be told apart from one the gateway raised itself.
func setFailureHeaders(w http.ResponseWriter, err error) {
	var re *router.RouteError
	if errors.As(err, &re) && len(re.FailedProviders) > 0 {
		w.Header().Set(headerUbiquumFailed, strings.Join(re.FailedProviders, ","))
	}
}
