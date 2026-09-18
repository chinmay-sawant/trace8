package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(Config{Addr: ":0", FrontendDir: dir})
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
