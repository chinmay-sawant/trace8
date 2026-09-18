package server

import (
	"context"
	"net/http"
	"time"
)

// Config holds the server settings. Addr is a listen address like
// ":8080". FrontendDir is the static directory served at /.
type Config struct {
	Addr        string
	FrontendDir string
}

// DefaultConfig returns the standard local settings.
func DefaultConfig() Config {
	return Config{Addr: ":8080", FrontendDir: "frontend"}
}

// Server owns the HTTP lifecycle. Routes and handlers live in
// routes.go and handlers.go; this file only starts and stops.
type Server struct {
	cfg  Config
	http *http.Server
}

// New builds a Server with defaults filled in for empty fields.
func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = DefaultConfig().Addr
	}
	if cfg.FrontendDir == "" {
		cfg.FrontendDir = DefaultConfig().FrontendDir
	}
	s := &Server{cfg: cfg}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handler exposes the mux for tests without opening a port.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Run serves until ctx ends, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
		if err := <-errCh; err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
