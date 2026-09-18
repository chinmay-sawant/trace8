package store

import (
	"bytes"
	"crypto/rand"
	"slices"
	"testing"
)

func TestRecordLayout(t *testing.T) {
	keywords := []byte("go\ndb")
	stored := []byte("payload")
	buf := encodeRecord(42, kindTextByte, codecGzip, flagTombstone, keywords, stored)

	if string(buf[:4]) != logMagic {
		t.Fatalf("magic = %q, want %q", buf[:4], logMagic)
	}
	if buf[4] != formatVersion || buf[5] != kindTextByte || buf[6] != codecGzip || buf[7] != flagTombstone {
		t.Fatalf("header bytes = %v", buf[4:8])
	}
	h := decodeHeader(buf)
	if h.id != 42 || h.keyLen != uint32(len(keywords)) || h.payLen != uint64(len(stored)) {
		t.Fatalf("decoded header = %+v", h)
	}
	if len(buf) != headerSize+len(keywords)+len(stored) {
		t.Fatalf("record length = %d, want %d", len(buf), headerSize+len(keywords)+len(stored))
	}
	if !bytes.Equal(buf[headerSize:headerSize+len(keywords)], keywords) {
		t.Fatalf("keywords blob = %q, want %q", buf[headerSize:headerSize+len(keywords)], keywords)
	}
	if !bytes.Equal(buf[headerSize+len(keywords):], stored) {
		t.Fatalf("payload = %q, want %q", buf[headerSize+len(keywords):], stored)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	for _, h := range []header{
		{kind: kindTextByte, codec: codecRaw, flags: 0, id: 0, keyLen: 0, payLen: 0},
		{kind: kindBlobByte, codec: codecGzip, flags: flagTombstone, id: 1, keyLen: 1, payLen: 1},
		{kind: kindTextByte, codec: codecGzip, flags: 0, id: 1 << 40, keyLen: 4096, payLen: 4096},
		{kind: kindBlobByte, codec: codecRaw, flags: 0, id: ^uint64(0), keyLen: 7, payLen: 255},
	} {
		buf := encodeRecord(h.id, h.kind, h.codec, h.flags, make([]byte, h.keyLen), make([]byte, h.payLen))
		if got := decodeHeader(buf); got != h {
			t.Fatalf("round trip = %+v, want %+v", got, h)
		}
	}
}

func TestKindBytes(t *testing.T) {
	if kindToByte(KindText) != 't' || kindToByte(KindBlob) != 'b' {
		t.Fatal("kindToByte returned the wrong byte")
	}
	if kindFromByte('t') != KindText || kindFromByte('b') != KindBlob {
		t.Fatal("kindFromByte returned the wrong kind")
	}
}

func TestCompressPayload(t *testing.T) {
	compressible := bytes.Repeat([]byte("queue "), 1000)
	stored, codec, err := compressPayload(compressible)
	if err != nil {
		t.Fatalf("compressPayload: %v", err)
	}
	if codec != codecGzip {
		t.Fatalf("codec = %d, want gzip", codec)
	}
	if len(stored) >= len(compressible) {
		t.Fatalf("gzip did not shrink: %d bytes for %d", len(stored), len(compressible))
	}
	got, err := decompressPayload(stored, codec)
	if err != nil {
		t.Fatalf("decompressPayload: %v", err)
	}
	if !bytes.Equal(got, compressible) {
		t.Fatalf("round trip changed %d bytes", len(compressible))
	}

	random := make([]byte, 10*1024)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	stored, codec, err = compressPayload(random)
	if err != nil {
		t.Fatalf("compressPayload: %v", err)
	}
	if codec != codecRaw {
		t.Fatalf("codec for random bytes = %d, want raw", codec)
	}
	got, err = decompressPayload(stored, codec)
	if err != nil {
		t.Fatalf("decompressPayload: %v", err)
	}
	if !bytes.Equal(got, random) {
		t.Fatalf("raw round trip changed %d bytes", len(random))
	}

	stored, codec, err = compressPayload(nil)
	if err != nil || codec != codecRaw || len(stored) != 0 {
		t.Fatalf("empty payload = %d bytes, codec %d, %v", len(stored), codec, err)
	}
}

func TestDecompressPayloadErrors(t *testing.T) {
	if _, err := decompressPayload([]byte("not gzip"), codecGzip); err == nil {
		t.Fatal("garbage gzip decoded without error")
	}
	if _, err := decompressPayload([]byte("x"), 9); err == nil {
		t.Fatal("unknown codec decoded without error")
	}
}

func TestKeywordBlob(t *testing.T) {
	blob := encodeKeywords([]string{"go", "body:queue"})
	if string(blob) != "go\nbody:queue" {
		t.Fatalf("blob = %q", blob)
	}
	if got := decodeKeywords(blob); !slices.Equal(got, []string{"go", "body:queue"}) {
		t.Fatalf("decode = %v", got)
	}
	if got := decodeKeywords(nil); got != nil {
		t.Fatalf("decode(nil) = %v, want nil", got)
	}
}
