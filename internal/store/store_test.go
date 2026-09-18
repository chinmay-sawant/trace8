package store

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenEmpty(t *testing.T) {
	s := openStore(t, testPath(t))

	if got := s.Stats(); got.Records != 0 || got.Keywords != 0 || got.Deleted != 0 || got.FileBytes != 0 {
		t.Fatalf("Stats() = %+v, want zero", got)
	}
	recs, err := s.Get("missing")
	if err != nil || len(recs) != 0 {
		t.Fatalf("Get(missing) = %v, %v, want no records", recs, err)
	}
	if _, ok := s.GetByID(1); ok {
		t.Fatal("GetByID(1) found a record in an empty store")
	}
}

func TestPutGetReload(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	payload := []byte("the queue is empty")
	id, err := s.Put([]string{"Go", " db "}, KindText, payload)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}

	rec, ok := s.GetByID(id)
	if !ok {
		t.Fatalf("GetByID(%d): not found", id)
	}
	if rec.ID != id || rec.Kind != KindText || rec.Size != int64(len(payload)) {
		t.Fatalf("record = %+v", rec)
	}
	if !bytes.Equal(rec.Payload, payload) {
		t.Fatalf("payload = %q, want %q", rec.Payload, payload)
	}
	if !slices.Equal(rec.Keywords, []string{"go", "db", "body:the", "body:queue", "body:is", "body:empty"}) {
		t.Fatalf("keywords = %v", rec.Keywords)
	}

	recs, err := s.Get("db")
	if err != nil || len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("Get(db) = %+v, %v, want record %d", recs, err, id)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openStore(t, path)
	recs, err = s2.Get("go")
	if err != nil || len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("Get(go) after reload = %+v, %v, want record %d", recs, err, id)
	}
	if !bytes.Equal(recs[0].Payload, payload) {
		t.Fatalf("reloaded payload = %q, want %q", recs[0].Payload, payload)
	}
	if recs, _ := s2.Get("body:queue"); len(recs) != 1 {
		t.Fatalf("Get(body:queue) after reload = %d records, want 1", len(recs))
	}
}

func TestGetReturnsStructCopy(t *testing.T) {
	s := openStore(t, testPath(t))
	id, err := s.Put([]string{"copy"}, KindBlob, []byte("data"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	recs, err := s.Get("copy")
	if err != nil || len(recs) != 1 {
		t.Fatalf("Get(copy) = %v, %v", recs, err)
	}
	recs[0].ID = 999
	recs[0].Kind = KindText
	recs[0].Size = 0

	again, err := s.Get("copy")
	if err != nil || len(again) != 1 {
		t.Fatalf("Get(copy) = %v, %v", again, err)
	}
	if again[0].ID != id || again[0].Kind != KindBlob || again[0].Size != 4 {
		t.Fatalf("mutating a result changed the store: %+v", again[0])
	}
}

func TestNormalizeKeywords(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
		err  error
	}{
		{"lowercase and trim", []string{"Go", " go ", "DB"}, []string{"go", "db"}, nil},
		{"empty rejected", []string{""}, nil, ErrNoKeywords},
		{"whitespace only", []string{"  ", "\t"}, nil, ErrNoKeywords},
		{"comma stays inside one keyword", []string{"Notes, 2026"}, []string{"notes, 2026"}, nil},
		{"duplicates collapse", []string{"a", "A", "a", " b "}, []string{"a", "b"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeKeywords(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("keywords = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTokenizer(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"sentence", "runs are slow when the queue is empty",
			[]string{"runs", "are", "slow", "when", "the", "queue", "is", "empty"}},
		{"punctuation and digits", "hello,world! 2026", []string{"hello", "world", "2026"}},
		{"short tokens dropped", "a I x 1 go", []string{"go"}},
		{"unicode letters", "café au lait", []string{"café", "au", "lait"}},
		{"one rune but two bytes", "é oui", []string{"oui"}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenize(tt.in); !slices.Equal(got, tt.want) {
				t.Fatalf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTextBodyTokens(t *testing.T) {
	s := openStore(t, testPath(t))
	id, err := s.Put([]string{"Notes"}, KindText, []byte("The queue is empty and a test"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, kw := range []string{"notes", "body:the", "body:queue", "body:is", "body:test"} {
		recs, err := s.Get(kw)
		if err != nil || len(recs) != 1 || recs[0].ID != id {
			t.Fatalf("Get(%q) = %+v, %v, want record %d", kw, recs, err, id)
		}
	}
	// "a" is one rune, so it is not indexed as a body token.
	if recs, _ := s.Get("body:a"); len(recs) != 0 {
		t.Fatalf("Get(body:a) = %+v, want none", recs)
	}

	// Blob payloads keep their bytes and are never tokenized.
	blobID, err := s.Put([]string{"binary"}, KindBlob, []byte("queue"))
	if err != nil {
		t.Fatalf("Put blob: %v", err)
	}
	recs, err := s.Get("body:queue")
	if err != nil || len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("Get(body:queue) = %+v, %v, want only record %d", recs, err, id)
	}
	rec, ok := s.GetByID(blobID)
	if !ok || string(rec.Payload) != "queue" {
		t.Fatalf("blob record = %+v, %v", rec, ok)
	}
}

func TestPutValidation(t *testing.T) {
	s := openStore(t, testPath(t))

	if _, err := s.Put(nil, KindText, []byte("x")); !errors.Is(err, ErrNoKeywords) {
		t.Fatalf("Put(nil) err = %v, want ErrNoKeywords", err)
	}
	if _, err := s.Put([]string{" "}, KindBlob, nil); !errors.Is(err, ErrNoKeywords) {
		t.Fatalf("Put(blank) err = %v, want ErrNoKeywords", err)
	}
	_, err := s.Put([]string{"k"}, Kind("json"), []byte("x"))
	if err == nil || err.Error() != `unknown kind "json"` {
		t.Fatalf("Put(bad kind) err = %v, want unknown kind", err)
	}
}

func TestStats(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	if _, err := s.Put([]string{"go", "db"}, KindText, []byte("queue of size two")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Put([]string{"db"}, KindBlob, []byte("binary")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	st := s.Stats()
	if st.Records != 2 || st.Deleted != 0 {
		t.Fatalf("Stats = %+v, want 2 records and no tombstones", st)
	}
	// go, db, body:queue, body:of, body:size, body:two
	if st.Keywords != 6 {
		t.Fatalf("keyword count = %d, want 6", st.Keywords)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.FileBytes != fi.Size() {
		t.Fatalf("FileBytes = %d, want %d", st.FileBytes, fi.Size())
	}
}

func TestDelete(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	id, err := s.Put([]string{"a"}, KindBlob, []byte("one"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	other, err := s.Put([]string{"b"}, KindBlob, []byte("two"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	ok, err := s.Delete(id)
	if err != nil || !ok {
		t.Fatalf("Delete(%d) = %v, %v, want true", id, ok, err)
	}
	if _, found := s.GetByID(id); found {
		t.Fatal("deleted record is still visible")
	}
	if recs, _ := s.Get("a"); len(recs) != 0 {
		t.Fatalf("Get(a) = %+v, want none", recs)
	}
	if ok, err := s.Delete(id); err != nil || ok {
		t.Fatalf("second Delete(%d) = %v, %v, want false", id, ok, err)
	}
	if st := s.Stats(); st.Records != 1 || st.Deleted != 1 {
		t.Fatalf("Stats = %+v, want 1 record and 1 tombstone", st)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openStore(t, path)
	if _, found := s2.GetByID(id); found {
		t.Fatal("deleted record came back after reload")
	}
	if st := s2.Stats(); st.Records != 1 || st.Deleted != 1 {
		t.Fatalf("reloaded Stats = %+v, want 1 record and 1 tombstone", st)
	}
	if recs, _ := s2.Get("b"); len(recs) != 1 || recs[0].ID != other {
		t.Fatalf("Get(b) = %+v, want record %d", recs, other)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := s2.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("compact did not shrink the log: %d -> %d bytes", before.Size(), after.Size())
	}
	if st := s2.Stats(); st.Deleted != 0 {
		t.Fatalf("Stats after Compact = %+v, want no tombstones", st)
	}

	// Appends must keep working after the handle is reopened.
	id3, err := s2.Put([]string{"c"}, KindBlob, []byte("three"))
	if err != nil {
		t.Fatalf("Put after Compact: %v", err)
	}
	if id3 != other+1 {
		t.Fatalf("id after Compact = %d, want %d", id3, other+1)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s3 := openStore(t, path)
	for _, want := range []uint64{other, id3} {
		if _, found := s3.GetByID(want); !found {
			t.Fatalf("record %d missing after Compact and reload", want)
		}
	}
	if _, found := s3.GetByID(id); found {
		t.Fatal("deleted record came back after Compact")
	}
}

func TestCompression(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	text := []byte(strings.Repeat("the queue is empty and the store is quick\n", 200))
	textID, err := s.Put([]string{"notes"}, KindText, text)
	if err != nil {
		t.Fatalf("Put text: %v", err)
	}
	random := make([]byte, 10*1024)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	blobID, err := s.Put([]string{"blob"}, KindBlob, random)
	if err != nil {
		t.Fatalf("Put blob: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	first := decodeHeader(data[:headerSize])
	if first.codec != codecGzip {
		t.Fatalf("text codec = %d, want gzip", first.codec)
	}
	if first.payLen >= uint64(len(text)) {
		t.Fatalf("gzip stored %d bytes, raw text is %d", first.payLen, len(text))
	}
	off := int64(headerSize) + int64(first.keyLen) + int64(first.payLen)
	second := decodeHeader(data[off : off+headerSize])
	if second.codec != codecRaw {
		t.Fatalf("random blob codec = %d, want raw", second.codec)
	}

	for _, tc := range []struct {
		id   uint64
		want []byte
	}{
		{textID, text},
		{blobID, random},
	} {
		rec, ok := s.GetByID(tc.id)
		if !ok {
			t.Fatalf("GetByID(%d): not found", tc.id)
		}
		if !bytes.Equal(rec.Payload, tc.want) {
			t.Fatalf("record %d payload does not round-trip: %d bytes vs %d", tc.id, len(rec.Payload), len(tc.want))
		}
		if rec.Size != int64(len(tc.want)) {
			t.Fatalf("record %d size = %d, want %d", tc.id, rec.Size, len(tc.want))
		}
	}
}

func TestLockExcludesSecondOpen(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	_, err := Open(path)
	if err == nil {
		t.Fatal("second Open succeeded while the first is open")
	}
	if !strings.Contains(err.Error(), "is locked by another process") || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want the locked-by-another-process message", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	path := testPath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.Put([]string{"x"}, KindBlob, nil); err == nil {
		t.Fatal("Put after Close succeeded")
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file still present after Close: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := openStore(t, testPath(t))

	const (
		workers = 8
		ops     = 40
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				id, err := s.Put([]string{"race", fmt.Sprintf("worker%d", w)}, KindBlob, []byte("payload"))
				if err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				if _, ok := s.GetByID(id); !ok {
					t.Errorf("GetByID(%d): not found", id)
					return
				}
				if recs, err := s.Get("race"); err != nil || len(recs) == 0 {
					t.Errorf("Get(race) = %d records, %v", len(recs), err)
					return
				}
				_ = s.Stats()
				if i%8 == 0 {
					if ok, err := s.Delete(id); err != nil || !ok {
						t.Errorf("Delete(%d) = %v, %v", id, ok, err)
						return
					}
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 4; i++ {
			if err := s.Compact(); err != nil {
				t.Errorf("Compact: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	if st := s.Stats(); st.Records == 0 {
		t.Fatal("concurrent run left no live records")
	}
}

func TestConcurrentClose(t *testing.T) {
	s, err := Open(testPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Close %d: %v", i, err)
		}
	}
}
