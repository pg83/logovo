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
