package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chinmay-sawant/trace8/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Addr: ":0", FrontendDir: dir, DBPath: filepath.Join(t.TempDir(), "trace8.db")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// request drives the handler without opening a port.
func request(t *testing.T, s *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// dbRecord mirrors the store record as it travels over JSON.
type dbRecord struct {
	ID       uint64   `json:"id"`
	Kind     string   `json:"kind"`
	Keywords []string `json:"keywords"`
	Size     int64    `json:"size"`
	Payload  []byte   `json:"payload"`
}

func putRecord(t *testing.T, s *Server, keywords []string, kind string, payload []byte) uint64 {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"keywords": keywords,
		"kind":     kind,
		"payload":  payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, s, http.MethodPost, "/api/db/put", bytes.NewReader(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("put status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	var out struct {
		ID uint64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.ID
}

func TestHealth(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["service"] != "trace8" {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestHelloDefaultsToWorld(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/hello", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"hello":"world"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestHelloNamed(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/hello?name=gopher", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"hello":"gopher"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestStaticIndex(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestDBPutGetRoundTrip(t *testing.T) {
	s := testServer(t)
	payload := make([]byte, 64)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	id := putRecord(t, s, []string{"Go", " db "}, "text", payload)

	w := request(t, s, http.MethodGet, "/api/db/get?tag=go", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var out struct {
		Records []dbRecord `json:"records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(out.Records))
	}
	got := out.Records[0]
	if got.ID != id || got.Kind != "text" || got.Size != int64(len(payload)) {
		t.Fatalf("record = %+v, want id %d kind text size %d", got, id, len(payload))
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("payload = %x, want %x", got.Payload, payload)
	}
	if !slices.Contains(got.Keywords, "go") || !slices.Contains(got.Keywords, "db") {
		t.Fatalf("keywords = %v, want go and db", got.Keywords)
	}
}

func TestDBGetRequiresTag(t *testing.T) {
	s := testServer(t)
	w := request(t, s, http.MethodGet, "/api/db/get", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != "tag required" {
		t.Fatalf("body = %s, want tag required", w.Body.String())
	}
}

func TestDBPutValidation(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"bad JSON", `{"keywords":[`},
		{"bad base64", `{"keywords":["go"],"kind":"text","payload":"!!!"}`},
		{"empty keywords", `{"keywords":[],"kind":"text","payload":""}`},
		{"blank keyword", `{"keywords":["  "],"kind":"text","payload":""}`},
		{"bad kind", `{"keywords":["go"],"kind":"csv","payload":""}`},
		{"missing kind", `{"keywords":["go"],"payload":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, s, http.MethodPost, "/api/db/put", strings.NewReader(tc.body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			var out map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["error"] == "" {
				t.Fatalf("body = %s, want an error field", w.Body.String())
			}
		})
	}
}

func TestDBPutRejectsTrailingData(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"garbage", `{"keywords":["go"],"kind":"text","payload":""} trailing`},
		{"second document", `{"keywords":["go"],"kind":"text","payload":""}{"keywords":["go"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, s, http.MethodPost, "/api/db/put", strings.NewReader(tc.body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			var out map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["error"] == "" {
				t.Fatalf("body = %s, want an error field", w.Body.String())
			}
		})
	}
}

func TestDBPutAllowsTrailingWhitespace(t *testing.T) {
	s := testServer(t)
	body := "{\"keywords\":[\"go\"],\"kind\":\"text\",\"payload\":\"\"}\n  \t"
	w := request(t, s, http.MethodPost, "/api/db/put", strings.NewReader(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
}

func TestDBPutRejectsBadKeywords(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"newline", `{"keywords":["go\nbad"],"kind":"text","payload":""}`, "newline"},
		{"reserved prefix", `{"keywords":["body:tok"],"kind":"text","payload":""}`, "reserved"},
		{"reserved prefix mixed case and spaces", `{"keywords":[" Body:Tok "],"kind":"text","payload":""}`, "reserved"},
		{"blank among keywords", `{"keywords":["go","  "],"kind":"text","payload":""}`, "blank"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, s, http.MethodPost, "/api/db/put", strings.NewReader(tc.body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			var out map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out["error"], tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", out["error"], tc.wantErr)
			}
		})
	}
}

func TestDBPutRejectsOversizedBody(t *testing.T) {
	old := maxPutBytes
	maxPutBytes = 16
	t.Cleanup(func() { maxPutBytes = old })

	s := testServer(t)
	body := `{"keywords":["go"],"kind":"text","payload":""}`
	w := request(t, s, http.MethodPost, "/api/db/put", strings.NewReader(body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out["error"], "exceeds") {
		t.Fatalf("error = %q, want an exceeds message", out["error"])
	}
}

func TestAPINotFoundIsJSON(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		method string
		target string
	}{
		{http.MethodPost, "/api/db/get"},
		{http.MethodGet, "/api/nope"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			w := request(t, s, tc.method, tc.target, nil)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type = %q, want application/json", ct)
			}
			var out map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["error"] != "not found" {
				t.Fatalf("body = %s, want error not found", w.Body.String())
			}
		})
	}
}

func TestServerTimeouts(t *testing.T) {
	s := testServer(t)
	if got := s.http.ReadHeaderTimeout; got != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", got)
	}
	if got := s.http.ReadTimeout; got != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", got)
	}
	if got := s.http.WriteTimeout; got != 60*time.Second {
		t.Errorf("WriteTimeout = %v, want 60s", got)
	}
	if got := s.http.IdleTimeout; got != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", got)
	}
}

func TestDBRecordLookup(t *testing.T) {
	s := testServer(t)
	payload := []byte{0x00, 0x01, 0xfe}
	id := putRecord(t, s, []string{"perf"}, "blob", payload)

	w := request(t, s, http.MethodGet, fmt.Sprintf("/api/db/record?id=%d", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var out struct {
		Record dbRecord `json:"record"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Record.ID != id || out.Record.Kind != "blob" || !bytes.Equal(out.Record.Payload, payload) {
		t.Fatalf("record = %+v", out.Record)
	}

	w = request(t, s, http.MethodGet, fmt.Sprintf("/api/db/record?id=%d", id+1), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	var errOut map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errOut); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("no record %d", id+1); errOut["error"] != want {
		t.Fatalf("error = %q, want %q", errOut["error"], want)
	}
}

func TestDBDelete(t *testing.T) {
	s := testServer(t)
	id := putRecord(t, s, []string{"go"}, "text", []byte("gone soon"))

	w := request(t, s, http.MethodDelete, fmt.Sprintf("/api/db/record?id=%d", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"deleted":true`) {
		t.Fatalf("body = %s, want deleted true", w.Body.String())
	}
	w = request(t, s, http.MethodDelete, fmt.Sprintf("/api/db/record?id=%d", id), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted":false`) {
		t.Fatalf("second delete = %d %s, want 200 deleted false", w.Code, w.Body.String())
	}
	w = request(t, s, http.MethodGet, fmt.Sprintf("/api/db/record?id=%d", id), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", w.Code)
	}
}

func TestDBBadID(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/db/record"},
		{http.MethodGet, "/api/db/record?id=abc"},
		{http.MethodGet, "/api/db/record?id=-1"},
		{http.MethodDelete, "/api/db/record"},
		{http.MethodDelete, "/api/db/record?id=1.5"},
	}
	for _, tc := range cases {
		w := request(t, s, tc.method, tc.target, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: status = %d, want 400 (body %s)", tc.method, tc.target, w.Code, w.Body.String())
		}
		var out map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out["error"] == "" {
			t.Fatalf("%s %s: body = %s, want an error field", tc.method, tc.target, w.Body.String())
		}
	}
}

func TestDBStats(t *testing.T) {
	s := testServer(t)
	w := request(t, s, http.MethodGet, "/api/db/stats", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var fresh store.Stats
	if err := json.Unmarshal(w.Body.Bytes(), &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Records != 0 || fresh.Keywords != 0 {
		t.Fatalf("fresh stats = %+v, want zero counts", fresh)
	}

	// Blob payloads are not tokenized, so the keyword count is exactly
	// the two tags.
	putRecord(t, s, []string{"go", "db"}, "blob", []byte("hi"))
	w = request(t, s, http.MethodGet, "/api/db/stats", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got store.Stats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Records != 1 || got.Keywords != 2 {
		t.Fatalf("stats = %+v, want 1 record and 2 keywords", got)
	}
}

func TestDBCompact(t *testing.T) {
	s := testServer(t)
	id := putRecord(t, s, []string{"go"}, "text", []byte("keep me"))

	w := request(t, s, http.MethodPost, "/api/db/compact", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("body = %s, want ok true", w.Body.String())
	}
	w = request(t, s, http.MethodGet, fmt.Sprintf("/api/db/record?id=%d", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("record after compact: status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

func TestRunShutdownReleasesStore(t *testing.T) {
	frontend := t.TempDir()
	if err := os.WriteFile(filepath.Join(frontend, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "trace8.db")
	s, err := New(Config{Addr: ":0", FrontendDir: frontend, DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store after shutdown: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStartupFailureReleasesStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "trace8.db")
	s, err := New(Config{Addr: "bad-addr", FrontendDir: t.TempDir(), DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run with a bad address: want error, got nil")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store after failed Run: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}
