package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chinmay-sawant/trace8/internal/store"
)

// dbUsageText describes the trace8 db command tree and every flag.
const dbUsageText = `Usage: trace8 db <command> [flags]

Commands:
  put <k1,k2,...>   store a payload from --text, --file, or - on stdin
  get <keyword>     print matching records
  del <id>          delete a record by id
  stats             print record count, keyword count, and file size
  compact           rewrite the database file without dead data

Common flags:
  --db string       database file (default "trace8.db")
  --remote string   talk to a running "trace8 --server" instead of opening
                    the file, for example http://127.0.0.1:8080

put flags:
  --text string     text payload
  --file path       file payload
  --kind text|blob  override the payload kind

get flags:
  --all             require every keyword (default: any keyword)
  --id N            print only record N
  --raw             write the payload bytes to stdout (needs --id)
  --out path        write the payload bytes to a file (needs --id)
  -n N              show at most N matches, newest first (default 0 = all)
`

func dbUsage() {
	fmt.Fprint(os.Stderr, dbUsageText)
}

// backend is the storage surface shared by the local store and the remote
// HTTP client, so every db command works the same way in both modes.
type backend interface {
	Put(keywords []string, kind store.Kind, payload []byte) (uint64, error)
	Get(keyword string) ([]store.Record, error)
	GetByID(id uint64) (store.Record, bool, error)
	Delete(id uint64) (bool, error)
	Stats() (store.Stats, error)
	Compact() error
	Close() error
}

// localBackend adapts *store.Store to backend.
type localBackend struct {
	s *store.Store
}

func (b *localBackend) Put(keywords []string, kind store.Kind, payload []byte) (uint64, error) {
	return b.s.Put(keywords, kind, payload)
}

func (b *localBackend) Get(keyword string) ([]store.Record, error) {
	return b.s.Get(keyword)
}

func (b *localBackend) GetByID(id uint64) (store.Record, bool, error) {
	rec, ok := b.s.GetByID(id)
	return rec, ok, nil
}

func (b *localBackend) Delete(id uint64) (bool, error) {
	return b.s.Delete(id)
}

func (b *localBackend) Stats() (store.Stats, error) {
	return b.s.Stats(), nil
}

func (b *localBackend) Compact() error {
	return b.s.Compact()
}

func (b *localBackend) Close() error {
	return b.s.Close()
}

// remoteBackend talks to the /api/db endpoints of a running trace8 server.
type remoteBackend struct {
	base   string
	client *http.Client
}

func newRemoteBackend(base string) *remoteBackend {
	return &remoteBackend{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (b *remoteBackend) Put(keywords []string, kind store.Kind, payload []byte) (uint64, error) {
	body := struct {
		Keywords []string   `json:"keywords"`
		Kind     store.Kind `json:"kind"`
		Payload  []byte     `json:"payload"`
	}{
		Keywords: keywords,
		Kind:     kind,
		Payload:  payload,
	}
	var out struct {
		ID uint64 `json:"id"`
	}
	if _, err := b.do(http.MethodPost, "/api/db/put", body, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

func (b *remoteBackend) Get(keyword string) ([]store.Record, error) {
	q := url.Values{}
	q.Set("tag", keyword)
	var out struct {
		Records []store.Record `json:"records"`
	}
	if _, err := b.do(http.MethodGet, "/api/db/get?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Records, nil
}

func (b *remoteBackend) GetByID(id uint64) (store.Record, bool, error) {
	q := url.Values{}
	q.Set("id", strconv.FormatUint(id, 10))
	var out struct {
		Record store.Record `json:"record"`
	}
	status, err := b.do(http.MethodGet, "/api/db/record?"+q.Encode(), nil, &out)
	if status == http.StatusNotFound {
		return store.Record{}, false, nil
	}
	if err != nil {
		return store.Record{}, false, err
	}
	return out.Record, true, nil
}

func (b *remoteBackend) Delete(id uint64) (bool, error) {
	q := url.Values{}
	q.Set("id", strconv.FormatUint(id, 10))
	var out struct {
		Deleted bool `json:"deleted"`
	}
	if _, err := b.do(http.MethodDelete, "/api/db/record?"+q.Encode(), nil, &out); err != nil {
		return false, err
	}
	return out.Deleted, nil
}

func (b *remoteBackend) Stats() (store.Stats, error) {
	var out store.Stats
	if _, err := b.do(http.MethodGet, "/api/db/stats", nil, &out); err != nil {
		return store.Stats{}, err
	}
	return out, nil
}

func (b *remoteBackend) Compact() error {
	_, err := b.do(http.MethodPost, "/api/db/compact", nil, nil)
	return err
}

func (b *remoteBackend) Close() error {
	return nil
}

// do sends a JSON request and decodes the JSON reply. It returns the HTTP
// status so callers can treat 404 as "missing" instead of an error. Failed
// responses surface the server's {"error":"..."} string.
func (b *remoteBackend) do(method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, b.base+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return resp.StatusCode, errors.New(e.Error)
		}
		return resp.StatusCode, fmt.Errorf("remote %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// runDB dispatches the trace8 db subcommands.
func runDB(args []string) int {
	if len(args) == 0 {
		dbUsage()
		return 2
	}
	switch args[0] {
	case "put":
		return runDBPut(args[1:])
	case "get":
		return runDBGet(args[1:])
	case "del":
		return runDBDel(args[1:])
	case "stats":
		return runDBStats(args[1:])
	case "compact":
		return runDBCompact(args[1:])
	default:
		dbUsage()
		return 2
	}
}

// newDBFlags builds a subcommand flag set carrying the common --db and
// --remote flags. Usage and parse errors are suppressed; callers print the
// db usage and exit 2 themselves.
func newDBFlags(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", "trace8.db", "database file")
	remote := fs.String("remote", "", "server base URL")
	return fs, db, remote
}

// parseArgs parses flags and positionals in any order. The stdlib flag
// package stops at the first positional argument, so this loops: parse,
// take one positional, parse the rest again. A "--" terminator stops flag
// parsing for good: the terminator and everything after it come back as
// positionals and are never re-parsed as flags.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	var terminator []string
	rest := args
	for len(rest) > 0 {
		if len(terminator) == 0 {
			if i := terminatorIndex(fs, rest); i >= 0 {
				terminator = rest[i:]
				rest = rest[:i]
				continue
			}
		}
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	return append(positional, terminator...), nil
}

// terminatorIndex returns the index of the first "--" in args that flag
// parsing would treat as the end of the flags, or -1 when there is none.
// A "--" consumed as a flag value, as in "--text --", is not a terminator.
func terminatorIndex(fs *flag.FlagSet, args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return i
		}
		if len(arg) < 2 || arg[0] != '-' {
			continue // "-" and bare words are positional
		}
		name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		if strings.ContainsRune(name, '=') {
			continue // the value is inline, nothing to skip
		}
		f := fs.Lookup(name)
		if f == nil {
			continue // let fs.Parse report the unknown flag
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !bf.IsBoolFlag() {
			i++ // this flag takes the next argument as its value
		}
	}
	return -1
}

// flagWasSet reports whether the named flag appeared on the command line,
// so an explicitly empty --text counts as a source while an unset --text
// does not.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// openBackend opens the local store or a remote client, depending on --remote.
func openBackend(dbPath, remote string) (backend, error) {
	if remote != "" {
		return newRemoteBackend(remote), nil
	}
	s, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &localBackend{s: s}, nil
}

func dbError(err error) {
	fmt.Fprintln(os.Stderr, "trace8 db:", err)
}

func runDBPut(args []string) int {
	fs, dbPath, remote := newDBFlags("trace8 db put")
	text := fs.String("text", "", "text payload")
	file := fs.String("file", "", "file payload")
	kindFlag := fs.String("kind", "", "override the payload kind")
	rest, err := parseArgs(fs, args)
	if err != nil {
		dbUsage()
		return 2
	}
	if len(rest) == 0 || rest[0] == "-" || len(rest) > 2 {
		dbUsage()
		return 2
	}
	stdin := false
	if len(rest) == 2 {
		if rest[1] != "-" {
			dbUsage()
			return 2
		}
		stdin = true
	}
	textSet := flagWasSet(fs, "text")
	fileSet := flagWasSet(fs, "file")
	sources := 0
	if textSet {
		sources++
	}
	if fileSet {
		sources++
	}
	if stdin {
		sources++
	}
	if sources != 1 {
		dbUsage()
		return 2
	}
	kind := store.KindText
	var payload []byte
	switch {
	case textSet:
		payload = []byte(*text)
	case fileSet:
		data, err := os.ReadFile(*file)
		if err != nil {
			dbError(err)
			return 1
		}
		payload = data
		kind = store.KindBlob
	default:
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			dbError(err)
			return 1
		}
		payload = data
		kind = detectKind(data)
	}
	keywords := strings.Split(rest[0], ",")
	if !hasKeyword(keywords) {
		dbUsage()
		return 2
	}
	// The store adds body: keywords itself when it indexes a text
	// payload, so a user supplied body: keyword would collide with that
	// index. Reject it here, with the trim and lowercase rules the store
	// applies, before the store sees it.
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if strings.HasPrefix(kw, "body:") {
			dbError(fmt.Errorf("keyword %q uses the reserved body: prefix", kw))
			return 1
		}
	}
	if *kindFlag != "" {
		switch *kindFlag {
		case string(store.KindText):
			kind = store.KindText
		case string(store.KindBlob):
			kind = store.KindBlob
		default:
			dbUsage()
			return 2
		}
	}
	be, err := openBackend(*dbPath, *remote)
	if err != nil {
		dbError(err)
		return 1
	}
	defer func() { _ = be.Close() }()
	id, err := be.Put(keywords, kind, payload)
	if err != nil {
		dbError(err)
		return 1
	}
	fmt.Println(id)
	return 0
}

// hasKeyword reports whether at least one comma-split keywords entry is
// non-blank. The store trims, lowercases, and dedupes the final list.
func hasKeyword(keywords []string) bool {
	for _, kw := range keywords {
		if strings.TrimSpace(kw) != "" {
			return true
		}
	}
	return false
}

// detectKind calls stdin data text when it is valid UTF-8 without NUL bytes.
func detectKind(data []byte) store.Kind {
	if utf8.Valid(data) && bytes.IndexByte(data, 0) < 0 {
		return store.KindText
	}
	return store.KindBlob
}

func runDBGet(args []string) int {
	fs, dbPath, remote := newDBFlags("trace8 db get")
	all := fs.Bool("all", false, "require every keyword")
	id := fs.Uint64("id", 0, "print only record N")
	raw := fs.Bool("raw", false, "write the payload bytes to stdout")
	out := fs.String("out", "", "write the payload bytes to a file")
	limit := fs.Int("n", 0, "show at most N matches, newest first")
	rest, err := parseArgs(fs, args)
	if err != nil {
		dbUsage()
		return 2
	}
	idSet := flagWasSet(fs, "id")
	rawSet := flagWasSet(fs, "raw")
	outSet := flagWasSet(fs, "out")
	if *limit < 0 || (rawSet && outSet) || ((rawSet || outSet) && !idSet) ||
		(!idSet && len(rest) != 1) || (idSet && len(rest) > 1) {
		dbUsage()
		return 2
	}
	be, err := openBackend(*dbPath, *remote)
	if err != nil {
		dbError(err)
		return 1
	}
	defer func() { _ = be.Close() }()

	if idSet {
		rec, ok, err := be.GetByID(*id)
		if err != nil {
			dbError(err)
			return 1
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "trace8 db: no record %d\n", *id)
			return 1
		}
		switch {
		case *raw:
			if _, err := os.Stdout.Write(rec.Payload); err != nil {
				dbError(err)
				return 1
			}
		case outSet:
			if err := os.WriteFile(*out, rec.Payload, 0o644); err != nil {
				dbError(err)
				return 1
			}
			fmt.Printf("wrote %d bytes to %s\n", len(rec.Payload), *out)
		default:
			printRecord(os.Stdout, rec)
		}
		return 0
	}

	keywords := strings.Split(rest[0], ",")
	ids := make(map[uint64]struct{})
	byID := make(map[uint64]store.Record)
	for i, keyword := range keywords {
		records, err := be.Get(keyword)
		if err != nil {
			dbError(err)
			return 1
		}
		current := make(map[uint64]struct{}, len(records))
		for _, rec := range records {
			current[rec.ID] = struct{}{}
			byID[rec.ID] = rec
		}
		switch {
		case i == 0:
			ids = current
		case *all:
			ids = intersect(ids, current)
		default:
			for id := range current {
				ids[id] = struct{}{}
			}
		}
	}
	if len(ids) == 0 {
		if len(keywords) == 1 {
			fmt.Fprintf(os.Stderr, "trace8 db: no records for keyword %q\n", keywords[0])
		} else {
			fmt.Fprintf(os.Stderr, "trace8 db: no records for keywords %q\n", strings.Join(keywords, ","))
		}
		return 1
	}
	total := len(ids)
	records := make([]store.Record, 0, total)
	for id := range ids {
		records = append(records, byID[id])
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	if *limit > 0 {
		// A limit always prints newest first, and the footer appears
		// only when the limit hid matching records.
		sort.Slice(records, func(i, j int) bool { return records[i].ID > records[j].ID })
		if *limit < len(records) {
			records = records[:*limit]
		}
	}
	for _, rec := range records {
		printRecord(os.Stdout, rec)
	}
	if *limit > 0 && total > *limit {
		fmt.Printf("showing %d of %d matches\n", len(records), total)
	}
	return 0
}

// printRecord writes one summary line and, for text records, the payload
// with a guaranteed trailing newline. Blob payloads are never printed.
// The summary lists the tag keywords the user supplied; the body: entries
// the store derives from text payloads stay hidden (plan 5.2).
func printRecord(w io.Writer, rec store.Record) {
	fmt.Fprintf(w, "%d %s %d [%s]\n", rec.ID, rec.Kind, rec.Size, strings.Join(displayKeywords(rec.Keywords), " "))
	if rec.Kind != store.KindText {
		return
	}
	_, _ = w.Write(rec.Payload)
	if len(rec.Payload) == 0 || rec.Payload[len(rec.Payload)-1] != '\n' {
		_, _ = w.Write([]byte{'\n'})
	}
}

// displayKeywords drops the body: entries a text payload feeds into the
// store index, so get output matches the keywords the user typed.
func displayKeywords(keywords []string) []string {
	out := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if strings.HasPrefix(kw, "body:") {
			continue
		}
		out = append(out, kw)
	}
	return out
}

func intersect(a, b map[uint64]struct{}) map[uint64]struct{} {
	out := make(map[uint64]struct{})
	for id := range a {
		if _, ok := b[id]; ok {
			out[id] = struct{}{}
		}
	}
	return out
}

func runDBDel(args []string) int {
	fs, dbPath, remote := newDBFlags("trace8 db del")
	rest, err := parseArgs(fs, args)
	if err != nil || len(rest) != 1 {
		dbUsage()
		return 2
	}
	id, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil {
		dbUsage()
		return 2
	}
	be, err := openBackend(*dbPath, *remote)
	if err != nil {
		dbError(err)
		return 1
	}
	defer func() { _ = be.Close() }()
	ok, err := be.Delete(id)
	if err != nil {
		dbError(err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "trace8 db: no record %d\n", id)
		return 1
	}
	fmt.Printf("deleted %d\n", id)
	return 0
}

func runDBStats(args []string) int {
	fs, dbPath, remote := newDBFlags("trace8 db stats")
	rest, err := parseArgs(fs, args)
	if err != nil || len(rest) != 0 {
		dbUsage()
		return 2
	}
	be, err := openBackend(*dbPath, *remote)
	if err != nil {
		dbError(err)
		return 1
	}
	defer func() { _ = be.Close() }()
	st, err := be.Stats()
	if err != nil {
		dbError(err)
		return 1
	}
	fmt.Printf("records: %d\n", st.Records)
	fmt.Printf("keywords: %d\n", st.Keywords)
	fmt.Printf("deleted: %d\n", st.Deleted)
	label := *dbPath
	if *remote != "" {
		// The byte count comes from the server, so label it with the
		// server the user pointed at, not the unused local path.
		label = strings.TrimSuffix(*remote, "/")
	}
	fmt.Printf("file: %s (%d bytes)\n", label, st.FileBytes)
	return 0
}

func runDBCompact(args []string) int {
	fs, dbPath, remote := newDBFlags("trace8 db compact")
	rest, err := parseArgs(fs, args)
	if err != nil || len(rest) != 0 {
		dbUsage()
		return 2
	}
	be, err := openBackend(*dbPath, *remote)
	if err != nil {
		dbError(err)
		return 1
	}
	defer func() { _ = be.Close() }()
	if err := be.Compact(); err != nil {
		dbError(err)
		return 1
	}
	fmt.Printf("compacted %s\n", *dbPath)
	return 0
}
