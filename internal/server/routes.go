package server

import (
	"net/http"
)

// routes wires the API before the static catch-all. Go matches the
// more specific patterns first, so / only serves files and the /api/
// fallback only sees requests no route matched, wrong methods
// included.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", s.handleHealth)
	mux.HandleFunc("GET /api/hello", s.handleHello)
	mux.HandleFunc("GET /api/db/get", s.handleDBGet)
	mux.HandleFunc("GET /api/db/record", s.handleDBRecord)
	mux.HandleFunc("POST /api/db/put", s.handleDBPut)
	mux.HandleFunc("DELETE /api/db/record", s.handleDBDelete)
	mux.HandleFunc("GET /api/db/stats", s.handleDBStats)
	mux.HandleFunc("POST /api/db/compact", s.handleDBCompact)
	mux.HandleFunc("/api/", s.handleAPINotFound)
	mux.Handle("/", http.FileServer(http.Dir(s.cfg.FrontendDir)))
	return mux
}
