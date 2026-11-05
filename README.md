# logovo

Collects Claude Code and Codex session logs from your hosts, keeps them per
session in S3, indexes them with SQLite FTS5 and lets you, or an agent,
search them.

```
logovo scan                      # on every host; no configuration needed
logovo search kernel build       # from anywhere on the mesh
logovo show <session> -from 40 -to 60
```

Lab side: `collect` receives portions on a mesh address, `merge` and `index`
run as scheduled jobs, `serve` answers `/v1/search` and `/v1/sessions/<id>`
from the latest index, `web` is the page in front of it.

See `CLAUDE.md` for the pipeline and `logovo` without arguments for the flags.

`serve` streams the compressed index from storage into a local file and
keeps the loaded index if a refresh fails. `index` also preserves the
published index on archive read errors. For deliberate recovery,
`index -allow-partial` publishes readable data and marks `/v1/status` with
`complete=false` and `broken_files`; the page displays an incomplete archive.
Damaged queue objects are retained under `bad/queue/` by `merge`.

When upgrading an archive created before sorted pile files, stop the
scheduled `index` job, update the `merge` and `index` jobs, run `merge`
followed by `index`, and then update `serve`. Resume the index schedule
afterwards. The first merge converts old `pile/` and `sessions/` objects
to `pile/*.sorted`; indexing refuses unmigrated objects. Keep the new
archive workers in place: old merge binaries write the previous format.
The new API requires a rebuilt index with the fork metadata columns.

`scan` warns and skips a single record whose compressed portion exceeds
the collector's 16 MiB limit, then continues shipping later records.
The original record remains in the local session log.
