package middleware

import "net/http"

// Middleware is a function that wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares to a handler in reverse order,
// so the first middleware in the list is the outermost.
func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}
