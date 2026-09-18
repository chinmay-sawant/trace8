package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: keywords are joined with '\n' in the log, so an embedded
// newline must be rejected instead of silently splitting on reload.
func TestKeywordWithNewlineRejected(t *testing.T) {
	s := openStore(t, testPath(t))

	if _, err := s.Put([]string{"a\nb"}, KindBlob, []byte("x")); err == nil {
		t.Fatal("Put accepted a keyword containing a newline")
	}
	if _, err := normalizeKeywords([]string{"a\nb"}); err == nil {
		t.Fatal("normalizeKeywords accepted a keyword containing a newline")
	}
	if _, err := s.Put([]string{"a b"}, KindBlob, []byte("x")); err != nil {
		t.Fatalf("space inside a keyword should be allowed: %v", err)
	}
}

// Regression: a lost log handle (failed reopen after compaction) must
// surface as an error on writes instead of a nil pointer panic.
func TestWritesAfterLostLogHandle(t *testing.T) {
	s := openStore(t, testPath(t))

	s.mu.Lock()
	if err := s.file.Close(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.file = nil
	s.writeErr = fmt.Errorf("store: simulated lost log handle")
	s.mu.Unlock()

	if _, err := s.Put([]string{"k"}, KindBlob, []byte("v")); err == nil {
		t.Fatal("Put after lost handle: want error, got nil")
	}
	if _, err := s.Delete(1); err == nil {
		t.Fatal("Delete after lost handle: want error, got nil")
	}
	if err := s.Compact(); err == nil {
		t.Fatal("Compact after lost handle: want error, got nil")
	}
}

// Regression: a failed append must roll the log back to its logical end
// and latch a sticky error, so a later write cannot land behind torn
// bytes and a rerun of the store does not drop acknowledged records.
func TestFailedAppendRollsBackAndLatches(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("/dev/full not available: %v", err)
	}
	defer full.Close()

	path := testPath(t)
	s := openStore(t, path)
	if _, err := s.Put([]string{"kept"}, KindBlob, []byte("before")); err != nil {
		t.Fatalf("Put before the failure: %v", err)
	}

	s.mu.Lock()
	logFile := s.file
	s.file = full
	s.mu.Unlock()
	if err := logFile.Close(); err != nil {
		t.Fatalf("closing the real log handle: %v", err)
	}

	if _, err := s.Put([]string{"lost"}, KindBlob, []byte("x")); err == nil {
		t.Fatal("Put on /dev/full returned nil error")
	}
	s.mu.RLock()
	latched := s.writeErr
	s.mu.RUnlock()
	if latched == nil {
		t.Fatal("writeErr was not latched after the rollback truncate failed")
	}
	if _, err := s.Delete(1); err == nil {
		t.Fatal("Delete after a latched failure returned nil error")
	}
	if err := s.Compact(); err == nil {
		t.Fatal("Compact after a latched failure returned nil error")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2 := openStore(t, path)
	if _, ok := s2.GetByID(1); !ok {
		t.Fatal("the acknowledged record was lost after the failed append")
	}
	if st := s2.Stats(); st.Records != 1 || st.Deleted != 0 {
		t.Fatalf("Stats after reopen = %+v, want 1 record and no tombstones", st)
	}
}

// Regression: the tracked logical size must always equal the on-disk log
// size across puts, deletes, compacts, and reloads.
func TestLogicalSizeTracksDisk(t *testing.T) {
	path := testPath(t)
	check := func(s *Store, label string) {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: Stat: %v", label, err)
		}
		s.mu.RLock()
		got := s.size
		s.mu.RUnlock()
		if got != fi.Size() {
			t.Fatalf("%s: tracked size = %d, on-disk size = %d", label, got, fi.Size())
		}
	}

	s := openStore(t, path)
	var ids []uint64
	for i := 0; i < 6; i++ {
		id, err := s.Put([]string{"size", fmt.Sprintf("n%d", i)}, KindBlob, []byte(fmt.Sprintf("payload-%d", i)))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		ids = append(ids, id)
	}
	check(s, "after puts")
	for _, id := range ids[:3] {
		ok, err := s.Delete(id)
		if err != nil || !ok {
			t.Fatalf("Delete(%d) = %v, %v", id, ok, err)
		}
	}
	check(s, "after deletes")
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	check(s, "after compact")
	if _, err := s.Put([]string{"tail"}, KindBlob, []byte("z")); err != nil {
		t.Fatalf("Put after Compact: %v", err)
	}
	check(s, "after put following compact")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openStore(t, path)
	check(s2, "after reopen")
	if err := s2.Compact(); err != nil {
		t.Fatalf("second Compact: %v", err)
	}
	check(s2, "after second compact")
}

// Regression: dropping tombstones in Compact must not reset the id
// counter, so a reopen does not hand out an id that was already used.
func TestIDsSurviveCompactReopen(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	var ids []uint64
	for i := 0; i < 3; i++ {
		id, err := s.Put([]string{"hw"}, KindBlob, []byte(fmt.Sprintf("v%d", i)))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids[1:] {
		ok, err := s.Delete(id)
		if err != nil || !ok {
			t.Fatalf("Delete(%d) = %v, %v", id, ok, err)
		}
	}
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openStore(t, path)
	id, err := s2.Put([]string{"after"}, KindBlob, []byte("next"))
	if err != nil {
		t.Fatalf("Put after Compact and reopen: %v", err)
	}
	if id != 4 {
		t.Fatalf("id after Compact and reopen = %d, want 4", id)
	}
	if st := s2.Stats(); st.Records != 2 || st.Deleted != 0 {
		t.Fatalf("Stats after reopen = %+v, want 2 records and no tombstones", st)
	}
	for _, gone := range ids[1:] {
		if _, ok := s2.GetByID(gone); ok {
			t.Fatalf("deleted record %d reappeared after Compact", gone)
		}
	}
}

// Regression: Get must normalize its keyword the same way Put does.
func TestGetNormalizesKeyword(t *testing.T) {
	s := openStore(t, testPath(t))
	id, err := s.Put([]string{"Go"}, KindText, []byte("Queue theory"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, kw := range []string{"Go", "go", " GO ", "\tGo\n"} {
		recs, err := s.Get(kw)
		if err != nil || len(recs) != 1 || recs[0].ID != id {
			t.Fatalf("Get(%q) = %+v, %v, want record %d", kw, recs, err, id)
		}
	}
	for _, kw := range []string{"body:Queue", " BODY:QUEUE ", "Body:Queue"} {
		recs, err := s.Get(kw)
		if err != nil || len(recs) != 1 || recs[0].ID != id {
			t.Fatalf("Get(%q) = %+v, %v, want record %d", kw, recs, err, id)
		}
	}
	if recs, _ := s.Get("  "); len(recs) != 0 {
		t.Fatalf("Get(blank) = %d records, want none", len(recs))
	}
}

// Regression: a tiny garbage file must be rejected as not-a-database
// instead of being mistaken for a truncated record and emptied.
func TestShortGarbageRejected(t *testing.T) {
	for _, data := range []string{"a", "ab", "abc", "T8LG\x02"} {
		t.Run(fmt.Sprintf("%q", data), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			writeTestFile(t, path, []byte(data))

			s, err := Open(path)
			if err == nil {
				s.Close()
				t.Fatalf("Open accepted %q as a database", data)
			}
			if !strings.Contains(err.Error(), "not a trace8 database") {
				t.Fatalf("Open(%q) error = %v, want a not-a-database error", data, err)
			}
			got, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("ReadFile: %v", rerr)
			}
			if string(got) != data {
				t.Fatalf("rejected file changed on disk: %q -> %q", data, got)
			}
			if _, statErr := os.Stat(path + ".lock"); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("lock file survived a failed Open: %v", statErr)
			}
		})
	}
}

// Regression: a first read that is a real prefix of the magic is a
// truncated tail; the file is emptied so the next append starts clean.
func TestMagicPrefixTruncatedTail(t *testing.T) {
	for _, data := range []string{"T", "T8", "T8L", "T8LG", "T8LG\x01"} {
		t.Run(fmt.Sprintf("%q", data), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			writeTestFile(t, path, []byte(data))

			logs := captureLog(t)
			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open(%q): %v", data, err)
			}
			if !strings.Contains(logs.String(), "truncated tail") {
				t.Fatalf("Open(%q): no truncated-tail warning, got %q", data, logs.String())
			}
			if st := s.Stats(); st.Records != 0 {
				t.Fatalf("Open(%q): records = %d, want 0", data, st.Records)
			}
			s.mu.RLock()
			tracked := s.size
			s.mu.RUnlock()
			fi, ferr := os.Stat(path)
			if ferr != nil {
				t.Fatalf("Stat: %v", ferr)
			}
			if tracked != 0 || fi.Size() != 0 {
				t.Fatalf("Open(%q): tracked size %d, on-disk size %d, want both 0", data, tracked, fi.Size())
			}
			id, err := s.Put([]string{"after"}, KindBlob, []byte("x"))
			if err != nil || id != 1 {
				t.Fatalf("Put after truncated tail = %d, %v, want id 1", id, err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			s2 := openStore(t, path)
			if _, ok := s2.GetByID(1); !ok {
				t.Fatal("record 1 missing after reopening past the truncated tail")
			}
		})
	}
}

// Regression: Compact used to rename a 0600 temp file over the log,
// silently changing its permissions. The mode must survive.
func TestCompactPreservesPermissions(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)
	if _, err := s.Put([]string{"perm"}, KindBlob, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode after Compact = %v, want %v", got, os.FileMode(0o644))
	}
}

// Regression: a handcrafted keyword blob ending with a separator must
// not produce an empty-string keyword.
func TestDegenerateKeywordBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	writeTestFile(t, path, encodeRecord(1, kindBlobByte, codecRaw, 0, []byte("go\n"), []byte("x")))

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	rec, ok := s.GetByID(1)
	if !ok {
		t.Fatal("record 1 missing")
	}
	if len(rec.Keywords) != 1 || rec.Keywords[0] != "go" {
		t.Fatalf("keywords = %q, want exactly [go]", rec.Keywords)
	}
	if recs, _ := s.Get(""); len(recs) != 0 {
		t.Fatalf("Get(empty) = %d records, want none", len(recs))
	}
	if got := decodeKeywords([]byte("go\n")); len(got) != 1 || got[0] != "go" {
		t.Fatalf("decodeKeywords(%q) = %q, want [go]", "go\n", got)
	}
	if got := decodeKeywords([]byte("\n")); got != nil {
		t.Fatalf("decodeKeywords(%q) = %q, want nil", "\n", got)
	}
}
