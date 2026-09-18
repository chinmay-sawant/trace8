package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/chinmay-sawant/trace8/internal/store"
)

// Config holds the server settings. Addr is a listen address like
// ":8080". FrontendDir is the static directory served at /. DBPath is
// the append log opened for the lifetime of the server.
type Config struct {
	Addr        string
	FrontendDir string
	DBPath      string
}

// DefaultConfig returns the standard local settings.
func DefaultConfig() Config {
	return Config{Addr: ":8080", FrontendDir: "frontend", DBPath: "trace8.db"}
}

// Server owns the HTTP lifecycle. Routes and handlers live in
// routes.go and handlers.go; this file only starts and stops.
type Server struct {
	cfg   Config
	http  *http.Server
	store *store.Store

	closeOnce sync.Once
	closeErr  error
}

// New builds a Server with defaults filled in for empty fields and
// opens the store at cfg.DBPath. Run closes the store.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = DefaultConfig().Addr
	}
	if cfg.FrontendDir == "" {
		cfg.FrontendDir = DefaultConfig().FrontendDir
	}
	if cfg.DBPath == "" {
		cfg.DBPath = DefaultConfig().DBPath
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// Handler exposes the mux for tests without opening a port.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Run serves until ctx ends, then shuts down gracefully and closes the
// store exactly once. A failed ListenAndServe also closes the store
// before Run returns.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.ListenAndServe() }()
	var serveErr error
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
		serveErr = <-errCh
	case serveErr = <-errCh:
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, s.closeStore())
}

// closeStore releases the store and its lock file, at most once.
func (s *Server) closeStore() error {
	s.closeOnce.Do(func() { s.closeErr = s.store.Close() })
	return s.closeErr
}
