package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/console"
)

// Server is the main HTTP server for the gateway.
type Server struct {
	cfg         *config.Config
	mux         *http.ServeMux
	logger      *slog.Logger
	OpenAPISpec []byte // embedded OpenAPI spec for /docs
	Console     *console.Manager
}

// New creates a new Server with the given configuration.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		mux:    http.NewServeMux(),
		logger: logger,
	}
	return s
}

// Mux returns the server's ServeMux for route registration.
func (s *Server) Mux() *http.ServeMux {
	return s.mux
}

// Run starts the HTTP server and blocks until shutdown signal is received.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.cfg.Server.Port),
		Handler:           s.mux,
		ReadHeaderTimeout: 30 * time.Second, // timeout only for reading headers
		// No WriteTimeout: it is measured from the start of the request and
		// covers the whole response write, so on a streamed completion it is a
		// hard cap on how long the model may take. A 5-minute cap cut long
		// agentic turns mid-stream once the conversation grew large enough,
		// and the client saw a truncated response rather than an error it
		// could act on. Slow-client protection stays with ReadHeaderTimeout
		// and IdleTimeout; how long a generation may run belongs to the
		// per-provider client and the request context, which can tell a
		// streaming turn from a stalled socket.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	// Shutdown signal handling
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("server starting", "port", s.cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case sig := <-shutdownCh:
		s.logger.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
		s.logger.Info("context cancelled")
	}

	// Graceful shutdown with timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s.logger.Info("shutting down gracefully")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown error: %w", err)
	}

	s.logger.Info("server stopped")
	return nil
}
