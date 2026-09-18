package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureRun runs Run with os.Stdout and os.Stderr redirected to pipes and
// returns the exit code plus what each stream received.
func captureRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW

	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&outBuf, outR)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&errBuf, errR)
	}()

	code := Run(args)

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	wg.Wait()
	_ = outR.Close()
	_ = errR.Close()
	return code, outBuf.String(), errBuf.String()
}

// captureRunStdin runs Run with stdin fed from a temp file.
func captureRunStdin(t *testing.T, stdin []byte, args ...string) (int, string, string) {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(stdin); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	defer func() {
		os.Stdin = old
		_ = f.Close()
	}()
	return captureRun(t, args...)
}

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "trace8.db")
}

func putText(t *testing.T, db, keywords, text string) {
	t.Helper()
	code, out, errOut := captureRun(t, "db", "put", keywords, "--text", text, "--db", db)
	if code != 0 {
		t.Fatalf("put %q exit = %d, stderr = %q", keywords, code, errOut)
	}
	if out == "" {
		t.Fatalf("put %q printed no id", keywords)
	}
}

func putFile(t *testing.T, db, keywords, path string) {
	t.Helper()
	code, out, errOut := captureRun(t, "db", "put", keywords, "--file", path, "--db", db)
	if code != 0 {
		t.Fatalf("put %q exit = %d, stderr = %q", keywords, code, errOut)
	}
	if out == "" {
		t.Fatalf("put %q printed no id", keywords)
	}
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDBUsage(t *testing.T) {
	cases := [][]string{
		{"db"},
		{"db", "nope"},
		{"db", "get"},
		{"db", "get", "a", "b"},
		{"db", "get", "kw", "--raw"},
		{"db", "get", "kw", "--out", "out.bin"},
		{"db", "put"},
		{"db", "put", "-"},
		{"db", "put", "kw"},
		{"db", "put", "", "--text", "hi"},
		{"db", "put", "kw", "--text", "a", "--file", "b"},
		{"db", "put", "kw", "--text", "a", "-"},
		{"db", "put", "kw", "--kind", "nope", "--text", "a"},
		{"db", "del"},
		{"db", "del", "abc"},
		{"db", "stats", "extra"},
		{"db", "compact", "extra"},
	}
	for _, args := range cases {
		code, out, errOut := captureRun(t, args...)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2 (stderr %q)", args, code, errOut)
		}
		if !strings.Contains(errOut, "Usage: trace8 db") {
			t.Errorf("%v: stderr = %q, want usage", args, errOut)
		}
		if out != "" {
			t.Errorf("%v: stdout = %q, want empty", args, out)
		}
	}
}

func TestDBVersionWins(t *testing.T) {
	code, out, errOut := captureRun(t, "--version", "db")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "trace8 "+Version {
		t.Fatalf("stdout = %q, want %q", out, "trace8 "+Version)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
}

func TestDBPutTextAndGet(t *testing.T) {
	db := tempDBPath(t)

	// Flags before and after the positional argument both work.
	code, out, errOut := captureRun(t, "db", "put", "--db", db, "Go, db", "--text", "runs are slow")
	if code != 0 {
		t.Fatalf("put exit = %d, stderr = %q", code, errOut)
	}
	if out != "1\n" {
		t.Fatalf("put stdout = %q, want %q", out, "1\n")
	}

	code, out, errOut = captureRun(t, "db", "get", "go", "--db", db)
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, errOut)
	}
	want := "1 text 13 [go db]\nruns are slow\n"
	if out != want {
		t.Fatalf("get stdout = %q, want %q", out, want)
	}
}

func TestDBPutTextEmpty(t *testing.T) {
	db := tempDBPath(t)
	code, out, errOut := captureRun(t, "db", "put", "empty", "--text", "", "--db", db)
	if code != 0 {
		t.Fatalf("put exit = %d, stderr = %q", code, errOut)
	}
	if out != "1\n" {
		t.Fatalf("put stdout = %q, want %q", out, "1\n")
	}

	code, out, errOut = captureRun(t, "db", "get", "empty", "--db", db)
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, errOut)
	}
	want := "1 text 0 [empty]\n\n"
	if out != want {
		t.Fatalf("get stdout = %q, want %q", out, want)
	}
}

func TestDBPutFileBlob(t *testing.T) {
	db := tempDBPath(t)
	payload := []byte{0x00, 0x01, 0xfe, 0xff}
	src := writeTempFile(t, "blob.bin", payload)
	putFile(t, db, "blob", src)

	code, out, errOut := captureRun(t, "db", "get", "blob", "--db", db)
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, errOut)
	}
	want := "1 blob 4 [blob]\n"
	if out != want {
		t.Fatalf("get stdout = %q, want %q", out, want)
	}
}

func TestDBPutStdin(t *testing.T) {
	db := tempDBPath(t)
	code, out, errOut := captureRunStdin(t, []byte("from stdin\n"), "db", "put", "stdin", "-", "--db", db)
	if code != 0 {
		t.Fatalf("put exit = %d, stderr = %q", code, errOut)
	}
	if out != "1\n" {
		t.Fatalf("put stdout = %q, want %q", out, "1\n")
	}
	code, out, errOut = captureRun(t, "db", "get", "stdin", "--db", db)
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, errOut)
	}
	want := "1 text 11 [stdin]\nfrom stdin\n"
	if out != want {
		t.Fatalf("get stdout = %q, want %q", out, want)
	}

	// Binary stdin becomes a blob.
	binDB := tempDBPath(t)
	code, out, errOut = captureRunStdin(t, []byte{0x00, 0x01, 0x02}, "db", "put", "bin", "-", "--db", binDB)
	if code != 0 {
		t.Fatalf("binary put exit = %d, stderr = %q", code, errOut)
	}
	if out != "1\n" {
		t.Fatalf("binary put stdout = %q, want %q", out, "1\n")
	}
	code, out, errOut = captureRun(t, "db", "get", "bin", "--db", binDB)
	if code != 0 {
		t.Fatalf("binary get exit = %d, stderr = %q", code, errOut)
	}
	want = "1 blob 3 [bin]\n"
	if out != want {
		t.Fatalf("binary get stdout = %q, want %q", out, want)
	}
}

func TestDBPutKindOverride(t *testing.T) {
	db := tempDBPath(t)
	code, out, errOut := captureRunStdin(t, []byte("plain text"), "db", "put", "k", "-", "--kind", "blob", "--db", db)
	if code != 0 {
		t.Fatalf("put exit = %d, stderr = %q", code, errOut)
	}
	if out != "1\n" {
		t.Fatalf("put stdout = %q, want %q", out, "1\n")
	}
	code, out, errOut = captureRun(t, "db", "get", "k", "--db", db)
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, errOut)
	}
	want := "1 blob 10 [k]\n"
	if out != want {
		t.Fatalf("get stdout = %q, want %q", out, want)
	}
}

func TestDBGetUnionAndIntersection(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "a", "one")
	putText(t, db, "b", "two")
	putText(t, db, "a,b", "three")

	code, out, errOut := captureRun(t, "db", "get", "a,b", "--db", db)
	if code != 0 {
		t.Fatalf("union get exit = %d, stderr = %q", code, errOut)
	}
	want := "1 text 3 [a]\none\n2 text 3 [b]\ntwo\n3 text 5 [a b]\nthree\n"
	if out != want {
		t.Fatalf("union stdout = %q, want %q", out, want)
	}

	code, out, errOut = captureRun(t, "db", "get", "a,b", "--all", "--db", db)
	if code != 0 {
		t.Fatalf("intersection get exit = %d, stderr = %q", code, errOut)
	}
	want = "3 text 5 [a b]\nthree\n"
	if out != want {
		t.Fatalf("intersection stdout = %q, want %q", out, want)
	}
}

func TestDBGetByID(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "a", "one")
	putText(t, db, "b", "two")

	code, out, errOut := captureRun(t, "db", "get", "--id", "2", "--db", db)
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, errOut)
	}
	want := "2 text 3 [b]\ntwo\n"
	if out != want {
		t.Fatalf("get stdout = %q, want %q", out, want)
	}

	code, out, errOut = captureRun(t, "db", "get", "--id", "99", "--db", db)
	if code != 1 {
		t.Fatalf("unknown id exit = %d, want 1", code)
	}
	if out != "" {
		t.Fatalf("unknown id stdout = %q, want empty", out)
	}
	if errOut != "trace8 db: no record 99\n" {
		t.Fatalf("unknown id stderr = %q", errOut)
	}
}

func TestDBGetRaw(t *testing.T) {
	db := tempDBPath(t)
	payload := []byte{0x00, 0x01, 0xff, 'x'}
	putFile(t, db, "bin", writeTempFile(t, "raw.bin", payload))

	code, out, errOut := captureRun(t, "db", "get", "--id", "1", "--raw", "--db", db)
	if code != 0 {
		t.Fatalf("raw exit = %d, stderr = %q", code, errOut)
	}
	if !bytes.Equal([]byte(out), payload) {
		t.Fatalf("raw bytes = %v, want %v", []byte(out), payload)
	}
}

func TestDBGetOut(t *testing.T) {
	db := tempDBPath(t)
	payload := []byte{0x00, 0x01, 0xff, 'x'}
	putFile(t, db, "bin", writeTempFile(t, "raw.bin", payload))

	dst := filepath.Join(t.TempDir(), "copy.bin")
	code, out, errOut := captureRun(t, "db", "get", "--id", "1", "--out", dst, "--db", db)
	if code != 0 {
		t.Fatalf("out exit = %d, stderr = %q", code, errOut)
	}
	want := fmt.Sprintf("wrote %d bytes to %s\n", len(payload), dst)
	if out != want {
		t.Fatalf("out stdout = %q, want %q", out, want)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("out file = %v, want %v", got, payload)
	}
}

func TestDBGetLimit(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "k", "one")
	putText(t, db, "k", "two")
	putText(t, db, "k", "three")

	// A limit prints newest first and footers when it hid matches.
	code, out, errOut := captureRun(t, "db", "get", "k", "-n", "2", "--db", db)
	if code != 0 {
		t.Fatalf("get -n 2 exit = %d, stderr = %q", code, errOut)
	}
	want := "3 text 5 [k]\nthree\n2 text 3 [k]\ntwo\nshowing 2 of 3 matches\n"
	if out != want {
		t.Fatalf("get -n 2 stdout = %q, want %q", out, want)
	}

	// A limit above the match count still prints newest first, with no
	// footer because nothing was hidden.
	code, out, errOut = captureRun(t, "db", "get", "k", "-n", "5", "--db", db)
	if code != 0 {
		t.Fatalf("get -n 5 exit = %d, stderr = %q", code, errOut)
	}
	want = "3 text 5 [k]\nthree\n2 text 3 [k]\ntwo\n1 text 3 [k]\none\n"
	if out != want {
		t.Fatalf("get -n 5 stdout = %q, want %q", out, want)
	}

	// A limit equal to the match count hides nothing, so no footer.
	code, out, errOut = captureRun(t, "db", "get", "k", "-n", "3", "--db", db)
	if code != 0 {
		t.Fatalf("get -n 3 exit = %d, stderr = %q", code, errOut)
	}
	want = "3 text 5 [k]\nthree\n2 text 3 [k]\ntwo\n1 text 3 [k]\none\n"
	if out != want {
		t.Fatalf("get -n 3 stdout = %q, want %q", out, want)
	}

	// Without -n the order stays ascending and the footer is absent.
	code, out, errOut = captureRun(t, "db", "get", "k", "--db", db)
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, errOut)
	}
	want = "1 text 3 [k]\none\n2 text 3 [k]\ntwo\n3 text 5 [k]\nthree\n"
	if out != want {
		t.Fatalf("get stdout = %q, want %q", out, want)
	}
}

func TestDBDoubleDashTerminator(t *testing.T) {
	// A "--" stops flag parsing; the terminator and everything after it
	// are positional, so these all carry too many positionals.
	cases := [][]string{
		{"db", "get", "--", "--all"},
		{"db", "get", "a,b", "--", "--all"},
		{"db", "put", "k", "--", "--db", "x"},
	}
	for _, args := range cases {
		code, out, errOut := captureRun(t, args...)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2 (stderr %q)", args, code, errOut)
		}
		if !strings.Contains(errOut, "Usage: trace8 db") {
			t.Errorf("%v: stderr = %q, want usage", args, errOut)
		}
		if out != "" {
			t.Errorf("%v: stdout = %q, want empty", args, out)
		}
	}
}

func TestDBDoubleDashFlagsNotReparsed(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "k", "keep")

	// Everything after -- is positional, so --db is not read and the
	// delete fails as a usage error instead of deleting record 1.
	code, out, errOut := captureRun(t, "db", "del", "--", "1", "--db", db)
	if code != 2 {
		t.Fatalf("del exit = %d, want 2 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "Usage: trace8 db") {
		t.Fatalf("del stderr = %q, want usage", errOut)
	}
	if out != "" {
		t.Fatalf("del stdout = %q, want empty", out)
	}

	code, out, errOut = captureRun(t, "db", "get", "--id", "1", "--db", db)
	if code != 0 || out != "1 text 4 [k]\nkeep\n" {
		t.Fatalf("get exit = %d stdout = %q stderr = %q, want record 1 intact", code, out, errOut)
	}
}

func TestDBGetKeywordNormalization(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "go", "fast")

	want := "1 text 4 [go]\nfast\n"
	for _, keyword := range []string{"GO", " Go "} {
		code, out, errOut := captureRun(t, "db", "get", keyword, "--db", db)
		if code != 0 {
			t.Fatalf("get %q exit = %d, stderr = %q", keyword, code, errOut)
		}
		if out != want {
			t.Fatalf("get %q stdout = %q, want %q", keyword, out, want)
		}
	}
}

func TestDBPutReservedBodyKeyword(t *testing.T) {
	db := tempDBPath(t)
	code, out, errOut := captureRun(t, "db", "put", "body:queue", "--text", "x", "--db", db)
	if code != 1 {
		t.Fatalf("put exit = %d, want 1 (stderr %q)", code, errOut)
	}
	if out != "" {
		t.Fatalf("put stdout = %q, want empty", out)
	}
	if !strings.HasPrefix(errOut, "trace8 db: ") || !strings.Contains(errOut, "body:") {
		t.Fatalf("put stderr = %q, want the reserved body: prefix named", errOut)
	}
}

func TestDBGetBodySearch(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "notes", "the queue is empty")

	code, out, errOut := captureRun(t, "db", "get", "body:queue", "--db", db)
	if code != 0 {
		t.Fatalf("body get exit = %d, stderr = %q", code, errOut)
	}
	want := "1 text 18 [notes]\nthe queue is empty\n"
	if out != want {
		t.Fatalf("body get stdout = %q, want %q", out, want)
	}
}

func TestDBGetNoMatches(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "here", "x")

	code, out, errOut := captureRun(t, "db", "get", "there", "--db", db)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if errOut != "trace8 db: no records for keyword \"there\"\n" {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestDBDel(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "k", "gone")

	code, out, errOut := captureRun(t, "db", "del", "1", "--db", db)
	if code != 0 {
		t.Fatalf("del exit = %d, stderr = %q", code, errOut)
	}
	if out != "deleted 1\n" {
		t.Fatalf("del stdout = %q, want %q", out, "deleted 1\n")
	}
	if errOut != "" {
		t.Fatalf("del stderr = %q, want empty", errOut)
	}

	code, out, errOut = captureRun(t, "db", "del", "1", "--db", db)
	if code != 1 {
		t.Fatalf("second del exit = %d, want 1", code)
	}
	if out != "" {
		t.Fatalf("second del stdout = %q, want empty", out)
	}
	if errOut != "trace8 db: no record 1\n" {
		t.Fatalf("second del stderr = %q", errOut)
	}
}

func TestDBStats(t *testing.T) {
	db := tempDBPath(t)
	code, out, errOut := captureRun(t, "db", "stats", "--db", db)
	if code != 0 {
		t.Fatalf("stats exit = %d, stderr = %q", code, errOut)
	}
	want := "records: 0\nkeywords: 0\ndeleted: 0\nfile: " + db + " (0 bytes)\n"
	if out != want {
		t.Fatalf("stats stdout = %q, want %q", out, want)
	}

	putText(t, db, "a,b", "hello")
	fi, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut = captureRun(t, "db", "stats", "--db", db)
	if code != 0 {
		t.Fatalf("stats exit = %d, stderr = %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("stats stdout = %q, want 4 lines", out)
	}
	if lines[0] != "records: 1" || lines[2] != "deleted: 0" {
		t.Fatalf("stats stdout = %q, want records: 1 and deleted: 0", out)
	}
	if !strings.HasPrefix(lines[1], "keywords: ") || lines[1] == "keywords: 0" {
		t.Fatalf("stats keywords line = %q, want a positive count", lines[1])
	}
	wantLine := fmt.Sprintf("file: %s (%d bytes)", db, fi.Size())
	if lines[3] != wantLine {
		t.Fatalf("stats file line = %q, want %q", lines[3], wantLine)
	}
}

func TestDBCompact(t *testing.T) {
	db := tempDBPath(t)
	putText(t, db, "k", "one")
	if code, _, errOut := captureRun(t, "db", "del", "1", "--db", db); code != 0 {
		t.Fatalf("del exit = %d, stderr = %q", code, errOut)
	}

	code, out, errOut := captureRun(t, "db", "compact", "--db", db)
	if code != 0 {
		t.Fatalf("compact exit = %d, stderr = %q", code, errOut)
	}
	want := "compacted " + db + "\n"
	if out != want {
		t.Fatalf("compact stdout = %q, want %q", out, want)
	}
	if errOut != "" {
		t.Fatalf("compact stderr = %q, want empty", errOut)
	}

	code, out, errOut = captureRun(t, "db", "stats", "--db", db)
	if code != 0 {
		t.Fatalf("stats after compact exit = %d, stderr = %q", code, errOut)
	}
	if !strings.HasPrefix(out, "records: 0\n") {
		t.Fatalf("stats after compact stdout = %q, want zero records", out)
	}
}

func TestDBBadPath(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing", "trace8.db")
	code, out, errOut := captureRun(t, "db", "put", "k", "--text", "x", "--db", bad)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.HasPrefix(errOut, "trace8 db: ") {
		t.Fatalf("stderr = %q, want trace8 db: prefix", errOut)
	}
}
