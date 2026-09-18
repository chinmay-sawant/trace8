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
	"sync/atomic"
	"testing"
	"time"
)

// TestTruncatedTailKeepsCompleteRecords cuts a second record at every
// meaningful boundary and checks that Open keeps the complete records,
// drops only the partial one, and leaves the log in a state a later
// append can grow from. The append plus reopen step is the regression
// guard: if the partial bytes stay on disk, the next append lands behind
// them and every later Open sees corruption.
func TestTruncatedTailKeepsCompleteRecords(t *testing.T) {
	first := encodeRecord(1, kindTextByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("hello"))
	second := encodeRecord(2, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("world"))

	tests := []struct {
		name    string
		tail    []byte
		warning bool
	}{
		{"zero bytes into the header", second[:0], false},
		{"4 bytes, magic cut", second[:4], true},
		{"8 bytes, id cut", second[:8], true},
		{"16 bytes, key length cut", second[:16], true},
		{"20 bytes, payload length cut", second[:20], true},
		{"27 bytes, one short of a header", second[:27], true},
		{"28 bytes, header only", second[:28], true},
		{"mid keywords", second[:29], true},
		{"all keywords, payload missing", second[:30], true},
		{"mid payload", second[:33], true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			writeTestFile(t, path, append(append([]byte{}, first...), tt.tail...))

			logs := captureLog(t)
			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got := s.Stats().Records; got != 1 {
				t.Fatalf("records = %d, want only the complete record", got)
			}
			if recs, err := s.Get("go"); err != nil || len(recs) != 1 || recs[0].ID != 1 {
				t.Fatalf("Get(go) = %+v, %v, want record 1", recs, err)
			}

			id, err := s.Put([]string{"after"}, KindBlob, []byte("tail"))
			if err != nil {
				t.Fatalf("Put after truncated tail: %v", err)
			}
			if id != 2 {
				t.Fatalf("id after truncated tail = %d, want 2 (the partial record must not claim an id)", id)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			s2, err := Open(path)
			if err != nil {
				t.Fatalf("reopen after appending past a truncated tail: %v", err)
			}
			defer s2.Close()
			for _, want := range []uint64{1, 2} {
				if _, ok := s2.GetByID(want); !ok {
					t.Fatalf("record %d missing after reopen", want)
				}
			}
			rec, ok := s2.GetByID(2)
			if !ok || string(rec.Payload) != "tail" || !slices.Equal(rec.Keywords, []string{"after"}) {
				t.Fatalf("record 2 after reopen = %+v, %v", rec, ok)
			}

			got := logs.String()
			if tt.warning && !strings.Contains(got, "truncated tail") {
				t.Fatalf("no truncated-tail warning logged, got %q", got)
			}
			if !tt.warning && strings.Contains(got, "truncated tail") {
				t.Fatalf("clean end logged a truncated-tail warning: %q", got)
			}
		})
	}
}

// TestMidFileCorruptionReleasesLock corrupts the middle record of three
// and checks that Open fails with an error naming the offset, returns no
// store, and removes the lock file so the next Open is not blocked.
func TestMidFileCorruptionReleasesLock(t *testing.T) {
	first := encodeRecord(1, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("one"))
	middle := encodeRecord(2, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("two"))
	last := encodeRecord(3, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("three"))

	tests := []struct {
		name   string
		want   string
		mutate func([]byte)
	}{
		{"bad magic", "bad magic", func(b []byte) { copy(b[:4], "XXXX") }},
		{"bad version", "format version 9", func(b []byte) { b[4] = 9 }},
		{"bad kind", "bad kind", func(b []byte) { b[5] = 'z' }},
		{"bad codec", "bad codec 7", func(b []byte) { b[6] = 7 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			rec := append([]byte{}, middle...)
			tt.mutate(rec)
			data := append(append(append([]byte{}, first...), rec...), last...)
			writeTestFile(t, path, data)

			s, err := Open(path)
			if err == nil {
				s.Close()
				t.Fatal("Open succeeded on a log with a corrupt middle record")
			}
			if s != nil {
				t.Fatalf("Open returned a store alongside error %v", err)
			}
			for _, want := range []string{"corrupt record", tt.want, fmt.Sprintf("offset %d", len(first))} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want it to mention %q", err, want)
				}
			}
			if _, statErr := os.Stat(path + ".lock"); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("lock file survived a failed Open: %v", statErr)
			}
		})
	}
}

func TestEdgeFileShapes(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		path := testPath(t)
		s := openStore(t, path)
		if got := s.Stats(); got.Records != 0 || got.FileBytes != 0 {
			t.Fatalf("Stats = %+v, want an empty store", got)
		}
		id, err := s.Put([]string{"k"}, KindBlob, []byte("v"))
		if err != nil || id != 1 {
			t.Fatalf("Put = %d, %v, want id 1", id, err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		writeTestFile(t, path, nil)

		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()
		if got := s.Stats(); got.Records != 0 || got.Deleted != 0 {
			t.Fatalf("Stats = %+v, want an empty store", got)
		}
		id, err := s.Put([]string{"k"}, KindBlob, []byte("v"))
		if err != nil || id != 1 {
			t.Fatalf("Put = %d, %v, want id 1", id, err)
		}
	})

	t.Run("header only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		full := encodeRecord(1, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("hello"))
		writeTestFile(t, path, full[:headerSize])

		logs := captureLog(t)
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got := s.Stats().Records; got != 0 {
			t.Fatalf("records = %d, want 0", got)
		}
		if !strings.Contains(logs.String(), "truncated tail") {
			t.Fatalf("no truncated-tail warning logged, got %q", logs.String())
		}
		id, err := s.Put([]string{"next"}, KindBlob, nil)
		if err != nil || id != 1 {
			t.Fatalf("Put after header-only file = %d, %v, want id 1", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		s2 := openStore(t, path)
		if _, ok := s2.GetByID(1); !ok {
			t.Fatal("record 1 missing after reopening a header-only log")
		}
	})

	t.Run("tombstone only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		writeTestFile(t, path, encodeRecord(1, kindTextByte, codecRaw, flagTombstone, nil, nil))

		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()
		if got := s.Stats(); got.Records != 0 || got.Deleted != 1 {
			t.Fatalf("Stats = %+v, want 0 records and 1 tombstone", got)
		}
		id, err := s.Put([]string{"next"}, KindBlob, []byte("x"))
		if err != nil || id != 2 {
			t.Fatalf("Put = %d, %v, want id 2 (ids are never reused)", id, err)
		}
	})

	t.Run("version 2 first record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		future := encodeRecord(1, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("x"))
		future[4] = 2
		writeTestFile(t, path, future)

		s, err := Open(path)
		if err == nil {
			s.Close()
			t.Fatal("Open accepted a format version 2 record")
		}
		if !strings.Contains(err.Error(), "not a trace8 database") {
			t.Fatalf("error = %v, want a not-a-database error", err)
		}
		if _, statErr := os.Stat(path + ".lock"); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("lock file survived a failed Open: %v", statErr)
		}
	})

	t.Run("version 2 later record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		first := encodeRecord(1, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("one"))
		future := encodeRecord(2, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("x"))
		future[4] = 2
		writeTestFile(t, path, append(append([]byte{}, first...), future...))

		s, err := Open(path)
		if err == nil {
			s.Close()
			t.Fatal("Open accepted a format version 2 record")
		}
		if !strings.Contains(err.Error(), "corrupt record") || !strings.Contains(err.Error(), "format version 2") {
			t.Fatalf("error = %v, want corruption naming version 2", err)
		}
		if _, statErr := os.Stat(path + ".lock"); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("lock file survived a failed Open: %v", statErr)
		}
	})
}

// TestCompactHygiene drives a delete-heavy log through Compact and checks
// the file shrinks, no temp files linger, ids and payloads are unchanged,
// a stale temp from an interrupted run is harmless, and appends work
// after the rewrite.
func TestCompactHygiene(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	stale := filepath.Join(dir, ".trace8-interrupted.tmp")
	writeTestFile(t, stale, []byte("temp left by a killed compaction"))

	s := openStore(t, path)

	const total = 200
	want := make(map[uint64][]byte)
	for i := 0; i < total; i++ {
		id, err := s.Put([]string{"compact", fmt.Sprintf("n%d", i)}, KindBlob, []byte(fmt.Sprintf("payload-%03d", i)))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if i%2 == 0 {
			ok, err := s.Delete(id)
			if err != nil || !ok {
				t.Fatalf("Delete(%d) = %v, %v", id, ok, err)
			}
			continue
		}
		want[id] = []byte(fmt.Sprintf("payload-%03d", i))
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat before: %v", err)
	}
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat after: %v", err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("Compact did not shrink the log: %d -> %d bytes", before.Size(), after.Size())
	}

	for id, payload := range want {
		rec, ok := s.GetByID(id)
		if !ok {
			t.Fatalf("record %d disappeared across Compact", id)
		}
		if !bytes.Equal(rec.Payload, payload) {
			t.Fatalf("record %d payload = %q, want %q", id, rec.Payload, payload)
		}
	}
	if st := s.Stats(); st.Deleted != 0 || st.Records != len(want) {
		t.Fatalf("Stats after Compact = %+v, want %d records and 0 tombstones", st, len(want))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var temps []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			temps = append(temps, e.Name())
		}
	}
	if len(temps) != 1 || temps[0] != filepath.Base(stale) {
		t.Fatalf("temp files after Compact = %v, want only the stale %s", temps, filepath.Base(stale))
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock file missing after Compact: %v", err)
	}

	newID, err := s.Put([]string{"after"}, KindBlob, []byte("append"))
	if err != nil {
		t.Fatalf("Put after Compact: %v", err)
	}
	if newID != total+1 {
		t.Fatalf("id after Compact = %d, want %d", newID, total+1)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openStore(t, path)
	for id, payload := range want {
		rec, ok := s2.GetByID(id)
		if !ok || !bytes.Equal(rec.Payload, payload) {
			t.Fatalf("record %d after reopen = %+v, %v", id, rec, ok)
		}
	}
	if _, ok := s2.GetByID(newID); !ok {
		t.Fatalf("record %d appended after Compact missing after reopen", newID)
	}
	if st := s2.Stats(); st.Records != len(want)+1 || st.Deleted != 0 {
		t.Fatalf("Stats after reopen = %+v, want %d records and 0 tombstones", st, len(want)+1)
	}
}

// TestGetDuringCompactConsistent pins four readers on the store while it
// compacts three times. Compact may change the on-disk layout, but every
// read must see the same complete set of records with the same payloads.
func TestGetDuringCompactConsistent(t *testing.T) {
	s := openStore(t, testPath(t))

	const n = 300
	expected := make(map[uint64]string, n)
	for i := 0; i < n; i++ {
		payload := fmt.Sprintf("record-%04d", i)
		id, err := s.Put([]string{"during"}, KindBlob, []byte(payload))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		expected[id] = payload
	}

	stop := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				recs, err := s.Get("during")
				if err != nil {
					errs <- fmt.Errorf("Get: %w", err)
					return
				}
				if len(recs) != n {
					errs <- fmt.Errorf("Get during Compact returned %d records, want %d", len(recs), n)
					return
				}
				for _, rec := range recs {
					if got := string(rec.Payload); got != expected[rec.ID] {
						errs <- fmt.Errorf("record %d payload = %q during Compact, want %q", rec.ID, got, expected[rec.ID])
						return
					}
					if rec.Size != int64(len(rec.Payload)) {
						errs <- fmt.Errorf("record %d size %d does not match payload length %d", rec.ID, rec.Size, len(rec.Payload))
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 3; i++ {
		if err := s.Compact(); err != nil {
			t.Fatalf("Compact: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestPayloadEdges round-trips a 1 MB random blob, an empty payload with
// valid keywords, and a nil payload, both live and after a reopen.
func TestPayloadEdges(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	big := make([]byte, 1<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	bigID, err := s.Put([]string{"big"}, KindBlob, big)
	if err != nil {
		t.Fatalf("Put big: %v", err)
	}
	emptyID, err := s.Put([]string{"empty-blob"}, KindBlob, []byte{})
	if err != nil {
		t.Fatalf("Put empty blob: %v", err)
	}
	nilID, err := s.Put([]string{"empty-text"}, KindText, nil)
	if err != nil {
		t.Fatalf("Put nil text: %v", err)
	}

	check := func(s *Store, label string) {
		t.Helper()
		rec, ok := s.GetByID(bigID)
		if !ok {
			t.Fatalf("%s: big record missing", label)
		}
		if rec.Size != int64(len(big)) || !bytes.Equal(rec.Payload, big) {
			t.Fatalf("%s: big record round trip failed: size %d, %d bytes", label, rec.Size, len(rec.Payload))
		}
		for _, id := range []uint64{emptyID, nilID} {
			rec, ok := s.GetByID(id)
			if !ok {
				t.Fatalf("%s: record %d missing", label, id)
			}
			if rec.Size != 0 || len(rec.Payload) != 0 {
				t.Fatalf("%s: record %d = size %d, %d payload bytes, want empty", label, id, rec.Size, len(rec.Payload))
			}
		}
	}
	check(s, "live")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openStore(t, path)
	check(s2, "reloaded")
}

// TestKeywordEdgeCases covers normalization and indexing corners:
// unicode, commas inside one element, duplicate body tokens, a literal
// body: keyword, and a thousand keywords on one record.
func TestKeywordEdgeCases(t *testing.T) {
	path := testPath(t)
	s := openStore(t, path)

	unicodeID, err := s.Put([]string{"Café", "日本", "Ωmega"}, KindBlob, []byte("u"))
	if err != nil {
		t.Fatalf("Put unicode: %v", err)
	}
	commaID, err := s.Put([]string{"Notes, 2026"}, KindBlob, []byte("c"))
	if err != nil {
		t.Fatalf("Put comma: %v", err)
	}
	dupID, err := s.Put([]string{"dup"}, KindText, []byte("queue queue Queue"))
	if err != nil {
		t.Fatalf("Put duplicate body tokens: %v", err)
	}
	bodyID, err := s.Put([]string{"body:queue"}, KindBlob, []byte("b"))
	if err != nil {
		t.Fatalf("Put body keyword: %v", err)
	}
	collideID, err := s.Put([]string{"body:queue"}, KindText, []byte("queue queue"))
	if err != nil {
		t.Fatalf("Put body collision: %v", err)
	}

	wantKeywords := make([]string, 1000)
	for i := range wantKeywords {
		wantKeywords[i] = fmt.Sprintf("kw%04d", i)
	}
	wideID, err := s.Put(wantKeywords, KindBlob, []byte("w"))
	if err != nil {
		t.Fatalf("Put 1000 keywords: %v", err)
	}

	rec, ok := s.GetByID(unicodeID)
	if !ok || !slices.Equal(rec.Keywords, []string{"café", "日本", "ωmega"}) {
		t.Fatalf("unicode keywords = %v, %v", rec.Keywords, ok)
	}
	rec, ok = s.GetByID(commaID)
	if !ok || !slices.Equal(rec.Keywords, []string{"notes, 2026"}) {
		t.Fatalf("comma keyword = %v, %v", rec.Keywords, ok)
	}
	rec, ok = s.GetByID(dupID)
	if !ok || !slices.Equal(rec.Keywords, []string{"dup", "body:queue"}) {
		t.Fatalf("duplicate body tokens = %v, %v", rec.Keywords, ok)
	}
	if got, _ := s.Get("body:queue"); len(got) != 3 {
		// dup, body, and collide records all carry body:queue
		t.Fatalf("Get(body:queue) = %d records, want 3", len(got))
	}
	rec, ok = s.GetByID(collideID)
	if !ok || !slices.Equal(rec.Keywords, []string{"body:queue"}) {
		t.Fatalf("body collision keywords = %v, %v", rec.Keywords, ok)
	}
	rec, ok = s.GetByID(bodyID)
	if !ok || !slices.Equal(rec.Keywords, []string{"body:queue"}) {
		t.Fatalf("literal body keyword = %v, %v", rec.Keywords, ok)
	}
	rec, ok = s.GetByID(wideID)
	if !ok || !slices.Equal(rec.Keywords, wantKeywords) {
		t.Fatalf("1000-keyword record = %d keywords, %v", len(rec.Keywords), ok)
	}
	if st := s.Stats(); st.Keywords != 1000+6 {
		// 1000 wide keywords plus café, 日本, ωmega, notes 2026, dup, body:queue
		t.Fatalf("keyword count = %d, want 1006", st.Keywords)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openStore(t, path)
	rec, ok = s2.GetByID(unicodeID)
	if !ok || !slices.Equal(rec.Keywords, []string{"café", "日本", "ωmega"}) {
		t.Fatalf("unicode keywords after reload = %v, %v", rec.Keywords, ok)
	}
	if recs, _ := s2.Get("café"); len(recs) != 1 || recs[0].ID != unicodeID {
		t.Fatalf("Get(café) after reload = %+v", recs)
	}
	if recs, _ := s2.Get("ωmega"); len(recs) != 1 || recs[0].ID != unicodeID {
		t.Fatalf("Get(ωmega) after reload = %+v", recs)
	}
	recs, _ := s2.Get("notes, 2026")
	if len(recs) != 1 || recs[0].ID != commaID {
		t.Fatalf("Get(notes, 2026) after reload = %+v", recs)
	}
	rec, ok = s2.GetByID(wideID)
	if !ok || !slices.Equal(rec.Keywords, wantKeywords) {
		t.Fatalf("1000-keyword record after reload = %d keywords, %v", len(rec.Keywords), ok)
	}
	if recs, _ := s2.Get("kw0000"); len(recs) != 1 || recs[0].ID != wideID {
		t.Fatalf("Get(kw0000) after reload = %+v", recs)
	}
	if recs, _ := s2.Get("kw0999"); len(recs) != 1 || recs[0].ID != wideID {
		t.Fatalf("Get(kw0999) after reload = %+v", recs)
	}
}

// TestLookupMisses checks that misses are ordinary empty results: a
// ranged Get over an unknown keyword runs zero times, an unknown id is
// not found, and a delete of an unknown id is false with no tombstone.
func TestLookupMisses(t *testing.T) {
	s := openStore(t, testPath(t))
	id, err := s.Put([]string{"known"}, KindBlob, []byte("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	recs, err := s.Get("unknown")
	if err != nil {
		t.Fatalf("Get(unknown): %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("Get(unknown) = %+v, want no records", recs)
	}
	count := 0
	for range recs {
		count++
	}
	if count != 0 {
		t.Fatalf("range over Get(unknown) ran %d times", count)
	}

	if _, ok := s.GetByID(id + 999); ok {
		t.Fatal("GetByID found an unknown id")
	}
	if ok, err := s.Delete(id + 999); err != nil || ok {
		t.Fatalf("Delete(unknown) = %v, %v, want false and no error", ok, err)
	}
	if st := s.Stats(); st.Records != 1 || st.Deleted != 0 {
		t.Fatalf("Stats = %+v, want 1 record and no tombstones", st)
	}
}

func TestClosedStore(t *testing.T) {
	path := testPath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, err := s.Put([]string{"kept"}, KindBlob, []byte("payload"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := s.Put([]string{"later"}, KindBlob, nil); !errors.Is(err, errClosed) {
		t.Fatalf("Put after Close = %v, want errClosed", err)
	}
	if ok, err := s.Delete(id); !errors.Is(err, errClosed) || ok {
		t.Fatalf("Delete after Close = %v, %v, want errClosed", ok, err)
	}
	if err := s.Compact(); !errors.Is(err, errClosed) {
		t.Fatalf("Compact after Close = %v, want errClosed", err)
	}

	// Reads stay available from memory, by design.
	recs, err := s.Get("kept")
	if err != nil || len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("Get after Close = %+v, %v, want record %d", recs, err, id)
	}
	if _, ok := s.GetByID(id); !ok {
		t.Fatal("GetByID after Close did not find the record")
	}
	if st := s.Stats(); st.Records != 1 {
		t.Fatalf("Stats after Close = %+v", st)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file still present after Close: %v", err)
	}
}

// TestStressConcurrentLifecycle runs eight writers, eight readers, one
// compactor, and one closer against one store. Writers and the compactor
// must only ever see nil or errClosed; readers must always see complete
// records; Close must not be observed as a panic or a partial write.
func TestStressConcurrentLifecycle(t *testing.T) {
	path := testPath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const (
		writerCount = 8
		readerCount = 8
		writerOps   = 200
	)

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		puts     atomic.Int64
		deletes  atomic.Int64
		compacts atomic.Int64
	)
	sawClosed := make(chan struct{}, writerCount+1)
	recordClosed := func() {
		select {
		case sawClosed <- struct{}{}:
		default:
		}
	}

	for w := 0; w < writerCount; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < writerOps; i++ {
				if stop.Load() {
					// Close has returned; a write now must fail cleanly.
					if _, err := s.Put([]string{"after-close"}, KindBlob, nil); !errors.Is(err, errClosed) {
						t.Errorf("Put after close = %v, want errClosed", err)
					} else {
						recordClosed()
					}
					return
				}
				id, err := s.Put([]string{"stress", fmt.Sprintf("w%d", w)}, KindBlob,
					[]byte(fmt.Sprintf("payload-%d-%d", w, i)))
				if errors.Is(err, errClosed) {
					recordClosed()
					return
				}
				if err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				puts.Add(1)
				if i%5 == 0 {
					ok, err := s.Delete(id)
					if errors.Is(err, errClosed) {
						recordClosed()
						return
					}
					if err != nil {
						t.Errorf("Delete(%d): %v", id, err)
						return
					}
					if !ok {
						t.Errorf("Delete(%d) = false", id)
						return
					}
					deletes.Add(1)
				}
			}
		}(w)
	}

	for r := 0; r < readerCount; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				recs, err := s.Get("stress")
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				for _, rec := range recs {
					if rec.Size != int64(len(rec.Payload)) {
						t.Errorf("record %d size %d does not match payload length %d", rec.ID, rec.Size, len(rec.Payload))
						return
					}
					if again, ok := s.GetByID(rec.ID); ok && !bytes.Equal(again.Payload, rec.Payload) {
						t.Errorf("GetByID(%d) payload differs from Get result", rec.ID)
						return
					}
				}
				_ = s.Stats()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			err := s.Compact()
			if errors.Is(err, errClosed) {
				recordClosed()
				return
			}
			if err != nil {
				t.Errorf("Compact: %v", err)
				return
			}
			compacts.Add(1)
		}
	}()

	// The main goroutine is the closer. Let the writers fill the store,
	// then close while everything is still running.
	deadline := time.Now().Add(2 * time.Second)
	for puts.Load() < 50 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stop.Store(true)
	wg.Wait()

	t.Logf("puts=%d deletes=%d compacts=%d", puts.Load(), deletes.Load(), compacts.Load())
	if puts.Load() == 0 {
		t.Fatal("no successful puts before Close")
	}
	select {
	case <-sawClosed:
	default:
		t.Fatal("no goroutine observed errClosed after Close")
	}
	if st := s.Stats(); st.Records < 0 {
		t.Fatalf("Stats after close = %+v", st)
	}
}
