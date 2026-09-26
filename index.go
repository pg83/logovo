package main

import (
	"database/sql"
	"flag"
	"io"
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

	// Every pile file holds portions of many sessions, sorted by session;
	// read all of them at once, one whole session at a time. Files from
	// before the pile was sorted are left to merge to sort out.
	var keys []string

	for _, o := range store.list("pile/") {
		if strings.HasSuffix(o.key, pileSorted) {
			keys = append(keys, o.key)
		}
	}

	eachSession(store, keys, func(session string, lines [][]byte) {
		exc := try(func() {
			n := normalize(session, lines)

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
	})

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

	if keep != "" {
		copyFile(path, keep)
	}

	zst := path + ".zst"
	compressFile(path, zst)
	store.putFile(indexKey, zst)

	slog.Info("index: published", "sessions", sessions, "docs", docs, "bytes", fileSize(path), "compressed", fileSize(zst), "took", time.Since(started).Round(time.Millisecond))
}

func compressFile(src, dst string) {
	in := throw2(os.Open(src))
	defer in.Close()

	out := throw2(os.Create(dst))
	enc := throw2(zstd.NewWriter(out))
	_, cerr := io.Copy(enc, in)
	throw(enc.Close())
	throw(out.Close())
	throw(cerr)
}
