# logovo

Agent session logs (Claude Code, Codex) from every host, gathered into the
lab, merged per session, indexed, searchable by people and by agents.

## Conventions

- Style: `STYLE.md`. One `package main`, all `.go` files in the repo root.
- Errors go through `throw.go` (`throw`, `throw2`, `try`, `catch`); no
  `if err != nil { return err }`.
- Config is flags and environment only; `logovo scan` runs with no
  configuration at all on a host that keeps sessions in the usual places.
- Commit messages in English.

## Pipeline

```
scan (every host)     POST https://logovo.lab.mesh/v1/portions
  ~/.claude/projects/*/<uuid>.jsonl                 |  lab_proxy routes the name
  ~/.codex/sessions/*/*/*/rollout-*-<uuid>.jsonl    |  to web on loopback
  <file>.latest = bytes already shipped             v
web (every lab host)      /            the page
                          /v1/portions -> collect   queue/<uuid>.<md5>
                          /v1/*        -> serve
merge (job, every minute)      queue/* (already in session order) repacked into one
                               pile/<level>-<seq>.sorted, then pairs of same-size pile
                               files merged by session into the next size class
index (job, every N minutes)   pile/*.sorted merged by session, one session at a time
                               -> normalize -> index/logovo.sqlite.zst
serve (every lab host)         fetches the index when it changes; /v1/search, /v1/sessions/<uuid>
search, show (CLI)             thin clients, default https://logovo.lab.mesh
```

- A portion is one JSON line (`Portion` in `portion.go`) in one zstd frame.
  The pile is the same lines, many sessions in one file but grouped by
  session in ascending order, repacked into single zstd streams whose
  sizes climb by powers of two (`merge.go`). Because every pile file is
  sorted, merge and index stream them and hold one session at a time,
  never the whole history. Pile files from before sorting (no `.sorted`
  suffix) and legacy `sessions/` objects are sent back to the queue by
  merge and folded anew. merge is not idempotent on purpose: a crash
  leaves duplicate portions, and the indexer drops duplicates by md5 and
  orders by offset.
- The store is `dir:/path` or `s3://bucket` (`store.go`); tests use `dir:`.
- The index is SQLite with FTS5 through `modernc.org/sqlite` (pure Go, no
  cgo). One document per message: user text, assistant text, tool calls
  (`▶ name: command`), tool results (`◀ …`, truncated). Thinking, encrypted
  reasoning and Codex's injected instructions are not indexed.

## Build and test

- `./build` builds `.build/bin/logovo` and publishes `./logovo`.
- `./build test` runs the e2e suite in `tst/` (Python, real binaries, a
  directory store). No Go unit tests.
- `./build -Drace test` runs it with the race detector (needs a C compiler).
- `./lint.sh` before committing.
