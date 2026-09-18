package store

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Log format, one record per entry, all integers big-endian. The byte
// layout is frozen:
//
//	offset size field
//	0      4    magic "T8LG"
//	4      1    format version (1)
//	5      1    kind 't' or 'b'
//	6      1    codec: 0 raw, 1 gzip
//	7      1    flags: bit 0 tombstone, bit 1 high-water marker
//	8      8    id
//	16     4    keywords length K (byte length)
//	20     8    stored payload length P
//	28     K    keywords blob: normalized keywords joined with '\n'
//	28+K   P    stored payload bytes (raw or gzip per codec)
const (
	logMagic      = "T8LG"
	formatVersion = byte(1)
	headerSize    = 28
)

const (
	kindTextByte = 't'
	kindBlobByte = 'b'
)

const (
	codecRaw  = byte(0)
	codecGzip = byte(1)
)

// flagTombstone marks a record that deletes the record with the same id.
const flagTombstone = byte(1 << 0)

// flagHighWater marks the marker Compact appends after the live records.
// The marker carries the highest id ever handed out, so ids are not
// reused once tombstones are dropped.
const flagHighWater = byte(0x02)

// header is the fixed 28-byte prefix of a log record.
type header struct {
	kind   byte
	codec  byte
	flags  byte
	id     uint64
	keyLen uint32
	payLen uint64
}

// encodeRecord returns the on-disk bytes of one record. keywords must
// already be normalized, and stored is the payload after compression.
func encodeRecord(id uint64, kind, codec, flags byte, keywords, stored []byte) []byte {
	buf := make([]byte, 0, headerSize+len(keywords)+len(stored))
	buf = append(buf, logMagic...)
	buf = append(buf, formatVersion, kind, codec, flags)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	buf = append(buf, b[:]...)
	binary.BigEndian.PutUint32(b[:4], uint32(len(keywords)))
	buf = append(buf, b[:4]...)
	binary.BigEndian.PutUint64(b[:], uint64(len(stored)))
	buf = append(buf, b[:]...)
	buf = append(buf, keywords...)
	buf = append(buf, stored...)
	return buf
}

// decodeHeader reads the fixed header fields. The caller must pass at
// least headerSize bytes and must validate magic and version.
func decodeHeader(b []byte) header {
	return header{
		kind:   b[5],
		codec:  b[6],
		flags:  b[7],
		id:     binary.BigEndian.Uint64(b[8:16]),
		keyLen: binary.BigEndian.Uint32(b[16:20]),
		payLen: binary.BigEndian.Uint64(b[20:28]),
	}
}

// headerPrefix reports whether the first len(b) bytes of a log could
// still grow into a record header: the magic must match so far, and once
// the magic is complete the version byte must match too. A short first
// read that is not a header prefix is not a trace8 database.
func headerPrefix(b []byte) bool {
	if len(b) <= len(logMagic) {
		return string(b) == logMagic[:len(b)]
	}
	return string(b[:len(logMagic)]) == logMagic && b[len(logMagic)] == formatVersion
}

// kindToByte maps a validated Kind to its log byte.
func kindToByte(k Kind) byte {
	if k == KindText {
		return kindTextByte
	}
	return kindBlobByte
}

// kindFromByte maps a validated log byte back to a Kind.
func kindFromByte(b byte) Kind {
	if b == kindTextByte {
		return KindText
	}
	return KindBlob
}

// encodeKeywords joins normalized keywords into the log blob.
func encodeKeywords(keywords []string) []byte {
	return []byte(strings.Join(keywords, "\n"))
}

// decodeKeywords splits a keywords blob, skipping empty entries. An
// empty blob (tombstones) and a blob of only separators yield nil.
func decodeKeywords(blob []byte) []string {
	if len(blob) == 0 {
		return nil
	}
	parts := strings.Split(string(blob), "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// compressPayload returns the stored form of payload and its codec.
// gzip is used only when it is strictly smaller than the raw bytes.
func compressPayload(payload []byte) ([]byte, byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		return nil, 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	if buf.Len() < len(payload) {
		return buf.Bytes(), codecGzip, nil
	}
	return payload, codecRaw, nil
}

// gzipReaders pools gzip.Reader state across payload decodes. A reader
// holds the flate decompressor's buffers, and replay would otherwise
// allocate a fresh set for every compressed record. Reset starts a new
// stream, so one pooled reader decodes the same bytes a fresh one
// would. The pool is safe for concurrent use; each reader is checked
// out to one goroutine at a time.
var gzipReaders = sync.Pool{New: func() any { return new(gzip.Reader) }}

// decompressPayload returns the original payload for one stored blob.
func decompressPayload(stored []byte, codec byte) ([]byte, error) {
	switch codec {
	case codecRaw:
		return stored, nil
	case codecGzip:
		zr := gzipReaders.Get().(*gzip.Reader)
		defer gzipReaders.Put(zr)
		if err := zr.Reset(bytes.NewReader(stored)); err != nil {
			return nil, err
		}
		payload, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		if err := zr.Close(); err != nil {
			return nil, err
		}
		return payload, nil
	default:
		return nil, fmt.Errorf("unknown codec %d", codec)
	}
}
