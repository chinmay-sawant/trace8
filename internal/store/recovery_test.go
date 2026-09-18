package store

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func TestTruncatedTail(t *testing.T) {
	good := encodeRecord(1, kindTextByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("hello"))
	second := encodeRecord(2, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("world"))

	tests := []struct {
		name string
		tail []byte
	}{
		{"cut mid header", second[:20]},
		{"magic only", second[:4]},
		{"cut mid body", second[:headerSize+3]},
		{"short garbage", []byte("junk")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			writeTestFile(t, path, append(append([]byte{}, good...), tt.tail...))

			logs := captureLog(t)
			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()

			if got := s.Stats().Records; got != 1 {
				t.Fatalf("records = %d, want 1", got)
			}
			recs, err := s.Get("go")
			if err != nil || len(recs) != 1 || recs[0].ID != 1 {
				t.Fatalf("Get(go) = %+v, %v, want record 1", recs, err)
			}
			if !strings.Contains(logs.String(), "truncated tail") {
				t.Fatalf("warning not logged, got %q", logs.String())
			}
		})
	}
}

func TestBadFirstRecord(t *testing.T) {
	valid := encodeRecord(1, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("hello"))
	wrongVersion := append([]byte{}, valid...)
	wrongVersion[4] = 99

	tests := []struct {
		name string
		data []byte
	}{
		{"garbage header", []byte("this is not a trace8 database at all")},
		{"short garbage", []byte("hello")},
		{"wrong version", wrongVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			writeTestFile(t, path, tt.data)

			s, err := Open(path)
			if err == nil {
				s.Close()
				t.Fatal("Open succeeded on a file that is not a trace8 database")
			}
			if s != nil {
				t.Fatalf("Open returned a store alongside error %v", err)
			}
			if !strings.Contains(err.Error(), "not a trace8 database") {
				t.Fatalf("error = %v, want a not-a-database error", err)
			}
			if _, statErr := os.Stat(path + ".lock"); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("lock file survived a failed Open: %v", statErr)
			}
		})
	}
}

func TestCorruptLaterRecord(t *testing.T) {
	good := encodeRecord(1, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("hello"))

	badMagic := encodeRecord(2, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("x"))
	copy(badMagic[:4], "XXXX")

	badVersion := encodeRecord(3, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("x"))
	badVersion[4] = 9

	badKind := encodeRecord(4, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("x"))
	badKind[5] = 'z'

	badCodec := encodeRecord(5, kindBlobByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("x"))
	badCodec[6] = 7

	tests := []struct {
		name string
		rec  []byte
		want string
	}{
		{"bad magic", badMagic, "bad magic"},
		{"bad version", badVersion, "format version 9"},
		{"bad kind", badKind, "bad kind"},
		{"bad codec", badCodec, "bad codec 7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			writeTestFile(t, path, append(append([]byte{}, good...), tt.rec...))

			s, err := Open(path)
			if err == nil {
				s.Close()
				t.Fatal("Open succeeded on a log with a corrupt record")
			}
			if s != nil {
				t.Fatalf("Open returned a store alongside error %v", err)
			}
			if !strings.Contains(err.Error(), "corrupt record") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want corruption mentioning %q", err, tt.want)
			}
			if _, statErr := os.Stat(path + ".lock"); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("lock file survived a failed Open: %v", statErr)
			}
		})
	}
}

func TestTombstoneReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	put := encodeRecord(1, kindTextByte, codecRaw, 0, encodeKeywords([]string{"go"}), []byte("hello"))
	tomb := encodeRecord(1, kindTextByte, codecRaw, flagTombstone, nil, nil)
	writeTestFile(t, path, append(append([]byte{}, put...), tomb...))

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if st := s.Stats(); st.Records != 0 || st.Deleted != 1 {
		t.Fatalf("Stats = %+v, want 0 records and 1 tombstone", st)
	}
	if recs, _ := s.Get("go"); len(recs) != 0 {
		t.Fatalf("Get(go) = %+v, want none", recs)
	}
	if _, ok := s.GetByID(1); ok {
		t.Fatal("deleted record is still visible")
	}

	// Ids never get reused: the next Put takes 2, not 1.
	id, err := s.Put([]string{"next"}, KindBlob, []byte("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != 2 {
		t.Fatalf("id = %d, want 2", id)
	}
}
