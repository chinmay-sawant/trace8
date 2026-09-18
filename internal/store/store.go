package store

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// errClosed is returned by writes after Close. Reads keep working from
// memory, so only the mutating methods guard on it.
var errClosed = errors.New("store: closed")

// Store is a local keyword store backed by an append-only log. Open
// replays the log into the index; reads are map lookups and never touch
// disk. Put, Delete, and Compact write the log immediately. All
// exported methods are safe for concurrent use.
type Store struct {
	path     string
	lockName string

	mu     sync.RWMutex
	index  *index
	file   *os.File
	closed bool
	// size is the logical end of the log in bytes, the offset just past
	// the last acknowledged append. A failed append is truncated back to
	// it so a later write cannot land behind torn bytes.
	size int64
	// writeErr is a sticky error set when the log handle is lost (for
	// example a failed reopen after compaction). Writes return it
	// instead of dereferencing a nil file.
	writeErr error
}

// Open opens the log at path, creating it when missing, and replays it
// into memory. It holds a lock file at path+".lock" until Close, so a
// second Open on the same path fails while the first is open. A record
// cut off by the end of the log is discarded with a warning and the log
// is truncated back to the last complete record; corruption elsewhere
// fails Open and releases the lock.
func Open(path string) (*Store, error) {
	lockName := path + ".lock"
	lock, err := os.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("store: %s is locked by another process (remove %s if it is stale)", path, lockName)
		}
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	_ = lock.Close()

	s := &Store{path: path, lockName: lockName, index: newIndex()}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		s.releaseLock()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	s.file = f
	if err := s.replay(); err != nil {
		_ = f.Close()
		s.releaseLock()
		return nil, err
	}
	return s, nil
}

// Put stores payload under keywords and returns the new record id.
// Keywords are lowercased, trimmed, deduped, and at least one must
// remain, else ErrNoKeywords. Text payloads also index their body
// tokens under a "body:" prefix. The store keeps payload without
// copying, so callers must not modify it after Put.
func (s *Store) Put(keywords []string, kind Kind, payload []byte) (uint64, error) {
	if kind != KindText && kind != KindBlob {
		return 0, fmt.Errorf("unknown kind %q", kind)
	}
	raw := make([]string, 0, len(keywords)+16)
	raw = append(raw, keywords...)
	if kind == KindText {
		for _, tok := range tokenize(string(payload)) {
			raw = append(raw, "body:"+tok)
		}
	}
	norm, err := normalizeKeywords(raw)
	if err != nil {
		return 0, err
	}
	stored, codec, err := compressPayload(payload)
	if err != nil {
		return 0, fmt.Errorf("store: %s: compress: %w", s.path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errClosed
	}
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	id := s.index.nextID
	buf := encodeRecord(id, kindToByte(kind), codec, 0, encodeKeywords(norm), stored)
	if err := s.appendLog(buf); err != nil {
		return 0, fmt.Errorf("store: %s: append: %w", s.path, err)
	}
	rec := Record{ID: id, Kind: kind, Keywords: norm, Size: int64(len(payload)), Payload: payload}
	s.index.add(rec)
	s.index.nextID = id + 1
	return id, nil
}

// Get returns the records carrying keyword, ascending by id. The
// keyword is lowercased and trimmed before the lookup, matching Put's
// normalization, so "Go" and " go " find records stored under "go".
// Body tokens live under a "body:" prefix. The records are struct
// copies, so changing a returned field is safe, but the Keywords
// slices and Payload byte slices share the index's backing arrays and
// must not be modified.
func (s *Store) Get(keyword string) ([]Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.index.lookup(strings.ToLower(strings.TrimSpace(keyword))), nil
}

// GetByID returns the record with id. The bool is false when the id is
// unknown or deleted. The sharing rules of Get apply to the result.
func (s *Store) GetByID(id uint64) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.index.records[id]
	return rec, ok
}

// Delete removes the record with id, reporting false for an unknown
// id. The delete is durable immediately as a tombstone record, and the
// id is never reused.
func (s *Store) Delete(id uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, errClosed
	}
	if s.writeErr != nil {
		return false, s.writeErr
	}
	rec, ok := s.index.records[id]
	if !ok {
		return false, nil
	}
	buf := encodeRecord(id, kindToByte(rec.Kind), codecRaw, flagTombstone, nil, nil)
	if err := s.appendLog(buf); err != nil {
		return false, fmt.Errorf("store: %s: append tombstone: %w", s.path, err)
	}
	s.index.remove(rec)
	s.index.deleted++
	return true, nil
}

// appendLog writes one encoded record at the logical end of the log. On
// a failed or short write it truncates the file back to the logical end
// so a later append cannot land behind torn bytes. When that rollback
// fails too the store latches writeErr and refuses further writes.
func (s *Store) appendLog(buf []byte) error {
	n, err := s.file.Write(buf)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	if err == nil {
		s.size += int64(len(buf))
		return nil
	}
	if terr := s.file.Truncate(s.size); terr != nil {
		s.writeErr = fmt.Errorf("store: %s: append: %w (rollback failed: %w)", s.path, err, terr)
		return s.writeErr
	}
	return err
}

// Stats returns a snapshot of the counters: live records, distinct
// keywords, tombstones since the last compact, and the log size on disk.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{
		Records:  len(s.index.records),
		Keywords: len(s.index.byKeyword),
		Deleted:  s.index.deleted,
	}
	if fi, err := os.Stat(s.path); err == nil {
		st.FileBytes = fi.Size()
	}
	return st
}

// Compact rewrites the log with only live records, ascending by id,
// dropping tombstones. The rewrite goes to a temp file in the same
// directory, which is synced and renamed over the log before the append
// handle is reopened, so the original log is intact if compaction stops
// before the rename. The lock file is untouched.
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if s.writeErr != nil {
		return s.writeErr
	}

	ids := make([]uint64, 0, len(s.index.records))
	for id := range s.index.records {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// os.CreateTemp makes the temp file 0600, and the rename would then
	// change the log's permissions. Copy the original mode across.
	perm := os.FileMode(0o644)
	if fi, perr := os.Stat(s.path); perr == nil {
		perm = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".trace8-compact-*.tmp")
	if err != nil {
		return fmt.Errorf("store: %s: compact: %w", s.path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: %s: compact: %w", s.path, err)
	}

	for _, id := range ids {
		rec := s.index.records[id]
		stored, codec, err := compressPayload(rec.Payload)
		if err != nil {
			_ = tmp.Close()
			return fmt.Errorf("store: %s: compact: %w", s.path, err)
		}
		buf := encodeRecord(id, kindToByte(rec.Kind), codec, 0, encodeKeywords(rec.Keywords), stored)
		if _, err := tmp.Write(buf); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("store: %s: compact: %w", s.path, err)
		}
	}
	if s.index.nextID > 1 {
		// Tombstones carry the high-water id and Compact drops them.
		// The marker keeps ids from being reused after a reopen.
		buf := encodeRecord(s.index.nextID-1, kindToByte(KindBlob), codecRaw, flagHighWater, nil, nil)
		if _, err := tmp.Write(buf); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("store: %s: compact: %w", s.path, err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: %s: compact: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: %s: compact: %w", s.path, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("store: %s: compact: %w", s.path, err)
	}
	tmpName = "" // the rename consumed the temp file

	if err := s.file.Close(); err != nil {
		s.file = nil
		s.writeErr = fmt.Errorf("store: %s: compact: close: %w", s.path, err)
		return s.writeErr
	}
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		s.file = nil
		s.writeErr = fmt.Errorf("store: %s: compact: reopen: %w", s.path, err)
		return s.writeErr
	}
	s.file = f
	fi, err := os.Stat(s.path)
	if err != nil {
		s.writeErr = fmt.Errorf("store: %s: compact: %w", s.path, err)
		return s.writeErr
	}
	s.size = fi.Size()
	s.index.deleted = 0
	return nil
}

// Close removes the lock file. Appends are durable on their own, so
// there is nothing to flush. Close is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var first error
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			first = fmt.Errorf("store: %s: %w", s.path, err)
		}
		s.file = nil
	}
	if err := os.Remove(s.lockName); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
		first = fmt.Errorf("store: %s: %w", s.path, err)
	}
	return first
}

// replay scans the log from the start and applies every record to the
// index. A record cut off by the end of the file is dropped with a
// warning and the file is truncated back to the last complete record,
// so a later append starts from a clean boundary; mid-file corruption
// is an error.
func (s *Store) replay() error {
	fi, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("store: %s: %w", s.path, err)
	}
	size := fi.Size()
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("store: %s: %w", s.path, err)
	}
	br := bufio.NewReader(s.file)

	var offset int64
	var hdr [headerSize]byte
	for {
		n, err := io.ReadFull(br, hdr[:])
		if err != nil {
			switch err {
			case io.EOF:
				s.size = offset // clean end of log
				return nil
			case io.ErrUnexpectedEOF:
				if offset == 0 && !headerPrefix(hdr[:n]) {
					return fmt.Errorf("store: %s: not a trace8 database", s.path)
				}
				log.Printf("store: %s: discarding truncated tail at offset %d", s.path, offset)
				return s.discardTail(offset)
			default:
				return fmt.Errorf("store: %s: reading header at offset %d: %w", s.path, offset, err)
			}
		}

		h := decodeHeader(hdr[:])
		if string(hdr[:len(logMagic)]) != logMagic {
			if offset == 0 {
				return fmt.Errorf("store: %s: not a trace8 database", s.path)
			}
			return fmt.Errorf("store: %s: corrupt record at offset %d: bad magic", s.path, offset)
		}
		if hdr[len(logMagic)] != formatVersion {
			if offset == 0 {
				return fmt.Errorf("store: %s: not a trace8 database", s.path)
			}
			return fmt.Errorf("store: %s: corrupt record at offset %d: format version %d", s.path, offset, hdr[len(logMagic)])
		}
		if h.kind != kindTextByte && h.kind != kindBlobByte {
			return fmt.Errorf("store: %s: corrupt record at offset %d: bad kind %q", s.path, offset, h.kind)
		}
		if h.codec != codecRaw && h.codec != codecGzip {
			return fmt.Errorf("store: %s: corrupt record at offset %d: bad codec %d", s.path, offset, h.codec)
		}

		avail := uint64(size - offset - headerSize)
		if uint64(h.keyLen) > avail || h.payLen > avail-uint64(h.keyLen) {
			log.Printf("store: %s: discarding truncated tail at offset %d", s.path, offset)
			return s.discardTail(offset)
		}
		body := make([]byte, int64(h.keyLen)+int64(h.payLen))
		if _, err := io.ReadFull(br, body); err != nil {
			// The size check above makes this unreachable for a file
			// that does not change under us, but stay safe.
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				log.Printf("store: %s: discarding truncated tail at offset %d", s.path, offset)
				return s.discardTail(offset)
			}
			return fmt.Errorf("store: %s: reading record at offset %d: %w", s.path, offset, err)
		}
		if err := s.apply(h, body, offset); err != nil {
			return err
		}
		offset += headerSize + int64(len(body))
	}
}

// discardTail cuts the log back to offset after a partial record was
// found there. Without the cut, a later append would sit behind the
// partial bytes and every future Open would read them as corruption.
func (s *Store) discardTail(offset int64) error {
	if err := s.file.Truncate(offset); err != nil {
		return fmt.Errorf("store: %s: truncating tail at offset %d: %w", s.path, offset, err)
	}
	s.size = offset
	return nil
}

// apply merges one decoded log record into the index. offset is used
// only in error messages.
func (s *Store) apply(h header, body []byte, offset int64) error {
	if h.flags&flagHighWater != 0 {
		// The marker keeps the highest id handed out alive across
		// compactions; it is not a record, so the index is untouched.
		if h.id >= s.index.nextID {
			s.index.nextID = h.id + 1
		}
		return nil
	}
	if h.flags&flagTombstone != 0 {
		if rec, ok := s.index.records[h.id]; ok {
			s.index.remove(rec)
		}
		s.index.deleted++
	} else {
		payload, err := decompressPayload(body[h.keyLen:], h.codec)
		if err != nil {
			return fmt.Errorf("store: %s: record at offset %d: %w", s.path, offset, err)
		}
		rec := Record{
			ID:       h.id,
			Kind:     kindFromByte(h.kind),
			Keywords: decodeKeywords(body[:h.keyLen]),
			Size:     int64(len(payload)),
			Payload:  payload,
		}
		s.index.add(rec)
	}
	if h.id >= s.index.nextID {
		s.index.nextID = h.id + 1
	}
	return nil
}

// releaseLock removes the lock file, logging any failure. It runs on
// Open error paths and from Close.
func (s *Store) releaseLock() {
	if err := os.Remove(s.lockName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("store: %s: removing lock: %v", s.path, err)
	}
}
