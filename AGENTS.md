# AGENTS.md - trace8

> This file is the conventions ledger for every coding agent working in this
> repo (opencode, grok, gemini, codex, antigravity/agy, claude). Read it at
> session start.

## Project

trace8 is a small Go CLI plus a placeholder web frontend. The CLI lives in
`cmd/trace8` and runs `internal/cli.Run`: `trace8 [--version] [name]`
prints `hello <name>` (`hello world` by default), `--version` prints
`trace8 <Version>`, `trace8 db put|get|del|stats|compact [flags]` reads or
writes the local keyword store in `internal/store` with `--db` (default
`trace8.db`) or talks to a running server with `--remote`, and `--server
[--addr :8080] [--db trace8.db]` runs the stdlib HTTP server in
`internal/server` until interrupted, serving the `/api/db/*` endpoints
plus `frontend/` statically. The frontend in `frontend/` is a static
placeholder (`src/app.js` sets the text of `#app`); there is no
frontend build step.

Module: `github.com/chinmay-sawant/trace8`.
Binary: `cmd/trace8` (`internal/cli.Version`, currently `0.0.1` in
`internal/cli/cli.go`).
There is a `Makefile` wrapping the gates (`make check` runs vet, tests,
build), but no `knowledge-base/`, no `scripts/`, and no release or
version-stamp tooling. Tests live next to the code
(`internal/store/*_test.go` covers the log format, replay, truncated
tails, compaction, and concurrency; `internal/cli/*_test.go` covers the
CLI contract and every `db` subcommand; `internal/server/server_test.go`
covers the API, the `/api/db/*` endpoints, and server shutdown). Do not
reference paths that do not exist as if they do; create them only when
the task actually needs them.

## Todo protocol - response-only (mandatory)

Every agent must manage todos live in the API response, not on disk. This is
the first action on any task, before any reads, edits, or other tool calls.

1. **Create todos in the response via API before any work.** On receiving any
   task (feature, fix, docs, question with multi-step work), immediately call
   the todo API (`todowrite` or equivalent) to publish the plan as todos. Do
   not start work until the todo list is visible in the API response.
2. **Show current todos in every response.** Each assistant turn must render
   the current todo list with status markers (`pending`, `in_progress`,
   `completed`, `cancelled`) and clearly highlight which item is
   `in_progress`. The todo list is the live progress bar for the user.
3. **Do not store todos on disk.** Do not create `TODO.md`, `todos.json`, or
   any other todo-tracking file. Todos live only in the API response state.
4. **Keep response todos updated as you go.** Mark items `in_progress` when
   started and `completed` when finished. If scope changes, update the list
   immediately.
5. **Completion requires todos to show done.** A task is done only when all
   todos show `completed` and you have sent a final summary stating what
   shipped.

If the todo API is unavailable, state that in the response and list todos
inline as a fallback - still do not write a file.

## Golden rules

1. **No git commands without explicit permission.** Never run `git add`,
   `git commit`, `git push`, `git restore`, `git clean`, `git reset`, or
   `git stash` unless the user asks. Subagent prompts carry this ban by
   default.
2. **No em dashes in any written output, docs, or commit messages.**
   Use plain hyphens or restructure. This includes this file.
3. **Commit at session end; never leave a dirty tree.** Uncommitted work is
   the top cause of cross-session rework. If interrupted, record what landed.
   Ask before committing if the user has not authorized it.
4. **Branch naming:** lowercase `feature/`, `fix/`, `chore/`, or
   `docs/<short-description>`. Verify the branch name before the first commit.
5. **PRs** use `skills/PR/PR_TEMPLATE.md`; issues use
   `skills/PR/ISSUE_TEMPLATE.md`. Whenever a user asks to raise, update, or
   review a PR, read the PR template first. (The template still carries
   gowkhtmltopdf example text; use its structure, not its old repo names.)
   PRs require self-assignment and at least one label.
6. **Checklists are live ledgers:** if a task creates a phase checklist,
   close rows `[x]` in the same change that implements them, and only when
   the gate actually passed. Never mark `[x]` from intent.
7. **Answer from code.** Open the real source under `cmd/` and `internal/`
   before answering. If docs and code disagree, trust the code and fix the
   docs.
8. **Unslop all writing.** Before writing plans, docs, knowledge-base pages,
   PR/issue text, commit messages, or user-facing replies, apply `/unslop`.
   Cut puffery, em dashes, chatbot filler, synonym cycling, title-case
   headings.
9. **Feynman every explanation and document.** Whenever you respond with an
   explanation or produce documentation (plans, README sections, PR bodies,
   design notes), apply `skills/feynman/SKILLS.md`: plain words a smart
   12-year-old could retell, grounded in real source (`file:line` citations),
   self-audited for jargon, hand-waves, circularity, and name-dropping until
   zero findings remain. Report the pass count when the loop runs as `/feynman`.

## Unslop (`/unslop`)

Apply to every prose surface: chat replies, `plans/`, README, CHANGELOG
entries, PR and issue bodies, commit messages. Rewrite to plain human voice,
then self-audit: "What makes this obviously AI generated?" Prefer concrete
facts and file paths over vague summary language.

## Feynman (`/feynman`)

Apply to every response that explains something and to every documentation
surface. Load `skills/feynman/SKILLS.md` and run its loop: explain in plain
words, audit your own explanation for jargon / hand-waves / circularity /
name-dropping, fill each gap from real source with `file:line` citations,
repeat until zero findings. Never explain from memory alone; read the code
or doc first. Report the pass count when invoked as `/feynman`.

## Verification gates

The `Makefile` wraps plain Go commands (`make build` runs `go build`,
`make test` runs `go test`, `make vet` runs `go vet`):

| Gate | Command | What it proves |
|------|---------|----------------|
| Build | `make build` | Whole tree compiles into `bin/` |
| Tests | `make test` | Full suite green |
| Vet | `make vet` | Standard vet clean before claiming done |
| Lint | `make lint` | golangci-lint clean before opening any PR |
| Format | `make fmt-check` | `gofmt` clean |

`make check` runs vet, tests, then build. `make test` caps parallelism
(`-p 2 -parallel 2` via `TEST_P` / `TEST_PARALLEL`); raise both only with
RAM to spare (`make test TEST_P=4 TEST_PARALLEL=4`), force a fresh run
(`make test GO_TEST_FLAGS='-count=1'`). Run the full set once at session
end before claiming done. During a session,
targeted runs (`go test ./internal/cli -run '<TestName>'`) are enough after
each edit.

## Things to AVOID (paid-for lessons)

1. **Lint whack-a-mole.** `.golangci.yml` is a curated set
   (errcheck, govet, ineffassign, staticcheck, unused, misspell, errorlint,
   bodyclose, unconvert, wastedassign, copyloopvar). The inherited
   enable-all config produced 1163 style-only findings (wsl, nlreturn,
   varnamelen, exhaustruct, mnd and similar), so it was replaced rather
   than chased. Run `golangci-lint run ./...` before opening any PR and
   keep it green; `//nolint` is a last resort with a written reason.
2. **Verifying against stale artifacts.** Rebuild before verifying CLI
   behavior: `go run ./cmd/trace8` or `go build -o /tmp/trace8 ./cmd/trace8`.
   Do not assert behavior from a previously built binary.
3. **Claiming completion without the gate output.** Read the final exit code.
   A task is done when the last validation exits 0, not when you expect it
   to.
4. **Guessing APIs and paths.** Grep the symbol, glob the file, read the
   package doc comment before writing against it. `go build` before writing
   tests.
5. **Scope creep.** Edit only the files in the task.
6. **Whole-file re-reading.** View targeted ranges/diffs. Line-by-line claims
   require actual coverage.
7. **Throwaway tooling.** Anything used twice belongs in `scripts/` with a
   note. Do not create `scripts/` for one-offs.
8. **Dead subagents.** Confirm spawned agents produced turns; diagnose
   before re-spawning identically; check cancelled agents for landed work
   before redoing it.
9. **Parallel agents on one shared tree.** One agent owns one package
   (e.g. one on `internal/cli`, another on `frontend`, never both on the
   same files).
10. **Referencing gowkhtmltopdf-era paths.** The `Makefile` has `build`,
    `test`, `vet`, `lint` (`Makefile:31-32`), `fmt`, `fmt-check`, `run`,
    `server`, `clean`, `check`, and `help`. There is no `make golden`,
    `make claim-scan`, `testdata/golden`, `output/`, `documentation/`, or
    `knowledge-base/` in this repo. Do not cite them as gates or
    evidence.

## trace8 specifics

- **CLI contract** (`internal/cli/cli.go`): flag parsing with
  `flag.ContinueOnError`, usage goes to stderr with exit 2 on bad flags,
  `--version` prints `trace8 <Version>` with exit 0, `--server`
  (`--addr`, default `:8080`, `--db`, default `trace8.db`) serves and
  blocks until interrupt with graceful shutdown, a first positional `db`
  routes to the subcommands in `internal/cli/db.go`, otherwise prints
  `hello <name>`. Keywords starting with `body:` are reserved and are
  rejected by the CLI (`trace8 db put`, exit 1) and by the HTTP API
  (400). Keep all behaviors covered when adding flags; `--version`
  wins over `--server` and over `db`.
- **Tests.** `internal/cli/cli_test.go` covers the CLI contract
  (`--version`, default name, named arg, bad flag exit code);
  `internal/cli/db_test.go` covers every `db` subcommand (usage exits,
  put sources, the reserved `body:` prefix, get union, intersection,
  raw, out, limit, keyword normalization, the `--` terminator, body
  search, del, stats, compact, bad path); `internal/cli/db_remote_test.go`
  covers `--remote` against an `httptest` server; `internal/store/*_test.go`
  covers the log codec, replay, truncated tails, corruption, compaction,
  permissions, the sticky write error, concurrency, and benchmarks;
  `internal/server/server_test.go` covers `/api/healthz`, `/api/hello`,
  the `/api/db/*` endpoints, the 64 MiB put cap, trailing JSON, keyword
  validation, the JSON `/api/` 404, server timeouts, and shutdown
  releasing the store, via `httptest` without opening a port. Extend
  these files before adding matching features.
- **Version discipline.** `internal/cli.Version` is a plain constant
  (`0.0.1`); there is no `VERSION` file, no ldflags injection, and no
  version-stamp check. Add that machinery only when cutting a real release.
- **Frontend is a placeholder.** `frontend/src/app.js` touches `#app` and
  nothing else. Do not treat it as a build pipeline or site generator.
  `--server` serves it from disk (`FrontendDir`, default `frontend/`)
  with no build step.
- **No dependencies.** `go.mod` has zero requires. Any dependency addition
  is a project-policy change: announce its purpose and get explicit user
  sign-off. No silent additions.

## Code structure

The tree is tiny on purpose:

- `cmd/trace8/main.go` - entrypoint, delegates to `cli.Run`.
- `internal/cli/` - flag parsing and output (`cli.go`, `db.go` for the
  `db` subcommands and the `--remote` client, `doc.go`, tests); imports
  `internal/server` only for `--server` mode and `internal/store` for the
  local db backend.
- `internal/server/` - stdlib HTTP surface (`doc.go`, `server.go` for
  lifecycle and the store handle, `routes.go` for wiring, `handlers.go`
  for handlers, `server_test.go`).
- `internal/store/` - append-only local keyword store (`doc.go` package
  comment, `store.go` for lifecycle, replay, and compaction, `index.go`
  for types, normalization, tokenizing, and the inverted index,
  `codec.go` for the `T8LG` log format and per-record gzip, tests next to
  the code).
- `frontend/` - static placeholder (`index.html`, `src/app.js`).

File size soft limit: about 2,000 lines per file. No Go file crosses it
without a written reason recorded in the change. Split module-wise, not
length-wise: each stage, view, store, or profile gets a focused file, never
"cut here" chunks. Follow the existing seams (`server.go` owns lifecycle,
`routes.go` owns wiring, `handlers.go` owns handlers; `store.go` owns
lifecycle, `index.go` owns the in-memory index, `codec.go` owns the log
format).

Keep it that way: one package per responsibility, small constructors,
composition over fat structs. Verify with `go build ./...` + targeted
tests after any split; do not reorder or rename code during a split.

## Plans and ledgers

`plans/` exists. `plans/0.0.1/local-keyword-store.md` is the canonical
ledger for the db effort: the shipped append-log store, the `db` CLI, the
HTTP API, and the remote client. Keep one canonical ledger per effort,
close rows on proof only (golden rule 6), and use the phase checklist
shape from `skills/phase-wise-checklist/`. The ledger's rows stay open
until verifier evidence is pasted into its `## Verification evidence`
section; never check them from intent.

## Skills (this folder)

Active skills:

- `skills/PR/` - templates for PRs, issues, review comments
- `skills/feynman/` - plain-words explanation loop with self-audit;
  mandatory default for every explanatory reply and documentation surface
  (golden rule 9)
- `skills/phase-wise-checklist/` - evidence-backed plan ledgers
- `skills/improve-codebase/` - architecture/seams/go-practices audit pack
  producing a phase-wise ledger
- `skills/critical-go-review/`, `skills/perf-review/`,
  `skills/solid-go-review/` - review waves (read-only reviewers, then fix
  agents, then one test gate)
- `skills/release-note/` - release notes (still carries gowkhtmltopdf
  VERSION/ldflags steps; adapt before use)
- `skills/diff-verify/` - history-aware change review
- `skills/golang-anti-patterns/` - Go anti-patterns catalog
- `skills/ponytail*` - laziness protocol family (YAGNI reviews, debt ledger)

Archived (gowkhtmltopdf only, not trace8 context): `skills/_archive/`
holds `chrome-flex-pdf-closure`, `debug-html-template`,
`diagnose-golden-fixture`, `diagnose-fixture-picture`, and
`gowkhtmltopdf-reference.md`. See `skills/_archive/README.md`. Do not load
these for trace8 work.

## Dependency policy

`go.mod` currently requires nothing. Adding any dependency is a
project-policy change: announce its purpose and get explicit user sign-off.
No silent additions.

---

## FAQ - does the root AGENTS.md get read automatically?

Yes. Creating `AGENTS.md` at the repo root is the standard, and it is read
automatically by every major tool: opencode, codex, gemini CLI,
antigravity/agy, grok, claude code, cursor. You do not need anything under
`.agents/`. Keep this one file at the root and keep it current.
