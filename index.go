package main

import (
	"bytes"
	"database/sql"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

const indexKey = "index/logovo.sqlite.zst"

const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE sessions (
    session TEXT PRIMARY KEY,
    agent TEXT NOT NULL,
    host TEXT NOT NULL,
    user TEXT NOT NULL,
    cwd TEXT NOT NULL,
    title TEXT NOT NULL,
    first_ts TEXT NOT NULL,
    last_ts TEXT NOT NULL,
    turns INTEGER NOT NULL,
    bytes INTEGER NOT NULL
);
CREATE VIRTUAL TABLE docs USING fts5(
    body,
    session UNINDEXED,
    n UNINDEXED,
    role UNINDEXED,
    ts UNINDEXED,
    tokenize='unicode61 remove_diacritics 2'
);
`

func indexMain(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	storeSpec := fs.String("store", "", "dir:/path or s3://bucket")
	out := fs.String("out", "", "also keep the uncompressed database at this path")
	throw(fs.Parse(args))

	if *storeSpec == "" {
		throwFmt("index: -store is required")
	}

	buildIndex(openStore(*storeSpec), *out)
}

// buildIndex reads every session, normalizes it and writes one fresh
// database; the previous index is replaced whole, nothing incremental.
func buildIndex(store objectStore, keep string) {
	// Built next to the working directory, not in TMPDIR: as a gorn task
	// the job may only write where it runs.
	dir := throw2(os.MkdirTemp(".", "logovo-index-"))
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "logovo.sqlite")
	db := throw2(sql.Open("sqlite", "file:"+path))
	throw2(db.Exec("PRAGMA journal_mode=OFF"))
	throw2(db.Exec("PRAGMA synchronous=OFF"))
	throw2(db.Exec(schema))

	tx := throw2(db.Begin())
	insSession := throw2(tx.Prepare("INSERT INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?)"))
	insDoc := throw2(tx.Prepare("INSERT INTO docs (body, session, n, role, ts) VALUES (?,?,?,?,?)"))
	sessions, docs := 0, 0
	started := time.Now()

	for _, key := range store.list("sessions/") {
		session := strings.TrimPrefix(key, "sessions/")

		if !uuidRe.MatchString(session) {
			continue
		}

		exc := try(func() {
			n := normalize(session, decodeFrames(store.get(key)))

			if n == nil {
				return
			}

			i := n.info
			throw2(insSession.Exec(i.Session, i.Agent, i.Host, i.User, i.Cwd, i.Title, i.FirstTS, i.LastTS, i.Turns, i.Bytes))

			for _, d := range n.docs {
				throw2(insDoc.Exec(d.Body, i.Session, d.N, d.Role, d.TS))
			}

			sessions++
			docs += len(n.docs)
		})

		if exc != nil {
			slog.Warn("index: skipping session", "session", session, "err", exc.Error())
		}
	}

	for k, v := range map[string]string{
		"built_at": nowRFC3339(),
		"sessions": itoa(int64(sessions)),
		"docs":     itoa(int64(docs)),
	} {
		throw2(tx.Exec("INSERT INTO meta VALUES (?,?)", k, v))
	}

	throw(tx.Commit())
	throw2(db.Exec("INSERT INTO docs(docs) VALUES ('optimize')"))
	throw(db.Close())

	raw := throw2(os.ReadFile(path))

	if keep != "" {
		throw(os.WriteFile(keep, raw, 0644))
	}

	var buf bytes.Buffer
	enc := throw2(zstd.NewWriter(&buf))
	throw2(enc.Write(raw))
	throw(enc.Close())
	store.put(indexKey, buf.Bytes())

	slog.Info("index: published", "sessions", sessions, "docs", docs, "bytes", len(raw), "compressed", buf.Len(), "took", time.Since(started).Round(time.Millisecond))
}
