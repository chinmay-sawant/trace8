package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chinmay-sawant/trace8/internal/store"
)

// writeJSON writes v as a 200 JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONStatus writes v as a JSON response with the given status.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes {"error": msg} with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSONStatus(w, status, map[string]string{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "service": "trace8"})
}

// handleAPINotFound answers /api/ requests no route matched, wrong
// methods included, with a JSON 404 instead of the static file
// server's plain-text 404.
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	writeJSON(w, map[string]string{"hello": name})
}

// idParam parses the required uint64 id query parameter. On a missing
// or malformed value it writes 400 and returns false.
func idParam(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	raw := r.URL.Query().Get("id")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return 0, false
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid id %q", raw))
		return 0, false
	}
	return id, true
}

// maxPutBytes caps the POST /api/db/put request body. It is a var so
// tests can lower it.
var maxPutBytes int64 = 64 << 20

// keywordBodyPrefix is the prefix the store uses for the tokens it
// indexes from text payloads. Users cannot claim it for their own
// keywords.
const keywordBodyPrefix = "body:"

// validateKeywords checks the request keywords the way the store will
// see them: at least one keyword, none blank after trimming, none
// containing a newline, and none using the reserved body: prefix. The
// store would otherwise surface these as internal errors.
func validateKeywords(keywords []string) error {
	if len(keywords) == 0 {
		return errors.New("at least one keyword required")
	}
	for _, kw := range keywords {
		trimmed := strings.TrimSpace(kw)
		if trimmed == "" {
			return errors.New("keywords must not be blank")
		}
		if strings.ContainsRune(kw, '\n') {
			return fmt.Errorf("keyword %q contains a newline", kw)
		}
		if strings.HasPrefix(strings.ToLower(trimmed), keywordBodyPrefix) {
			return fmt.Errorf("keyword %q uses the reserved %q prefix", kw, keywordBodyPrefix)
		}
	}
	return nil
}

func (s *Server) handleDBGet(w http.ResponseWriter, r *http.Request) {
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		writeError(w, http.StatusBadRequest, "tag required")
		return
	}
	records, err := s.store.Get(tag)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if records == nil {
		records = []store.Record{}
	}
	writeJSON(w, map[string]any{"records": records})
}

func (s *Server) handleDBRecord(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	rec, found := s.store.GetByID(id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no record %d", id))
		return
	}
	writeJSON(w, map[string]any{"record": rec})
}

// dbPutRequest is the POST /api/db/put body. Payload travels as a
// base64 string, which is what encoding/json does for []byte.
type dbPutRequest struct {
	Keywords []string `json:"keywords"`
	Kind     string   `json:"kind"`
	Payload  []byte   `json:"payload"`
}

func (s *Server) handleDBPut(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePutBody(w, r)
	if !ok {
		return
	}
	if err := validateKeywords(req.Keywords); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	kind := store.Kind(req.Kind)
	if kind != store.KindText && kind != store.KindBlob {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid kind %q", req.Kind))
		return
	}
	id, err := s.store.Put(req.Keywords, kind, req.Payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]uint64{"id": id})
}

// decodePutBody decodes exactly one JSON document from the POST body.
// It caps the body at maxPutBytes and rejects any trailing data after
// the document. On failure it writes 400 and returns false.
func decodePutBody(w http.ResponseWriter, r *http.Request) (dbPutRequest, bool) {
	var req dbPutRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPutBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, putBodyMessage(err, "invalid JSON"))
		return req, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, putBodyMessage(err, "unexpected data after JSON body"))
		return req, false
	}
	return req, true
}

// putBodyMessage prefers the oversized-body message when the byte
// limit tripped, and falls back to the caller's message otherwise.
func putBodyMessage(err error, fallback string) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return fmt.Sprintf("request body exceeds %d bytes", maxPutBytes)
	}
	return fallback
}

func (s *Server) handleDBDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	deleted, err := s.store.Delete(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"deleted": deleted})
}

func (s *Server) handleDBStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.Stats())
}

func (s *Server) handleDBCompact(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Compact(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
