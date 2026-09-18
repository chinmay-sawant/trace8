package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/chinmay-sawant/trace8/internal/store"
)

// newRemoteStoreServer serves the frozen /api/db endpoints on top of a real
// store, the way a running trace8 --server does.
func newRemoteStoreServer(t *testing.T) *httptest.Server {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/db/get", func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Query().Get("tag")
		if tag == "boom" {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "boom"})
			return
		}
		records, err := s.Get(tag)
		if err != nil {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if records == nil {
			records = []store.Record{}
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"records": records})
	})
	mux.HandleFunc("/api/db/put", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Keywords []string   `json:"keywords"`
			Kind     store.Kind `json:"kind"`
			Payload  []byte     `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		id, err := s.Put(req.Keywords, req.Kind, req.Payload)
		if err != nil {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeTestJSON(w, http.StatusCreated, map[string]any{"id": id})
	})
	mux.HandleFunc("/api/db/record", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
			return
		}
		if r.Method == http.MethodDelete {
			ok, err := s.Delete(id)
			if err != nil {
				writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeTestJSON(w, http.StatusOK, map[string]any{"deleted": ok})
			return
		}
		rec, ok := s.GetByID(id)
		if !ok {
			writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "no record"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"record": rec})
	})
	mux.HandleFunc("/api/db/stats", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, s.Stats())
	})
	mux.HandleFunc("/api/db/compact", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Compact(); err != nil {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		_ = s.Close()
	})
	return srv
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestDBRemoteRoundTrip(t *testing.T) {
	srv := newRemoteStoreServer(t)
	base := srv.URL

	code, out, errOut := captureRun(t, "db", "put", "go", "--text", "hello remote", "--remote", base)
	if code != 0 {
		t.Fatalf("remote put exit = %d, stderr = %q", code, errOut)
	}
	if out != "1\n" {
		t.Fatalf("remote put stdout = %q, want %q", out, "1\n")
	}

	want := "1 text 12 [go]\nhello remote\n"
	code, out, errOut = captureRun(t, "db", "get", "go", "--remote", base)
	if code != 0 || out != want {
		t.Fatalf("remote get exit = %d stdout = %q stderr = %q, want %q", code, out, errOut, want)
	}

	code, out, errOut = captureRun(t, "db", "get", "--id", "1", "--remote", base)
	if code != 0 || out != want {
		t.Fatalf("remote get by id exit = %d stdout = %q stderr = %q, want %q", code, out, errOut, want)
	}

	code, out, errOut = captureRun(t, "db", "get", "--id", "42", "--remote", base)
	if code != 1 || out != "" || errOut != "trace8 db: no record 42\n" {
		t.Fatalf("remote missing id exit = %d stdout = %q stderr = %q", code, out, errOut)
	}

	code, out, errOut = captureRun(t, "db", "stats", "--remote", base+"/")
	if code != 0 {
		t.Fatalf("remote stats exit = %d, stderr = %q", code, errOut)
	}
	if !strings.HasPrefix(out, "records: 1\nkeywords: ") || !strings.Contains(out, "\ndeleted: 0\nfile: "+base+" (") ||
		!strings.HasSuffix(out, " bytes)\n") || strings.Contains(out, "(0 bytes)") {
		t.Fatalf("remote stats stdout = %q", out)
	}

	code, out, errOut = captureRun(t, "db", "compact", "--remote", base)
	if code != 0 || out != "compacted trace8.db\n" {
		t.Fatalf("remote compact exit = %d stdout = %q stderr = %q", code, out, errOut)
	}

	code, out, errOut = captureRun(t, "db", "del", "99", "--remote", base)
	if code != 1 || out != "" || errOut != "trace8 db: no record 99\n" {
		t.Fatalf("remote missing del exit = %d stdout = %q stderr = %q", code, out, errOut)
	}

	code, out, errOut = captureRun(t, "db", "del", "1", "--remote", base)
	if code != 0 || out != "deleted 1\n" {
		t.Fatalf("remote del exit = %d stdout = %q stderr = %q", code, out, errOut)
	}

	code, out, errOut = captureRun(t, "db", "get", "go", "--remote", base)
	if code != 1 || out != "" || errOut != "trace8 db: no records for keyword \"go\"\n" {
		t.Fatalf("remote get after del exit = %d stdout = %q stderr = %q", code, out, errOut)
	}
}

func TestDBRemoteStatsFileLabel(t *testing.T) {
	srv := newRemoteStoreServer(t)
	base := srv.URL

	// The server's byte count is labelled with the remote base, not the
	// unused local --db path, and a trailing slash is trimmed.
	code, out, errOut := captureRun(t, "db", "stats", "--remote", base+"/")
	if code != 0 {
		t.Fatalf("remote stats exit = %d, stderr = %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("remote stats stdout = %q, want 4 lines", out)
	}
	want := fmt.Sprintf("file: %s (0 bytes)", base)
	if lines[3] != want {
		t.Fatalf("remote stats file line = %q, want %q", lines[3], want)
	}
	if strings.Contains(out, "trace8.db") {
		t.Fatalf("remote stats stdout = %q, must not name the local database", out)
	}
}

func TestDBRemoteRaw(t *testing.T) {
	srv := newRemoteStoreServer(t)
	payload := []byte{0x00, 0xff, 'a', 'b'}
	src := writeTempFile(t, "payload.bin", payload)

	code, _, errOut := captureRun(t, "db", "put", "bin", "--file", src, "--remote", srv.URL)
	if code != 0 {
		t.Fatalf("remote put exit = %d, stderr = %q", code, errOut)
	}
	code, out, errOut := captureRun(t, "db", "get", "--id", "1", "--raw", "--remote", srv.URL)
	if code != 0 {
		t.Fatalf("remote raw exit = %d, stderr = %q", code, errOut)
	}
	if !bytes.Equal([]byte(out), payload) {
		t.Fatalf("remote raw bytes = %v, want %v", []byte(out), payload)
	}
}

func TestDBRemoteError(t *testing.T) {
	srv := newRemoteStoreServer(t)
	code, out, errOut := captureRun(t, "db", "get", "boom", "--remote", srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if errOut != "trace8 db: boom\n" {
		t.Fatalf("stderr = %q, want server error text", errOut)
	}
}
