package store

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

const benchRecords = 10000

// benchPath builds a log with benchRecords records, half text and half
// random payloads, five keywords each, and returns its path.
func benchPath(b *testing.B) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.db")
	s, err := Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	rng := rand.New(rand.NewSource(1))
	random := make([]byte, 256)
	for i := 0; i < benchRecords; i++ {
		keywords := []string{
			"bench",
			fmt.Sprintf("tag%d", i%100),
			fmt.Sprintf("group%d", i%10),
			fmt.Sprintf("n%d", i%1000),
			"all",
		}
		var (
			kind    Kind
			payload []byte
		)
		if i%2 == 0 {
			kind = KindText
			payload = []byte(strings.Repeat("text ", 51))
		} else {
			kind = KindBlob
			if _, err := rng.Read(random); err != nil {
				b.Fatalf("rand read: %v", err)
			}
			payload = append([]byte(nil), random...)
		}
		if _, err := s.Put(keywords, kind, payload); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}
	return path
}

func BenchmarkGet(b *testing.B) {
	path := benchPath(b)
	s, err := Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer s.Close()

	warm, err := s.Get("tag42")
	if err != nil || len(warm) == 0 {
		b.Fatalf("warm Get: %d records, %v", len(warm), err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recs, err := s.Get("tag42")
		if err != nil || len(recs) != len(warm) {
			b.Fatalf("Get: %d records, %v", len(recs), err)
		}
	}
}

func BenchmarkOpen(b *testing.B) {
	path := benchPath(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := Open(path)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		if st := s.Stats(); st.Records != benchRecords {
			b.Fatalf("records = %d, want %d", st.Records, benchRecords)
		}
		if err := s.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}
