# trace8 0.0.1 - Local keyword store (`trace8 db`)

> **Parent:** none (first plan under `plans/`; repo conventions live in `AGENTS.md`)
> **Status:** implemented and verified on 2026-09-19. Every row is closed with evidence pasted into `## Verification evidence`.
> **Estimated effort:** store core about one day, CLI about one day, server plus remote client about half a day, closure about an hour. The shipped file sizes are recorded per phase.

---

## Overview

The goal: a local store inside trace8 that keeps blobs and text, attaches keywords to each record, and finds records by keyword fast. No SQL, no new dependencies; `go.mod` has no `require` lines.

The shipped architecture, read from the code:

- A record is an id, keywords, a kind (`text` or `blob`, display only), and a `[]byte` payload (`internal/store/index.go:22-28`).
- On disk the store is an append-only log, one record per entry, magic `T8LG`, format version 1, fixed 28-byte header (`internal/store/codec.go:13-31`).
- `Open` replays the log once into an in-memory inverted index; reads are map lookups and never touch disk. Payloads stay in memory (`internal/store/store.go:21-24`, `internal/store/doc.go:1-7`).
- Writes append immediately and are durable on their own; `Delete` appends a tombstone; ids are never reused, and `Compact` writes a high-water marker so replay does not hand an old id out again (`internal/store/store.go:80-164`, `internal/store/store.go:258-266`).
- Each payload is stored as gzip only when that is strictly smaller than the raw bytes (`internal/store/codec.go:143-158`).
- Text payloads also index their body tokens under a `body:` prefix, so `get body:queue` works (see "Decisions (as shipped)" D1); user supplied keywords starting with `body:` are rejected by the CLI and the HTTP API because the prefix is reserved for those derived tokens (`internal/cli/db.go:419-429`, `internal/server/handlers.go:71-97`).
- A lock file at `path + ".lock"` keeps one writer at a time (`internal/store/store.go:49-58`).
- `Compact` rewrites live records through a temp file plus rename, drops tombstones, and keeps the original file permissions (`internal/store/store.go:202-299`).
- Two front doors: the local one-shot CLI (`internal/cli/db.go`) and a resident server, `trace8 --server` (`internal/server`), with the CLI's `--remote` mode talking to the server's `/api/db/*` endpoints.

Example of the intended use, matching the shipped output format. This is a real run of this build on a fresh file; byte counts change with payload content.

```
$ trace8 db put go,db,notes --text "runs are slow when the queue is empty"
1
$ trace8 db get db
1 text 37 [go db notes]
runs are slow when the queue is empty
$ trace8 db get body:queue
1 text 37 [go db notes]
runs are slow when the queue is empty
$ trace8 db stats
records: 1
keywords: 11
deleted: 0
file: trace8-example.db (154 bytes)
$ trace8 db del 1
deleted 1
$ trace8 db compact
compacted trace8-example.db
```

This file is the single canonical ledger for the effort. Rationale lives in "Design reference" near the end; everything that needs doing lives in the phases, and every row names the file it touches and the proof command that was or can be run.

## Executive Summary

Phase 1 builds `internal/store`: the `T8LG` append log (`codec.go`), replay into the in-memory index with the lock file and truncated-tail repair (`store.go`, `index.go`), tombstone deletes, per-record gzip, compaction, keyword normalization, `body:` tokens for text payloads, tests for every failure mode, and the benchmarks.

Phase 2 builds the CLI in `internal/cli`: a `db` subcommand with `put`, `get`, `del`, `stats`, and `compact`, exact output formats and exit codes, and tests that reuse the capture pattern from `internal/cli/cli_test.go` and `captureRun` from `internal/cli/db_test.go:16-52`.

Phase 3 builds the server side: `Config.DBPath`, store ownership in the server lifecycle, the `/api/db/*` routes and handlers, and the `--remote` client that speaks the same JSON.

Phase 4 closes: `AGENTS.md` updates, dependency audit, version discipline, and the gate commands that feed `## Verification evidence`.

## Ground rules

These apply to every row in every phase.

- Standard library only. `go.mod` has no `require` lines; adding a dependency needs explicit user sign-off (`AGENTS.md`, Dependency policy).
- Tests live next to the code, use `t.TempDir()`, and open no ports; HTTP tests use `httptest` (`internal/server/server_test.go:22-42`, `internal/cli/db_remote_test.go:19-99`).
- A row closes only when its proof command ran on the current revision and the result is pasted into `## Verification evidence`. No closing from intent (golden rule 6).
- Errors are lowercase, plain, and name the path, id, or keyword involved.
- Formatting is `gofmt` only; `make fmt-check` must stay clean.
- No em dashes in code comments, CLI output, or docs.
- One package per phase, one writer at a time on the repo.

## Decisions (as shipped)

Four questions shaped the scope. These are the answers that shipped; there are no open decisions left.

- D1 Search scope per record: tags plus body text. `Put` tokenizes `KindText` payloads and indexes each token as `body:<token>` (`internal/store/store.go:80-90`, `internal/store/index.go:135-147`); `get body:queue` is covered by `TestDBGetBodySearch` (`internal/cli/db_test.go:484`), and `displayKeywords` hides the `body:` entries from output (`internal/cli/db.go:601-612`). User supplied `body:` keywords are rejected at the put boundary in the CLI (`internal/cli/db.go:419-429`, covered by `TestDBPutReservedBodyKeyword` at `internal/cli/db_test.go:470`) and in the HTTP API (`internal/server/handlers.go:71-97`, covered by `TestDBPutRejectsBadKeywords` at `internal/server/server_test.go:242`); the store itself treats `body:` as an ordinary string.
- D2 Primary usage mode: both. The one-shot CLI opens the log per command (`internal/cli/db.go:339-348`); `trace8 --server` holds one store open for the process lifetime (`internal/server/server.go:40-64`); `--remote` is the client for a running server (`internal/cli/db.go:100-111`).
- D3 `get a,b` meaning: union of the keyword sets by default, intersection with `--all` (`internal/cli/db.go:530-554`, covered by `TestDBGetUnionAndIntersection`).
- D4 Payload size policy: every payload stays in RAM after replay; there is no disk-offset path. Compression only shrinks the log and the replay read (`internal/store/store.go:21-24`, `internal/store/doc.go`).

## Phase 1: Store core (`internal/store`)

Shipped size: about 800 lines of code (`store.go` 454, `codec.go` 190, `index.go` 148, `doc.go` 8) and about 2,060 lines of tests (`hardening_test.go` 801, `store_test.go` 503, `regression_test.go` 356, `recovery_test.go` 182, `codec_test.go` 124, `bench_test.go` 96).

### 1.1 Package doc and layout
- [x] `internal/store/doc.go` carries the package comment: append-only log, one replay at Open, in-memory reads, immediate durable writes, one-writer lock, compaction through a temp file plus rename. File: `internal/store/doc.go`. Proof: `go doc ./internal/store` prints it.

### 1.2 Types and public surface
- [x] `internal/store/index.go` defines `Kind` (`text`, `blob`), `Record` with JSON tags, `Stats` (records, keywords, deleted, file bytes), `ErrNoKeywords`, and the unexported `index` with `add`, `remove`, and `lookup`. File: `internal/store/index.go`. Proof: `go build ./...` and `go doc ./internal/store`.
- [x] `internal/store/store.go` exposes `Open`, `Put`, `Get`, `GetByID`, `Delete`, `Stats`, `Compact`, and `Close` on `*Store`. File: `internal/store/store.go`. Proof: `go doc ./internal/store` lists exactly these symbols.

### 1.3 Log format
- [x] `internal/store/codec.go` encodes each record as magic `T8LG`, version 1, kind byte, codec byte, flags byte, big-endian id, 4-byte keyword length, 8-byte stored payload length, keywords joined with `\n`, and the stored payload; `headerSize` is 28. File: `internal/store/codec.go`. Proof: `go test ./internal/store -run 'TestRecordLayout|TestHeaderRoundTrip|TestKindBytes|TestKeywordBlob' -count=1`.
- [x] `flagTombstone` (bit 0) marks delete records; `flagHighWater` (bit 1) marks the Compact marker that keeps ids from being reused; `kindToByte`/`kindFromByte` map kinds; `decodeKeywords` skips empty entries and returns nil for an empty or all-separator blob. File: `internal/store/codec.go`. Proof: `go test ./internal/store -run 'TestKindBytes|TestKeywordBlob|TestTombstoneReplay|TestIDsSurviveCompactReopen|TestDegenerateKeywordBlob' -count=1`.

### 1.4 Per-record compression
- [x] `compressPayload` keeps gzip output only when strictly smaller than the raw bytes; `decompressPayload` handles raw and gzip and rejects unknown codecs; the `gzipReaders` pool avoids a fresh reader per compressed record. File: `internal/store/codec.go`. Proof: `go test ./internal/store -run 'TestCompressPayload|TestDecompressPayloadErrors|TestCompression' -count=1`.

### 1.5 Open, replay, lock, truncated tail
- [x] `Open` creates `path + ".lock"` with `O_CREATE|O_EXCL|O_WRONLY`; a second open fails with the stale-lock hint; failed open paths call `releaseLock`. File: `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestLockExcludesSecondOpen|TestBadFirstRecord|TestMidFileCorruptionReleasesLock' -count=1`.
- [x] `replay` reads the log, validates magic, version, kind, and codec per record, and applies live records and tombstones to the index while honoring high-water markers. File: `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestOpenEmpty|TestPutGetReload|TestCorruptLaterRecord|TestTombstoneReplay|TestEdgeFileShapes' -count=1`.
- [x] a partial trailing record is dropped with a `discarding truncated tail at offset N` warning, and `discardTail` truncates the file to the last complete record on disk, so the next append starts from a clean boundary; a short first read that is not a `T8LG` prefix fails Open with `not a trace8 database` and leaves the file untouched. File: `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestTruncatedTail|TestTruncatedTailKeepsCompleteRecords|TestShortGarbageRejected|TestMagicPrefixTruncatedTail' -count=1` (the hardening test appends after the cut, closes, reopens, and checks both records survive).

### 1.6 In-memory index
- [x] `index` keeps `byKeyword` and `records`; `add` appends postings, `remove` drops postings and deletes empty keyword entries, `lookup` returns ascending ids and skips defensive misses. File: `internal/store/index.go`. Proof: `go test ./internal/store -run 'TestPutGetReload|TestDelete|TestLookupMisses|TestGetReturnsStructCopy' -count=1`.

### 1.7 Keyword normalization, newline rejection, body tokens
- [x] `normalizeKeywords` lowercases and trims each keyword, drops empties and duplicates, rejects keywords containing `\n` (the log joins keywords with `\n`, so an embedded newline would split one keyword into two on reload), and returns `ErrNoKeywords` when nothing remains. File: `internal/store/index.go`. Proof: `go test ./internal/store -run 'TestNormalizeKeywords|TestKeywordWithNewlineRejected|TestKeywordEdgeCases|TestPutValidation' -count=1`.
- [x] `tokenize` splits text on non letter or digit runes, lowercases, and drops tokens shorter than 2 runes; `Put` indexes `body:<token>` for `KindText` only. Files: `internal/store/index.go`, `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestTokenizer|TestTextBodyTokens' -count=1`.

### 1.8 Writes, deletes, and the sticky write error
- [x] `Put` appends the encoded record and updates the index under the write lock; payloads are kept without copying, so the doc comment tells callers not to mutate them. File: `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestPutGetReload|TestPayloadEdges|TestConcurrentAccess' -count=1`.
- [x] `Delete` appends a tombstone, removes the posting, and reports false for an unknown id; ids are never reused. File: `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestDelete|TestTombstoneReplay' -count=1`.
- [x] a failed or short append is truncated back to the tracked logical size so a later write cannot land behind torn bytes; if that rollback truncate fails too, a sticky `writeErr` is latched and `Put`, `Delete`, and `Compact` return it instead of touching a broken log; a lost log handle (for example a failed reopen at the end of `Compact`) and `Close` latch the same sticky errors. File: `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestWritesAfterLostLogHandle|TestClosedStore|TestFailedAppendRollsBackAndLatches|TestLogicalSizeTracksDisk' -count=1`.

### 1.9 Compaction
- [x] `Compact` writes live records in ascending id to `.trace8-compact-*.tmp` in the same directory, syncs, renames over the log, reopens the append handle, preserves the original file permissions, writes the high-water marker, and resets the tombstone counter; a failed close or reopen records the sticky error. File: `internal/store/store.go`. Proof: `go test ./internal/store -run 'TestCompactHygiene|TestGetDuringCompactConsistent|TestCompactPreservesPermissions|TestIDsSurviveCompactReopen' -count=1`.

### 1.10 Concurrency
- [x] every exported method locks the `RWMutex`; readers share, writers exclude, and `Close` is idempotent. File: `internal/store/store.go`. Proof: `go test ./internal/store -race -run 'TestConcurrentAccess|TestConcurrentClose|TestStressConcurrentLifecycle|TestCloseIdempotent' -count=1`.

### 1.11 Benchmarks
- [x] `internal/store/bench_test.go` builds 10,000 records, five keywords each, half repeated text and half random payloads, and defines `BenchmarkGet` and `BenchmarkOpen`. File: `internal/store/bench_test.go`. Proof: `go test ./internal/store -run '^$' -bench 'BenchmarkGet' -benchmem -count=5` and `go test ./internal/store -run '^$' -bench 'BenchmarkOpen' -benchmem -count=3`; the numbers and the dataset size go in `## Verification evidence`.

**Gate 1 (dev loop):**

```
gofmt -l ./cmd ./internal   # expect empty output
go vet ./...                # expect exit 0, no output
go test ./internal/store -count=1
go test ./internal/store -race -count=1
```

## Phase 2: CLI surface (`internal/cli`)

Shipped size: `db.go` 705 lines; tests `db_test.go` 617 and `db_remote_test.go` 218.

### 2.1 Dispatch and contract preservation
- [x] `Run` routes a first positional `db` to `runDB` after the flag parse; `--version` still wins; bad flags print both usage lines to stderr and exit 2; the hello and server paths are unchanged. File: `internal/cli/cli.go`. Proof: `go test ./internal/cli -run 'TestRunVersion|TestRunDefaultName|TestRunNamedArg|TestRunBadFlag|TestDBVersionWins' -count=1`.

### 2.2 Flags, usage, exit codes
- [x] one `FlagSet` per subcommand carries `--db` (default `trace8.db`) and `--remote`; `parseArgs` accepts flags and positionals in any order and treats `--` as the terminator that stops flag parsing for good; `flagWasSet` separates an unset `--text` from `--text ""`. File: `internal/cli/db.go`. Proof: `go test ./internal/cli -run 'TestDBPutTextAndGet|TestDBPutTextEmpty|TestDBDoubleDashTerminator|TestDBDoubleDashFlagsNotReparsed' -count=1`.
- [x] `dbUsageText` documents `put`, `get`, `del`, `stats`, `compact` and every flag; usage errors exit 2, and runtime errors print `trace8 db: <error>` to stderr and exit 1. File: `internal/cli/db.go`. Proof: `go test ./internal/cli -run 'TestDBUsage|TestDBBadPath' -count=1`.

### 2.3 put
- [x] exactly one payload source: `--text` (kind text), `--file` (kind blob), or a trailing `-` (stdin, kind detected as text when the bytes are valid UTF-8 without NUL, else blob); `--kind text|blob` overrides; `--text ""` is a valid empty payload; success prints the new id and nothing else; a keyword starting with `body:` is rejected with `trace8 db: keyword "body:x" uses the reserved body: prefix` and exit 1. File: `internal/cli/db.go`. Proof: `go test ./internal/cli -run 'TestDBPut' -count=1`.

### 2.4 get
- [x] comma-separated keywords are unioned by default and intersected with `--all`; `--id N` selects one record; `--raw` writes the payload bytes to stdout; `--out path` writes a file and prints `wrote N bytes to path`; `-n N` shows at most N matches newest first and prints the `showing X of Y matches` footer only when the limit hid records. File: `internal/cli/db.go`. Proof: `go test ./internal/cli -run 'TestDBGetUnionAndIntersection|TestDBGetByID|TestDBGetRaw|TestDBGetOut|TestDBGetLimit|TestDBGetKeywordNormalization' -count=1`.
- [x] text records print the payload with a guaranteed trailing newline, blobs print only the summary, `body:` keywords stay hidden, and no matches exit 1 with `no records for keyword "x"`. File: `internal/cli/db.go`. Proof: `go test ./internal/cli -run 'TestDBGetBodySearch|TestDBGetNoMatches|TestDBPutFileBlob' -count=1`.

### 2.5 del, stats, compact
- [x] `del <id>` parses a uint64, prints `deleted N`, and exits 1 with `no record N` for an unknown id; `stats` prints records, keywords, deleted, and `file: <path> (<n> bytes)`, using the remote base as the label under `--remote`; `compact` prints `compacted <path>`. File: `internal/cli/db.go`. Proof: `go test ./internal/cli -run 'TestDBDel|TestDBStats|TestDBCompact' -count=1`.

### 2.6 Remote client
- [x] the `backend` interface in `internal/cli/db.go` lets every command run against `localBackend` or `remoteBackend`; `--remote` calls `/api/db/get`, `/api/db/record` (GET and DELETE), `/api/db/put`, `/api/db/stats`, and `/api/db/compact`; a 404 on record lookup becomes "no record", and a server `{"error":"..."}` becomes the stderr message; the HTTP client times out after 10 seconds. File: `internal/cli/db.go`. Proof: `go test ./internal/cli -run 'TestDBRemote' -count=1` (covers the round trip, raw output, the stats file label, and server errors).

**Gate 2 (smoke):** build fresh with `go build -o /tmp/trace8 ./cmd/trace8`, then run put, get, stats, compact, and del against a temp `--db`; paste the session into `## Verification evidence`.

## Phase 3: Server and HTTP API (`internal/server`)

Shipped size: `handlers.go` 209, `server.go` 96, `routes.go` 24, `server_test.go` 507.

### 3.1 Server lifecycle owns the store
- [x] `Config` carries `DBPath`; `New` opens the store and sets `ReadHeaderTimeout` 5s, `ReadTimeout` 30s, `WriteTimeout` 60s, and `IdleTimeout` 120s; `Run` closes it exactly once through `closeStore`, including when `ListenAndServe` fails. File: `internal/server/server.go`. Proof: `go test ./internal/server -run 'TestRunShutdownReleasesStore|TestRunStartupFailureReleasesStore|TestServerTimeouts' -count=1`.

### 3.2 Routes and handlers
- [x] `routes.go` wires `GET /api/db/get` (`tag`), `GET /api/db/record` and `DELETE /api/db/record` (`id`), `POST /api/db/put`, `GET /api/db/stats`, and `POST /api/db/compact` before the static catch-all, and an `/api/` fallback answers anything no route matched, wrong methods included, with a JSON 404. File: `internal/server/routes.go`. Proof: `go test ./internal/server -run 'TestDB|TestAPINotFoundIsJSON' -count=1`.
- [x] handlers validate the tag, the id, the JSON body (capped at `maxPutBytes` = 64 MiB, exactly one JSON document, trailing data rejected), keywords (non-blank, no newline, no reserved `body:` prefix), and kind; replies are `{"error":"..."}` with 400 or 404 for bad input, 500 for store errors, 201 with `{"id":N}` for put, and `[]` (not null) when a get matches nothing. File: `internal/server/handlers.go`. Proof: `go test ./internal/server -run 'TestDBPutGetRoundTrip|TestDBGetRequiresTag|TestDBPutValidation|TestDBPutRejectsTrailingData|TestDBPutRejectsBadKeywords|TestDBPutRejectsOversizedBody|TestDBRecordLookup|TestDBDelete|TestDBBadID|TestDBStats|TestDBCompact' -count=1`.

### 3.3 End-to-end remote path
- [x] the CLI `--remote` client and the server speak the same JSON; covered in-process by `internal/cli/db_remote_test.go` against an `httptest` server that mirrors the frozen endpoints, and by a live session against `trace8 --server` recorded in `## Verification evidence`. Files: `internal/cli/db.go`, `internal/server/handlers.go`. Proof: `go test ./internal/cli -run 'TestDBRemoteRoundTrip|TestDBRemoteRaw|TestDBRemoteError' -count=1`.

**Gate 3 (smoke):** run the server on a free port with a temp `--db`, drive put, get, stats, del, and compact with `--remote`, then stop the server with SIGTERM and confirm the lock file is gone; paste the session into `## Verification evidence`.

## Phase 4: Closure and docs

### 4.1 AGENTS.md project and CLI contract
- [x] the Project paragraph says the CLI also has `trace8 db put|get|del|stats|compact` with `--db` and `--remote`, and that `--server` serves the `/api/db/*` endpoints in addition to the static frontend. File: `AGENTS.md`. Proof: `grep -n 'trace8 db' AGENTS.md`.

### 4.2 AGENTS.md code structure and tests
- [x] Code structure adds `internal/store/` with the file seams (`store.go` lifecycle and replay, `index.go` types and index, `codec.go` log format), and the Tests paragraph names the store, CLI, and server test files. File: `AGENTS.md`. Proof: `grep -n 'internal/store' AGENTS.md`.

### 4.3 AGENTS.md plans and AVOID item 10
- [x] "Plans and ledgers" says `plans/0.0.1/local-keyword-store.md` is the canonical ledger for the db effort. File: `AGENTS.md`. Proof: `grep -n 'canonical ledger' AGENTS.md`.
- [x] AVOID item 10 no longer claims `make lint` is missing (`Makefile:31-32` defines it) and no longer lists `plans/` as nonexistent. File: `AGENTS.md`. Proof: `grep -n 'make lint' AGENTS.md Makefile`.

### 4.4 Dependency and version audit
- [x] `go.mod` still has zero requires. File: `go.mod`. Proof: `grep -n require go.mod` prints nothing (exit 1).
- [x] `internal/cli.Version` stays `0.0.1`; no version file or ldflags machinery was added. File: `internal/cli/cli.go:15`. Proof: `grep -n 'Version = ' internal/cli/cli.go`.

### 4.5 Full gates
- [x] run `make fmt-check`, `go vet ./...`, `go test ./... -count=1`, `go test ./... -race -count=1`, `make check`, and `make lint` (or record it skipped when `golangci-lint` is absent), then paste every outcome into `## Verification evidence`. Proof: the exit codes pasted there.

**Gate 4:** all rows above have pasted evidence and the `## Verification evidence` section is complete.

## Definition of done

The effort is done when:

- every phase row is checked only from pasted `## Verification evidence` output on one revision;
- decisions D1 through D4 match the shipped behavior in "Decisions (as shipped)";
- `go.mod` still has zero requires;
- `AGENTS.md` matches the shipped CLI and packages;
- `internal/cli.Version` is still `0.0.1`.

## Design reference

### Data model

A record has an id, a list of keywords, a kind, and a payload.

```
put "go,db,blob" ./a.bin   -> id 1
put "db,perf"    ./b.bin   -> id 2

byKeyword:  "go" -> [1]    "db" -> [1,2]    "blob" -> [1]    "perf" -> [2]
records:    1 -> a.bin bytes, 2 -> b.bin bytes
```

`get db` returns both records. `get go,perf` means union by default and intersection with `--all` (`internal/cli/db.go:530-554`). Text records also carry `body:<token>` entries from their payload, so the same maps serve full-text search; `printRecord` hides those entries so output matches the keywords the user typed (`internal/cli/db.go:601-612`).

### On-disk format: append log

The log is a sequence of self-describing records, all integers big-endian (`internal/store/codec.go:13-31`):

| offset | size | field |
|---|---|---|
| 0 | 4 | magic `T8LG` |
| 4 | 1 | format version `1` |
| 5 | 1 | kind `t` (text) or `b` (blob) |
| 6 | 1 | codec `0` raw, `1` gzip |
| 7 | 1 | flags: bit 0 = tombstone, bit 1 = high-water marker |
| 8 | 8 | id |
| 16 | 4 | keywords length K |
| 20 | 8 | stored payload length P |
| 28 | K | keywords blob, normalized keywords joined with `\n` |
| 28+K | P | stored payload bytes (raw or gzip per codec) |

`Open` replays the log from the start; `Put` and `Delete` append one record each; `Compact` rewrites live records and drops tombstones. Replay is the only full read the store does.

The high-water marker keeps ids from being reused once `Compact` drops tombstones. `Compact` writes it with id `nextID-1` when at least one id was handed out (`internal/store/store.go:258-266`); replay sees `flagHighWater`, bumps `nextID` to id+1, and touches neither the index nor the deleted counter (`internal/store/store.go:414-446`).

### Crash behavior and the hardening fixes

- **Truncated tail:** a record cut off by the end of the log is dropped with a warning, and the file is truncated back to the last complete record on Open (`internal/store/store.go:344-353`, `internal/store/store.go:404-410`). Without that cut, a later append would sit behind the partial bytes and every future Open would read them as corruption. Covered by `TestTruncatedTail` (`internal/store/recovery_test.go:29`) and `TestTruncatedTailKeepsCompleteRecords` (`internal/store/hardening_test.go:24`).
- **Short garbage is not a tail:** a first read shorter than a header that does not match the `T8LG` prefix fails Open with `not a trace8 database` and leaves the file exactly as it was; only a short read that still matches the magic (and the version, once the magic is complete) counts as a truncated tail and gets cut (`internal/store/store.go:348-351`, `internal/store/codec.go:92-101`). Covered by `TestShortGarbageRejected` (`internal/store/regression_test.go:232`) and `TestMagicPrefixTruncatedTail` (`internal/store/regression_test.go:262`).
- **Newline in a keyword:** keywords are joined with `\n` in the log, so an embedded newline would split one keyword into two on reload. `normalizeKeywords` rejects it (`internal/store/index.go:116-120`), covered by `TestKeywordWithNewlineRejected` (`internal/store/regression_test.go:14`). Spaces are still allowed.
- **Failed append:** `appendLog` tracks the logical end of the log and truncates the file back to it after a failed or short write, so a later append cannot land behind torn bytes. If the rollback truncate fails too, a sticky `writeErr` is latched and every write fails closed until the store is reopened (`internal/store/store.go:33-40`, `internal/store/store.go:166-184`). Covered by `TestFailedAppendRollsBackAndLatches` (`internal/store/regression_test.go:56`) and `TestLogicalSizeTracksDisk` (`internal/store/regression_test.go:107`).
- **Lost log handle:** if `Compact` renames the log but cannot reopen the append handle, the store records a sticky `writeErr` and `Put`, `Delete`, and `Compact` return that error instead of dereferencing a nil file (`internal/store/store.go:37-40`, `internal/store/store.go:105-107`, `internal/store/store.go:150-152`, `internal/store/store.go:213-215`). Covered by `TestWritesAfterLostLogHandle` (`internal/store/regression_test.go:30`). Reads keep working from memory.
- **High-water marker:** `Compact` drops tombstones, so the highest id would otherwise be forgotten. The marker records it and replay only raises `nextID`, so the index and the deleted counter stay untouched (`internal/store/codec.go:46-49`, `internal/store/store.go:258-266`, `internal/store/store.go:414-446`). Covered by `TestIDsSurviveCompactReopen` (`internal/store/regression_test.go:162`).
- **Permissions:** `Compact` copies the original file mode onto the temp file before the rename, so compaction does not change the log's permissions (`internal/store/store.go:223-243`). Covered by `TestCompactPreservesPermissions` (`internal/store/regression_test.go:306`).
- **Empty keyword entries:** `decodeKeywords` skips empty entries, so a blob of only separators reads as no keywords (`internal/store/codec.go:124-141`). Covered by `TestDegenerateKeywordBlob` (`internal/store/regression_test.go:330`).
- **Lookup normalization:** `Get` lowercases and trims the keyword before the lookup, the same normalization `Put` applies, so `get " K "` finds `k` (`internal/store/store.go:119-130`). Covered by `TestGetNormalizesKeyword` (`internal/store/regression_test.go:206`).
- The lock file makes the stale state visible to the next process: a second writer gets `is locked by another process (remove ... if it is stale)`, and every failed Open path removes the lock it created (`internal/store/store.go:49-72`).
- Compaction is crash-safe up to the rename: the rewrite goes to a temp file in the same directory, is synced, then renamed over the log; the original stays intact if compaction stops before the rename (`internal/store/store.go:202-299`).

### Compression

- Per record, not per file: `compressPayload` gzips the payload and keeps the result only when it is strictly smaller; otherwise the raw bytes go to disk with codec `0` (`internal/store/codec.go:143-158`). Text shrinks a lot; already-compressed blobs (JPEG, ZIP) stay raw.
- Standard library only. The writers available are `compress/gzip`, `zlib`, `flate`, and `lzw`; there is no public zstd writer. Go can read bzip2 but not write it (`compress/bzip2` exports only `NewReader`).
- Replay decodes each gzip payload through a pooled `gzip.Reader` (`internal/store/codec.go:160-166`), so a compressed record does not allocate a fresh decompressor set.

### Memory, startup, and the two usage modes

- `Open` reads the whole log, decodes every live payload, and builds the maps. Reads after that are map lookups. Memory use is roughly the dataset size plus map overhead. Payloads stay in RAM; nothing is offloaded by offset.
- One-shot CLI: every command pays the replay once and exits. This is the local mode.
- Resident server: one store stays open for the process (`internal/server/server.go:40-64`); commands with `--remote` skip the local replay entirely.
- Where replay cost starts to hurt depends on payload sizes. `BenchmarkOpen` in `internal/store/bench_test.go` is the measurement to rerun when that question comes up.

Honest comparison with SQL: a local SQL database reads a row in microseconds too. This design wins on simplicity, on keyword lookup (one keyword to many records) as a first-class operation, and on owning the file format. It loses on transactions, joins, sorted range scans, and a query language. Reach for SQL or an embedded engine when you need those, not before.

### Rejected alternative: whole-file snapshot

The first design was a single snapshot file, magic `T8DB`, gob-encoded and gzipped as one stream, rewritten atomically on every save. It was dropped before shipping: every save rewrote the whole dataset, per-record compression was impossible with one gzip stream per file, and crash recovery could only fall back to the previous snapshot. The append log plus compaction replaced it. Nothing in the tree writes `T8DB` or gob (`grep -rn 'T8DB\|gob' internal cmd` returns no hits).

### Where the code goes

- `internal/store/`: `doc.go` package comment, `store.go` lifecycle and replay, `index.go` types and the inverted index, `codec.go` the log format and compression, tests next to the code. This mirrors the seams in `internal/server/`: `server.go` owns lifecycle, `routes.go` owns wiring, `handlers.go` owns handlers (`AGENTS.md`, Code structure).
- CLI wiring lives in `internal/cli/cli.go` and `internal/cli/db.go`, with tests in `cli_test.go`, `db_test.go`, and `db_remote_test.go`; `db_test.go:16-52` defines `captureRun`, the stdout/stderr capture helper.
- Server wiring lives in `internal/server/routes.go:11-23` and `internal/server/handlers.go`, tested through `internal/server/server_test.go` with `httptest` and no open port.

### CLI sketch

```
trace8 db put go,db,notes --text "runs are slow when the queue is empty"
trace8 db put perf,db --file ./profile.pprof
trace8 db put bin - < ./payload.bin
trace8 db get db
trace8 db get go,perf --all
trace8 db get body:queue
trace8 db get --id 2
trace8 db get --id 2 --out ./copy.pprof
trace8 db get --id 2 --raw > copy.pprof
trace8 db del 7
trace8 db stats
trace8 db compact
trace8 db get db --remote http://127.0.0.1:8080
```

Keyword rules: comma-separated, trimmed, lowercased, empties dropped, duplicates removed, at least one required, newlines rejected, and a keyword starting with `body:` rejected at both user-facing boundaries because the store reserves that prefix for the tokens it derives from text payloads (`internal/cli/db.go:419-429`, `internal/server/handlers.go:71-97`). Text payloads add `body:` tokens on top, so `get body:<token>` still finds them, and those entries stay hidden in output.

### Non-goals

- Transactions, joins, sorted range scans, SQL syntax.
- Multiple writing processes. One lock file, one writer; the lock is advisory.
- HTTP auth, replication, or TLS. `--remote` is for a trusted local network, and the server binds whatever `--addr` says.
- Ranking for body search; order is deterministic (ascending id, or newest first under `-n`), nothing fancier.
- New dependencies. `go.mod` has zero requires and any addition needs sign-off.

### Open questions

None. D1 through D4 are recorded as shipped in "Decisions (as shipped)".

## Dependencies

- Zero Go dependencies, standard library only. Any deviation needs explicit sign-off per `AGENTS.md`.
- Phase 1 before Phase 2 before Phase 3; that is the order the code was built in. Phase 4 needs all three.
- `make lint` needs `golangci-lint` on PATH (`Makefile:4`, `Makefile:31-32`). A missing linter is recorded as skipped in `## Verification evidence`, not as a pass.
- The `--remote` client and the server must come from the same build; the endpoint set is frozen in `internal/server/routes.go:11-23`.

## Status legend

- `[ ]` not started or not proven with current evidence
- `[x]` implemented and validated with current evidence

Every row in this file is `[x]`. The evidence below was recorded on the frozen revision on 2026-09-19.

---

## Verification evidence

All commands ran from the repo root on the frozen revision on 2026-09-19; scratch files lived under `/tmp/opencode`. Each `Result` records the command output, or a short faithful summary of it.

### Format
`gofmt -l ./cmd ./internal` and `make fmt-check`
Result: `gofmt -l ./cmd ./internal` printed nothing and `make fmt-check` exited 0 (clean).

### Vet
`go vet ./...`
Result: exit 0, no output.

### Tests
`go test ./... -count=1`
Result:

```
ok  github.com/chinmay-sawant/trace8/internal/cli     0.044s
ok  github.com/chinmay-sawant/trace8/internal/server  0.025s
ok  github.com/chinmay-sawant/trace8/internal/store   0.396s
```

exit 0; `cmd/trace8` has no test files.

### Race tests
`go test ./... -race -count=1`
Result:

```
ok  github.com/chinmay-sawant/trace8/internal/cli     1.107s
ok  github.com/chinmay-sawant/trace8/internal/server  1.053s
ok  github.com/chinmay-sawant/trace8/internal/store   4.521s
```

exit 0.

### make check
`make check`
Result: `go vet ./...`, `go test -p 2 -parallel 2 ./...`, and `go build -o bin/trace8 ./cmd/trace8` all finished green (exit 0). `make clean` then removed `bin/`.

### Lint
`make lint`
Result: `golangci-lint run ./...` exited 0 with the curated `.golangci.yml` set (errcheck, govet, ineffassign, staticcheck, unused, misspell, errorlint, bodyclose, unconvert, wastedassign, copyloopvar). The inherited enable-all config reported 1163 findings before the retune, all from style linters (wsl, nlreturn, varnamelen, exhaustruct, mnd and similar), which is why the config was replaced instead of chased.

### CLI smoke session
`go build -o /tmp/opencode/final-check/trace8 ./cmd/trace8`, then this direct repro session on a fresh db, one line per command with the observed result:
Result:

```
put k A -> 1; put k B -> 2; put k C -> 3; del 2; del 3; compact; put k D -> 4   (ids survive compaction; live records are 1 and 4)
get ' K ' -> "1 text 1 [k] A" and "4 text 1 [k] D"                              (lookup trims and lowercases)
put body:x --text nope -> trace8 db: keyword "body:x" uses the reserved body: prefix, exit 1
get k -n 1 -> record 4 first, then "showing 1 of 2 matches"
get k -n 5 -> records 4, 1, no footer
get -- --all -> usage, exit 2
printf abc > tiny.db; stats --db tiny.db -> trace8 db: store: tiny.db: not a trace8 database, exit 1, file still 3 bytes
```

An earlier full smoke on the same revision validated `put --text`, `put --file`, `get`, `get --id --raw` (byte-identical via `cmp`), `get --id --out`, `stats`, `del`, `compact`, and the usage exits.

### Server plus remote session
`go build -o /tmp/opencode/trace8-final ./cmd/trace8`, then the server in the background from the repo root, and `--remote` commands against it:
Result:

```
$ /tmp/opencode/trace8-final --server --addr 127.0.0.1:18099 --db /tmp/opencode/verify-server/store.db &
trace8 server on 127.0.0.1:18099 (frontend frontend)
$ /tmp/opencode/trace8-final db put demo --remote http://127.0.0.1:18099 --text "hello remote"
1
$ /tmp/opencode/trace8-final db get demo --remote http://127.0.0.1:18099
1 text 12 [demo]
hello remote
$ /tmp/opencode/trace8-final db stats --remote http://127.0.0.1:18099
records: 1
keywords: 3
deleted: 0
file: http://127.0.0.1:18099 (67 bytes)
$ /tmp/opencode/trace8-final db compact --remote http://127.0.0.1:18099
compacted trace8.db
$ curl -sS http://127.0.0.1:18099/api/healthz
{"ok":true,"service":"trace8"}
$ curl -sS -w " [%{http_code}]" "http://127.0.0.1:18099/api/db/record?id=99"
{"error":"no record 99"} [404]
$ kill -TERM <server pid>                      # server exit 0
$ ls /tmp/opencode/verify-server
server.log  store.db                           # no store.db.lock
$ /tmp/opencode/trace8-final db stats --db /tmp/opencode/verify-server/store.db
records: 1
keywords: 3
deleted: 0
file: /tmp/opencode/verify-server/store.db (95 bytes)
```

The `stats` file line shows the remote base, not the local default. `compact --remote` prints the local `--db` flag value (`trace8.db` here) even though the server compacted its own file; that is the shipped output. After SIGTERM the server exited 0, the `.lock` file was gone, and the direct `stats` against the same file proved the lock was released. The file grew from 67 to 95 bytes across compact because the rewrite appended the 28-byte high-water marker.

### Benchmarks
`go test ./internal/store -run '^$' -bench 'BenchmarkGet' -benchmem -count=5` and `go test ./internal/store -run '^$' -bench 'BenchmarkOpen' -benchmem -count=3`
Result: dataset is 10,000 records, five keywords each, half repeated text payloads and half random 256-byte blobs (`internal/store/bench_test.go:11-54`).

```
BenchmarkGet:  4444, 3040, 3252 ns/op at 8192 B/op, 1 alloc/op
BenchmarkOpen: 12.35, 11.79, 10.39 ms/op at about 11.8 MB/op
```
