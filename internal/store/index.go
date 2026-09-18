package store

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind separates display behavior only; storage is identical.
type Kind string

const (
	KindText Kind = "text"
	KindBlob Kind = "blob"
)

// Record is one stored entry. Keywords are normalized: lowercase,
// trimmed, deduped, first-seen order. Size is len(Payload) at Put time.
// Text records also index body tokens under a "body:" prefix.
type Record struct {
	ID       uint64   `json:"id"`
	Kind     Kind     `json:"kind"`
	Keywords []string `json:"keywords"`
	Size     int64    `json:"size"`
	Payload  []byte   `json:"payload"`
}

// Stats is a snapshot of the store counters. Deleted counts tombstone
// records appended since the last Compact; FileBytes is the log size on
// disk and is 0 when the file does not exist.
type Stats struct {
	Records   int   `json:"records"`
	Keywords  int   `json:"keywords"`
	Deleted   int   `json:"deleted"`
	FileBytes int64 `json:"file_bytes"`
}

// ErrNoKeywords is returned by Put when normalization leaves nothing.
var ErrNoKeywords = errors.New("no keywords given")

// index is the in-memory inverted index. The zero value is not ready;
// use newIndex.
type index struct {
	nextID    uint64              // next id to hand out; starts at 1
	byKeyword map[string][]uint64 // keyword -> record ids, ascending
	records   map[uint64]Record   // id -> live record
	deleted   int                 // tombstones seen since the last compact
}

func newIndex() *index {
	return &index{
		nextID:    1,
		byKeyword: make(map[string][]uint64),
		records:   make(map[uint64]Record),
	}
}

// add stores rec under its id and keywords. A keyword's id list stays
// ascending because ids only grow and put appends at the end.
func (ix *index) add(rec Record) {
	ix.records[rec.ID] = rec
	for _, kw := range rec.Keywords {
		ix.byKeyword[kw] = append(ix.byKeyword[kw], rec.ID)
	}
}

// remove drops rec and its postings. A keyword entry disappears when
// its last id goes away.
func (ix *index) remove(rec Record) {
	delete(ix.records, rec.ID)
	for _, kw := range rec.Keywords {
		ids := ix.byKeyword[kw]
		for i, id := range ids {
			if id == rec.ID {
				ids = append(ids[:i], ids[i+1:]...)
				break
			}
		}
		if len(ids) == 0 {
			delete(ix.byKeyword, kw)
			continue
		}
		ix.byKeyword[kw] = ids
	}
}

// lookup returns records carrying kw in ascending id order.
func (ix *index) lookup(kw string) []Record {
	ids := ix.byKeyword[kw]
	if len(ids) == 0 {
		return nil
	}
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		rec, ok := ix.records[id]
		if !ok {
			continue // defensive: postings are cleaned on delete
		}
		out = append(out, rec)
	}
	return out
}

// normalizeKeywords lowercases and trims each keyword, drops empties,
// removes duplicates, and keeps first-seen order.
func normalizeKeywords(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, kw := range raw {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw == "" {
			continue
		}
		// Keywords are joined with '\n' in the log, so an embedded
		// newline would split one keyword into two on reload.
		if strings.ContainsRune(kw, '\n') {
			return nil, fmt.Errorf("keyword %q contains a newline", kw)
		}
		if _, dup := seen[kw]; dup {
			continue
		}
		seen[kw] = struct{}{}
		out = append(out, kw)
	}
	if len(out) == 0 {
		return nil, ErrNoKeywords
	}
	return out, nil
}

// tokenize splits s on anything that is not a letter or digit,
// lowercases each piece, and drops tokens shorter than 2 runes.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ToLower(f)
		if utf8.RuneCountInString(f) < 2 {
			continue
		}
		out = append(out, f)
	}
	return out
}
