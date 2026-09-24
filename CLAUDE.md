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
merge (job, every minute)      queue/* appended to sessions/<uuid>, queue emptied
index (job, every N minutes)   sessions/* -> normalize -> index/logovo.sqlite.zst
serve (every lab host)         fetches the index when it changes; /v1/search, /v1/sessions/<uuid>
search, show (CLI)             thin clients, default https://logovo.lab.mesh
```

- A portion is one JSON line (`Portion` in `portion.go`) in one zstd frame.
  A session object is those frames concatenated; nothing on the lab side
  looks inside until the indexer does. The indexer drops duplicate
  portions by md5 and orders them by offset, so merge needs no state.
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
