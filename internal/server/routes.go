package server

import (
	"net/http"
)

// routes wires the API before the static catch-all. Go matches the
// more specific /api/ patterns first, so / only serves files.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", s.handleHealth)
	mux.HandleFunc("GET /api/hello", s.handleHello)
	mux.Handle("/", http.FileServer(http.Dir(s.cfg.FrontendDir)))
	return mux
}
