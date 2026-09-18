# trace8 0.0.1 - Local keyword store (`trace8 db`)

> **Parent:** none (first plan under `plans/`; repo conventions live in `AGENTS.md`)
> **Status:** planned, not started. No row is closed; deferred rows are marked `[~]` with their trigger.
> **Estimated effort:** Phase 1 about half a day, Phase 2 about half a day, Phase 6 about an hour. Phases 3 to 5 are deferred behind explicit triggers.

---

## Overview

The goal: a local store inside trace8 that keeps blobs and text, attaches keywords to each record, and finds records by any one keyword fast. No SQL, no new dependencies; `go.mod:1-5` currently requires nothing.

The design conversation settled on this shape:

- A record is an id plus keywords plus a payload. The payload is `[]byte`, so paragraphs and binary blobs share one code path. A `kind` field (`text` or `blob`) affects display only.
- The index is inverted: `keyword -> record ids`. Lookups are map reads.
- The default engine is shape A: a single snapshot file, gob-encoded and gzipped, rewritten atomically on save.
- The default front door is the one-shot CLI (`trace8 db ...`). A resident server mode that keeps the index in RAM across commands is Phase 3, deferred.
- Retrieval speed comes from holding the index in memory. Compression only shrinks the file and shortens load time.

Example of the intended use:

```
$ trace8 db put go,db,notes --text "runs are slow when the queue is empty"
1
$ trace8 db get db
1 text 41 [db go notes]
runs are slow when the queue is empty
```

This file is the single canonical ledger for the effort. Rationale lives in "Design reference" near the end; everything that needs doing lives in the phases.

## Executive Summary

Phase 1 builds `internal/store`: `Open`/`Close` lifecycle with a lock file, the inverted index, keyword normalization, the snapshot format (magic bytes, format version, gob, whole-file gzip), atomic temp-file-plus-rename saves, tests for every failure mode, and the benchmark that backs the "quick retrieval" claim.

Phase 2 builds the CLI in `internal/cli`: a `db` subcommand with `put`, `get`, `del`, and `stats`, exact output formats and exit codes, and tests that reuse the existing `captureStdout` pattern (`internal/cli/cli_test.go:10-32`).

Phases 3, 4, and 5 stay deferred until their triggers fire: resident server mode, the append-log engine with per-record compression, and full-text body search. Each phase states its reason, owner boundary, and next gate.

Phase 6 closes: full gates, `AGENTS.md` updates, dependency audit, and version discipline.

## Ground rules

These apply to every row in every phase.

- Standard library only. `go.mod:1-5` requires nothing; adding a dependency needs explicit user sign-off (`AGENTS.md`, Dependency policy).
- Tests live next to the code, use `t.TempDir()`, and open no ports. Style follows `internal/server/server_test.go:13-20` (temp dir, `httptest` for HTTP) and `internal/cli/cli_test.go:10-32` (stdout capture).
- A row closes only when its proof command ran on the current revision and the result is pasted into this file. No closing from intent (golden rule 6).
- Errors are lowercase, plain, and name the path, id, or keyword involved.
- Formatting is `gofmt` only; `make fmt-check` must stay clean.
- No em dashes in code comments, CLI output, or docs.
- Nothing in this plan touches `internal/server` until Phase 3 activates. One package per phase, one writer at a time on the repo.

## Decisions

Four answers shape scope. Each row states the default, so Phase 1 and Phase 2 can start immediately.

- [ ] D1 Search scope per record: tags only (default) or tags plus words from the body text. Owner: user. Next gate: recorded here, then Phase 5 stays deferred or activates.
- [ ] D2 Primary usage mode: one-shot CLI (default) or resident `--server` with the store open in memory. Owner: user. Next gate: recorded here, then Phase 3 stays deferred or activates.
- [ ] D3 `get a,b` meaning: union of the two keyword sets (default, what you described) or intersection (a `--all` flag, planned in row 2.4). Owner: user. Next gate: recorded here before row 2.4.
- [ ] D4 Payload size policy: keep every payload in RAM (default, fine into the low hundreds of MB) or offload payloads above a threshold to disk offsets. Owner: user. Next gate: recorded here before row 1.6.

## Phase 1: Store core (`internal/store`)

Scale estimate: about 350 lines of code and 300 lines of tests across the files below.

| File | Contents | Approx lines |
|---|---|---|
| `doc.go` | package comment, one paragraph | 10 |
| `store.go` | `Store`, `Open`, `Close`, lock, save policy | 130 |
| `index.go` | `index`, `Record`, `Kind`, put/get/del/rebuild | 110 |
| `codec.go` | magic, version, snapshot struct, read/write | 110 |
| `store_test.go` | lifecycle, index, normalization tests | 170 |
| `codec_test.go` | round trip, corrupt file, atomic save | 100 |
| `bench_test.go` | `Get` and `Open` benchmarks | 70 |

### 1.1 Package doc and layout
- [ ] Create `internal/store/doc.go` with a short package comment: what the store is, that it is a single snapshot file rewritten atomically, and that it allows one writer at a time. Proof: `go doc ./internal/store` prints the comment.

### 1.2 Types and public surface
- [ ] Create `internal/store/index.go` with these types and methods. Keep the surface this small; new methods need a plan row.

```go
// Kind separates display behavior only; storage is identical.
type Kind string

const (
    KindText Kind = "text"
    KindBlob Kind = "blob"
)

type Record struct {
    ID       uint64
    Kind     Kind
    Keywords []string // normalized: lowercase, trimmed, deduped
    Size     int64    // len(Payload) when stored
    Payload  []byte   // decoded bytes; shared, callers must not mutate
}

type Stats struct {
    Records  int
    Keywords int
}
```

- [ ] Create `internal/store/store.go` with this public surface:

```go
func Open(path string) (*Store, error)
func (s *Store) Put(keywords []string, kind Kind, payload []byte) (uint64, error)
func (s *Store) Get(keyword string) ([]Record, error)
func (s *Store) Delete(id uint64) (bool, error) // false when the id is unknown
func (s *Store) Stats() Stats
func (s *Store) Close() error
```

Proof: `go build ./...` succeeds; `go doc ./internal/store` lists exactly these symbols.

### 1.3 Lifecycle, lock, and the close-save model
- [ ] Implement `Open` with this behavior:
  - missing file -> empty store, no error;
  - present file -> read, verify magic and version, decode, rebuild the index (no index on disk; records are the single source of truth);
  - lock: create `path + ".lock"` with `os.OpenFile(lock, os.O_CREATE|os.O_EXCL, 0o644)`. If it exists, return `store: %s is locked by another process (remove %s if it is stale)`.
- [ ] Implement `Close`: save when the store is dirty, then remove the lock file. Writes are not durable until `Close`; every CLI command calls `Close` before exiting, which makes each one-shot command durable.

```go
func Open(path string) (*Store, error) {
    lock, err := acquireLock(path + ".lock")
    if err != nil {
        return nil, err
    }
    s := &Store{path: path, lockName: lock, index: newIndex()}
    data, err := os.ReadFile(path)
    if errors.Is(err, os.ErrNotExist) {
        return s, nil // empty store
    }
    if err != nil {
        s.releaseLock()
        return nil, err
    }
    snap, err := readSnapshot(bytes.NewReader(data))
    if err != nil {
        s.releaseLock()
        return nil, err
    }
    s.index.rebuild(snap.Records, snap.NextID)
    return s, nil
}
```

Proof: `go test ./internal/store -run 'TestOpen'` with cases: create new, reopen after save, second open while locked fails, missing parent directory fails with a clear error, stale lock removal note printed in the error text.

### 1.4 In-memory index
- [ ] Implement the index over two maps, with ascending-id results so output is deterministic.

```go
type index struct {
    nextID    uint64            // next id to hand out; starts at 1
    byKeyword map[string][]uint64
    records   map[uint64]Record
}

func (ix *index) put(kw []string, kind Kind, payload []byte) uint64 {
    id := ix.nextID
    ix.nextID++
    ix.records[id] = Record{ID: id, Kind: kind, Keywords: kw,
        Size: int64(len(payload)), Payload: payload}
    for _, k := range kw {
        ix.byKeyword[k] = append(ix.byKeyword[k], id)
    }
    return id
}

func (ix *index) get(kw string) []Record {
    ids := ix.byKeyword[kw] // already ascending: ids only grow
    out := make([]Record, 0, len(ids))
    for _, id := range ids {
        out = append(out, ix.records[id])
    }
    return out
}
```

- [ ] `Delete` removes the record and its postings; drop a keyword entry when its list becomes empty. `rebuild` recreates both maps from a decoded slice and sets `nextID = max(id)+1`. Proof: table tests for put/get/delete, one record under two keywords, delete removing the keyword entry, and delete of an unknown id returning `false, nil`.

### 1.5 Keyword normalization
- [ ] Normalize at the store boundary (the CLI splits on commas before this point), and reject empty results.

```go
var ErrNoKeywords = errors.New("no keywords given")

func normalizeKeywords(raw []string) ([]string, error) {
    seen := make(map[string]struct{}, len(raw))
    out := make([]string, 0, len(raw))
    for _, kw := range raw {
        kw = strings.ToLower(strings.TrimSpace(kw))
        if kw == "" {
            continue
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
```

Proof: table test with these rows:

| input | want keywords | want error |
|---|---|---|
| `["Go", " go ", "DB"]` | `["go", "db"]` | nil |
| `[""]` | nil | `ErrNoKeywords` |
| `["  ", "\t"]` | nil | `ErrNoKeywords` |
| `["Notes, 2026"]` (comma inside one element) | `["notes, 2026"]` | nil, commas are the CLI's job |

### 1.6 Snapshot format and codec
- [ ] Create `internal/store/codec.go` with this on-disk layout:

```
offset  size  field
0       4     magic "T8DB"
4       1     format version (1)
5       ...   gzip stream wrapping a gob stream of snapshot
```

```go
const formatVersion byte = 1

var magic = [4]byte{'T', '8', 'D', 'B'}

type snapshot struct {
    NextID  uint64
    Records []Record // sorted by ID before encoding
}

func writeSnapshot(w io.Writer, snap snapshot) error {
    if _, err := w.Write(magic[:]); err != nil {
        return err
    }
    if _, err := w.Write([]byte{formatVersion}); err != nil {
        return err
    }
    zw := gzip.NewWriter(w)
    if err := gob.NewEncoder(zw).Encode(snap); err != nil {
        return err
    }
    return zw.Close()
}
```

- [ ] `readSnapshot` returns these exact wrapped errors, never a panic:
  - wrong magic: `store: %s: not a trace8 database`;
  - version newer than this build: `store: %s: format version %d is newer than this build`;
  - truncated file: the gzip or gob error wrapped with the path.
- [ ] Sort `Records` by `ID` before encoding. Map iteration order is random, and identical content should produce identical bytes for tests and diffs.

Proof: round-trip test (empty store, one record, 10,000 records), corrupt-magic test with 5 garbage bytes, newer-version test with a handcrafted header, and a determinism test that saves twice and compares bytes.

### 1.7 Atomic save
- [ ] Implement `save` with temp file plus rename, in the same directory, with a leading-dot temp name so a crashed save never looks like a database.

```go
func (s *Store) save() error {
    tmp, err := os.CreateTemp(filepath.Dir(s.path), ".trace8-*.tmp")
    if err != nil {
        return err
    }
    tmpName := tmp.Name()
    defer func() {
        if tmpName != "" {
            _ = os.Remove(tmpName) // no-op after a successful rename
        }
    }()
    snap := snapshot{NextID: s.index.nextID, Records: s.index.sortedRecords()}
    if err := writeSnapshot(tmp, snap); err != nil {
        _ = tmp.Close()
        return err
    }
    if err := tmp.Sync(); err != nil {
        _ = tmp.Close()
        return err
    }
    if err := tmp.Close(); err != nil {
        return err
    }
    if err := os.Rename(tmpName, s.path); err != nil {
        return err
    }
    tmpName = "" // rename consumed the file
    s.dirty = false
    return nil
}
```

- [ ] Known accepted gap, write it in the code comment: the directory entry itself is not fsynced, so a machine crash in the tiny window between rename and disk flush can lose the newest save. That is acceptable for a local tool and is cheaper than directory fsync on every close.

Proof: save, add a record, save again, reopen, both records present; a leftover `.trace8-*.tmp` file in the directory does not affect `Open`; a read-only directory makes `save` fail with the OS error wrapped, not swallowed.

### 1.8 Compression evidence
- [ ] Test that whole-file gzip does its job, and record the observed sizes in this row:
  - 1 MB of repeated text compresses to well under 10 percent of raw;
  - 1 MB of random bytes stays within a few percent of raw (gzip adds a small header);
  - both round-trip byte-identical.
- [ ] Record the numbers, for example `text 1048576 -> 3542 bytes, random 1048576 -> 1048650 bytes`. This is evidence for the "compressed database" part of the original ask. Per-record compression is deliberately not here; it belongs to the append log in Phase 4.

### 1.9 Retrieval and load evidence
- [ ] Create `internal/store/bench_test.go` with 10,000 records, 5 keywords each, mixed 256-byte text and random payloads. Run and paste results into this row:

```
go test ./internal/store -run '^$' -bench 'BenchmarkGet' -benchmem -count=5
go test ./internal/store -run '^$' -bench 'BenchmarkOpen' -count=3
```

- [ ] Record `BenchmarkGet` ns/op (warm, single keyword) and `BenchmarkOpen` ms/op (cold load) plus the on-disk dataset size. The row stays open until the numbers are here; "it compiles" is not evidence.

**Gate 1:** run and paste the results below on one revision.

```
gofmt -l ./cmd ./internal   # expect empty output
go vet ./...                # expect exit 0, no output
go test ./internal/store    # expect ok github.com/chinmay-sawant/trace8/internal/store
make fmt-check              # expect exit 0
```

## Phase 2: CLI surface (`internal/cli`)

Scale estimate: about 250 lines in `cli.go` plus 300 lines of tests.

### 2.1 Dispatch and contract preservation
- [ ] Route `db` in `Run` after the existing flag parse, before the hello path. The existing contract stays untouched: `--version` wins (`internal/cli/cli.go:27-30`), bad flags exit 2 with usage on stderr (`cli.go:23-26`), default name behavior stays (`cli.go:42-46`).

```go
func Run(args []string) int {
    // existing flag parsing unchanged (cli.go:17-26)
    if *showVersion {
        fmt.Println("trace8 " + Version)
        return 0
    }
    if rest := fs.Args(); len(rest) > 0 && rest[0] == "db" {
        return runDB(rest[1:])
    }
    // existing server and hello paths unchanged (cli.go:31-46)
}
```

- [ ] Runtime errors from store commands go to stderr prefixed `trace8 db: ` and exit 1. Usage errors print the command usage to stderr and exit 2. Keyword-not-found and unknown-record exits are 1, so scripts can distinguish "ran fine, nothing there" from "bad invocation".

| case | stream | exit |
|---|---|---|
| bad flag or missing args | usage on stderr | 2 |
| store or file error | `trace8 db: <error>` on stderr | 1 |
| no records for a keyword | `trace8 db: no records for keyword "x"` on stderr | 1 |
| success | results on stdout | 0 |

### 2.2 Per-command flagsets and usage
- [ ] One `FlagSet` per subcommand, each carrying `--db` (default `trace8.db`), so flags may follow the subcommand: `trace8 db get db --db notes.db`. Flags before `db` are not supported; document that.

```go
func runDB(args []string) int {
    if len(args) == 0 {
        dbUsage(os.Stderr)
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
    default:
        dbUsage(os.Stderr)
        return 2
    }
}

func dbFlags(name string) (*flag.FlagSet, *string) {
    fs := flag.NewFlagSet(name, flag.ContinueOnError)
    fs.SetOutput(io.Discard)
    db := fs.String("db", "trace8.db", "database file")
    return fs, db
}
```

- [ ] Usage text, exact wording:

```
Usage: trace8 db <command> [flags]

Commands:
  put <k1,k2,...>   store a payload from --text, --file, or -
  get <keyword>     print matching records
  del <id>          delete a record by id
  stats             print record count, keyword count, and file size

Common flags:
  --db string   database file (default "trace8.db")

get flags:
  --all         require every keyword (default: any keyword)
  --id N        print only record N
  --raw         write the payload bytes to stdout (needs --id)
  --out path    write the payload bytes to a file (needs --id)
  -n N          stop after N matches (default 0 = all)
```

Proof: `trace8 db`, `trace8 db nope`, and `trace8 db get` all print usage to stderr and exit 2; covered by tests.

### 2.3 put
- [ ] Implement `put` with these rules:
  - keywords are the first positional argument, split on `,`, normalized by the store;
  - exactly one payload source: `--text "s"` (kind text), `--file path` (kind blob), or positional `-` (stdin);
  - stdin kind: `text` when the bytes are valid UTF-8 with no NUL byte, else `blob`; `--kind text|blob` forces it;
  - `--text ""` is a valid empty payload; use `fs.Visit` to detect flags, not string emptiness;
  - more than one source or none: usage, exit 2;
  - success prints the new id, nothing else.

```go
s, err := store.Open(*db)
if err != nil {
    fmt.Fprintln(os.Stderr, "trace8 db:", err)
    return 1
}
defer s.Close() // Close saves the snapshot

id, err := s.Put(keywords, kind, payload)
if err != nil {
    fmt.Fprintln(os.Stderr, "trace8 db:", err)
    return 1
}
fmt.Println(id)
```

Proof: CLI tests with `t.TempDir()` for all three sources, empty text, both-kind override, two sources (exit 2), zero sources (exit 2), no keywords (exit 2), and a reopen in the same test to prove persistence.

### 2.4 get
- [ ] Output format, one block per match, ascending id:

```
1 text 41 [db go notes]
runs are slow when the queue is empty
2 blob 12345 [db perf]
```

  Rules:
  - text records print the payload on the line(s) after the summary;
  - blob records print only the summary, never bytes;
  - `--all` switches comma-separated keywords from union to intersection;
  - `--id N` filters to one record; `--raw` and `--out path` require `--id`, write the exact bytes, and `--out` prints `wrote 12345 bytes to copy.pprof`;
  - `-n N` caps printed matches and adds a footer `showing 2 of 7 matches`;
  - no matches: message to stderr, exit 1.
- [ ] Snippet for intersection (small and clear):

```go
func intersect(a, b map[uint64]struct{}) map[uint64]struct{} {
    out := make(map[uint64]struct{})
    smaller, larger := a, b
    if len(b) < len(a) {
        smaller, larger = b, a
    }
    for id := range smaller {
        if _, ok := larger[id]; ok {
            out[id] = struct{}{}
        }
    }
    return out
}
```

Proof: CLI tests for text output, blob summary, `--raw` byte-exact round trip through `--out` compared with `bytes.Equal`, `--all` with a record matching one of two keywords and one matching both, `-n` footer, empty result exit 1.

### 2.5 del and stats
- [ ] `del <id>` parses a uint64, deletes, prints `deleted 7`; unknown id prints `trace8 db: no record 7` and exits 1.
- [ ] `stats` prints exactly:

```
records: 12
keywords: 34
file: trace8.db (45678 bytes)
```

  The byte count comes from `os.Stat` on the snapshot (0 when the file does not exist yet). Proof: CLI tests, including stats on a fresh path and after deletes.

### 2.6 Test helper and case matrix
- [ ] Add a `captureRun(t, args ...string) (code int, stdout, stderr string)` helper next to the existing `captureStdout` (`internal/cli/cli_test.go:10-32`); do not change `captureStdout` itself, existing tests depend on it.
- [ ] Case matrix to cover, each as a row in `cli_test.go`: dispatch usage exits, all three put sources, get union, get intersection, get raw and out, del, stats, `--db` with a temp path, `--version` still winning when combined with `db`, and a bad `--db` path exiting 1.

### 2.7 Deferred: compact
- [~] `db compact` is deferred. Reason: shape A rewrites the whole snapshot on every save, so there is no dead data to reclaim. Owner: Phase 4 engine work. Next gate: row 4.5; when it lands, this row becomes active, not closed.

**Gate 2:** paste the results below.

```
make check   # vet, tests, build all green
go build -o /tmp/trace8 ./cmd/trace8
/tmp/trace8 db put go,notes --text "hello store"
/tmp/trace8 db get go
/tmp/trace8 db put blob --file /bin/ls
/tmp/trace8 db get blob
/tmp/trace8 db stats
```

Rebuild the binary first, per `AGENTS.md` "Verifying against stale artifacts". Paste the observed output here.

## Phase 3: Resident server mode (deferred)

Reason: the default is the one-shot CLI. Owner: server/CLI work. Next gate: D2 recorded here, or real usage shows the per-command load cost hurts (large snapshot or frequent queries).

- [~] 3.1 Hold one `*store.Store` in the server lifecycle. `internal/server/server.go:51-69` owns start and stop, so `New` opens the store and `Run` closes it during shutdown. Owner: server work. Next gate: D2 recorded.
- [~] 3.2 Wire two routes in `internal/server/routes.go:9-15`, handlers in the style of `internal/server/handlers.go:8-11`:

```
GET  /api/db/get?tag=db&n=20
     -> 200 {"records":[{"id":1,"kind":"text","keywords":["db","go"],"size":41,"payload":"..."}]}

POST /api/db/put
     {"keywords":["go"],"kind":"text","payload_base64":"aGVsbG8="}
     -> 201 {"id":7}
```

  Bad JSON or no keywords: 400 with `{"error":"..."}`. Store errors: 500. Owner: server work. Next gate: row 3.1 lands.
- [~] 3.3 Add a CLI client mode that talks to a running server instead of opening the file, so repeated `get` calls skip the load. Owner: CLI work. Next gate: row 3.2 lands.

Proof for 3.1 and 3.2: `httptest` tests like `internal/server/server_test.go:13-20`, including one test that shutdown closes the store cleanly.

## Phase 4: Append-log engine (deferred)

Reason: shape A is enough until writes get heavy or a save pause becomes visible at the real data size. Owner: store engine work. Next gate: measured put-cycle cost from `db stats` plus a timed save.

- [~] 4.1 Append log with one header per record. Layout, all integers big-endian:

| offset | size | field |
|---|---|---|
| 0 | 4 | magic `T8LG` |
| 4 | 1 | format version `1` |
| 5 | 1 | kind `t` or `b` |
| 6 | 1 | codec `0` raw, `1` gzip |
| 7 | 1 | flags, bit 0 = tombstone |
| 8 | 8 | id |
| 16 | 4 | keywords length K |
| 20 | 8 | stored payload length P |
| 28 | K | keywords blob, normalized keywords joined with `\n` |
| 28+K | P | stored payload bytes (raw or gzip per codec) |

  Owner: store engine work. Next gate: phase trigger.
- [~] 4.2 `Open` scans the log once, rebuilds the index, and discards a truncated tail with a warning instead of failing.

```go
for {
    hdr, err := readHeader(br)
    if errors.Is(err, io.EOF) {
        break // clean end
    }
    if errors.Is(err, io.ErrUnexpectedEOF) {
        log.Printf("store: %s: discarding truncated tail", path)
        break
    }
    if err != nil {
        return nil, err
    }
    // apply hdr and record body to the index; tombstone means delete
}
```

  Owner: store engine work. Next gate: phase trigger.
- [~] 4.3 Deletes append tombstones; reads ignore tombstoned ids. Owner: store engine work. Next gate: phase trigger.
- [~] 4.4 Per-record compression moves here: try gzip, keep whichever is smaller, set the codec byte. The snapshot version of this idea is deliberately absent from Phase 1 because whole-file gzip already covers shape A. Owner: store engine work. Next gate: phase trigger.
- [~] 4.5 `Compact` writes live records to `path + ".compact"`, syncs, renames over the log, and drops tombstones; ids never change. Owner: store engine work. Next gate: row 2.7 becomes active here.

Constraint: the `Put/Get/Delete/Close` signatures from Phase 1 do not change, and `internal/cli` plus its tests must keep passing untouched. Proof for each row: `internal/store` tests, including a handcrafted truncated log and a tombstone-heavy log that compacts to a smaller file.

## Phase 5: Full-text body search (deferred, gated by D1)

Reason: tags only is the default; body search changes result quality and needs a limit. Owner: store work. Next gate: D1 recorded here as "tags plus body text".

- [~] 5.1 Tokenizer, covered by a table test:

```go
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
```

  Owner: store work. Next gate: D1 recorded.
- [~] 5.2 Feed body tokens into the same inverted index under a `body:` prefix, so tag matches and body matches stay separable and `get body:queue` works for debugging. Only the put path changes; `index.go` stays as is. Owner: store work. Next gate: D1 recorded.
- [~] 5.3 `get -n N` result limit and newest-first order, because a common word can match many records. Owner: CLI work. Next gate: rows 5.1 and 5.2 land.

## Phase 6: Closure and docs

- [ ] 6.1 Run `make check` on the final tree and record the result summary here.
- [ ] 6.2 Run `make lint` (`Makefile:31-32`) if golangci-lint is installed, and record the outcome. A missing linter is recorded as skipped, not passed.
- [ ] 6.3 Fix the stale claim in `AGENTS.md` "Things to AVOID" item 10: it says `make lint` does not exist, while `Makefile:31-32` defines it.
- [ ] 6.4 Update `AGENTS.md` after the feature lands: the CLI contract gains the `db` subcommand, the code structure gains `internal/store/`, and the "There is no `plans/` directory yet" sentence is no longer true.
- [ ] 6.5 Confirm `go.mod:1-5` still requires nothing. Any dependency proposal stops here and needs explicit user sign-off.
- [ ] 6.6 Keep `internal/cli.Version` at `0.0.1` (`internal/cli/cli.go:15`). No version machinery exists and this plan does not add any.

## Definition of done

The effort is done when:

- Phases 1 and 2 rows are closed with pasted evidence, and `make check` is green on the final revision;
- benchmark numbers for `Get` and `Open` are recorded in row 1.9;
- the smoke session output is recorded under Gate 2;
- `go.mod` still has zero requires;
- `AGENTS.md` is updated per rows 6.3 and 6.4;
- D1 through D4 are marked decided or explicitly carried as `[~]` with a reason.

## Design reference

### Data model

A record has an id, a list of keywords, a kind, and a payload.

```
put "go,db,blob" ./a.bin   -> id 1
put "db,perf"    ./b.bin   -> id 2

byKeyword:  "go" -> [1]    "db" -> [1,2]    "blob" -> [1]    "perf" -> [2]
records:    1 -> a.bin bytes, 2 -> b.bin bytes
```

`get db` returns both records. `get go,perf` means union by default and intersection with `--all`. D3 picks nothing here; the flag covers both.

This is an inverted index. The same maps serve full-text search later; the only difference is where keywords come from (you type tags, or a tokenizer splits the body text). That is Phase 5.

### Storage shapes

| Shape | Writes | Reads | Stops working when | Plan phase |
|---|---|---|---|---|
| A. Snapshot (gob + gzip, atomic rename) | rewrite whole file | map lookup in RAM | writes get frequent or the file gets large | Phase 1 |
| B. Append-only log + in-memory index | append one record | map lookup, payload by `ReadAt` | data no longer fits in RAM | Phase 4, deferred |
| C. Embedded engine (bbolt and similar) | engine handles it | page reads from disk | needs a dependency and sign-off | not planned |

Shape A is small and nearly crash-proof because of the temp-file-plus-rename write. Shape B is what shape A grows into if writes get heavy. Shape C is for data bigger than RAM.

### Compression

- Go can read bzip2 but not write it: `compress/bzip2` exports only `NewReader` (checked with `go doc compress/bzip2` on go1.26.4, the toolchain in `go.mod:5`).
- The standard library writers are `compress/gzip`, `zlib`, `flate`, and `lzw` (checked with `go list std`). zstd is not public; only `internal/zstd` exists inside the runtime.
- Shape A compresses the whole snapshot as one gzip stream. Phase 4 adds a per-record flag byte when the append log lands, because there each record must stay seekable on its own. Text shrinks a lot; already-compressed blobs (JPEG, ZIP) stay effectively raw.

The thing that makes reads fast is the in-memory index. Compression only shrinks the file and shortens load time at startup.

### Memory, startup, and the two usage modes

- `Open` reads the whole snapshot, decompresses it, and builds the maps. Reads after that are map lookups. Memory use is roughly the dataset size plus map overhead; a save briefly holds old plus new bytes.
- One-shot CLI: every command pays the load once and exits. This is the default mode.
- Resident server: load once, serve many. This is the fastest mode, and the lifecycle code already exists in `internal/server/server.go:51-69`.
- Where the load cost starts to hurt depends on D4 (payload sizes) and D2 (usage mode).

Honest comparison with SQL: a local SQL database also reads a row in microseconds. This design wins on simplicity, on tag lookups (one keyword to many records) as a first-class operation, and on owning the file format. It loses on transactions, joins, sorted range scans, and a query language. Reach for SQL or an embedded engine when you need those, not before.

### Where the code goes

- New package `internal/store/`: `doc.go`, `store.go`, `index.go`, `codec.go`, tests next to the code. This mirrors the seams in `internal/server/`: `server.go` owns lifecycle, `routes.go` owns wiring, `handlers.go` owns handlers (`AGENTS.md`, Code structure).
- CLI wiring stays in `internal/cli/cli.go` and `internal/cli/cli_test.go`, reusing the `captureStdout` test pattern (`cli_test.go:10-32`).
- Server wiring, if Phase 3 runs, lands in `internal/server/routes.go:9-15` and `internal/server/handlers.go:8-11`, tested like `internal/server/server_test.go:13-20`.

### CLI sketch

```
trace8 db put go,db,notes --text "runs are slow when the queue is empty"
trace8 db put perf,db --file ./profile.pprof
trace8 db get db
trace8 db get go,perf --all
trace8 db get blob --id 2 --out ./copy.pprof
trace8 db del 7
trace8 db stats
```

Keyword rules: comma-separated, trimmed, lowercased, empties dropped, duplicates removed, at least one required.

### Non-goals

- Transactions, joins, sorted range scans, SQL syntax.
- Multiple writing processes. One lock file, one writer.
- Remote access, replication, HTTP auth.
- Ranking for body search; if Phase 5 runs, order is deterministic and capped, nothing fancier.
- New dependencies. `go.mod:1-5` has zero requires and any addition needs sign-off.

### Open questions

D1 through D4 at the top of this file.

## Dependencies

- Zero Go dependencies, standard library only. Any deviation needs explicit sign-off per `AGENTS.md`.
- D1 gates Phase 5. D2 gates Phase 3. D3 and D4 pick defaults and can be answered later.
- Row 6.2 needs `golangci-lint` installed in the shell.
- Phase 3 depends on Phases 1 and 2. Phase 4 replaces Phase 1 internals only. Phase 5 depends on D1.

## Status legend

- `[ ]` not started, or not proven with current evidence
- `[x]` implemented and validated with current evidence
- `[~]` intentionally deferred or partial, with reason, owner boundary, and next gate
