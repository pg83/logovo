package main

import (
	"cmp"
	"database/sql"
	"flag"
	"hash/fnv"
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
    bytes INTEGER NOT NULL,
    first_rowid INTEGER NOT NULL,
    parent TEXT NOT NULL DEFAULT '',
    copied INTEGER NOT NULL DEFAULT 0,
    parent_n INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sessions_first_rowid ON sessions (first_rowid, copied);
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

	// The files SQLite spills temporary tables to stay out of TMPDIR too.
	throw2(db.Exec("PRAGMA temp_store_directory = '" + dir + "'"))

	tx := throw2(db.Begin())
	insSession := throw2(tx.Prepare("INSERT INTO sessions (session, agent, host, user, cwd, title, first_ts, last_ts, turns, bytes, first_rowid) VALUES (?,?,?,?,?,?,?,?,?,?,?)"))
	insDoc := throw2(tx.Prepare("INSERT INTO docs (rowid, body, session, n, role, ts) VALUES (?,?,?,?,?,?)"))

	// What fork detection compares: a key for every doc, under the doc's
	// rowid. A temp table lives only as long as the connection, so it never
	// reaches the published file; it has to be made inside the transaction,
	// which holds the one connection it exists on.
	throw2(tx.Exec("CREATE TEMP TABLE keys (session TEXT, n INTEGER, ts TEXT, key INTEGER, text INTEGER)"))
	insKey := throw2(tx.Prepare("INSERT INTO keys (rowid, session, n, ts, key, text) VALUES (?,?,?,?,?,?)"))
	sessions, docs := 0, 0
	lastTS := ""
	started := time.Now()

	// The docs of a session get consecutive rowids in order of n, starting
	// at its first_rowid, so serve reads a session as one rowid range
	// instead of scanning the whole docs table.
	rowid := int64(1)

	// Every pile file holds portions of many sessions, sorted by session;
	// read all of them at once, one whole session at a time. Files from
	// before the pile was sorted are left to merge to sort out.
	var keys []string

	for _, o := range store.list("pile/") {
		if strings.HasSuffix(o.key, pileSorted) {
			keys = append(keys, o.key)
		}
	}

	// A pile file that breaks off costs only what follows the break: the
	// index is rebuilt from scratch every run.
	eachSession(store, keys, false, func(session string, lines [][]byte) {
		exc := try(func() {
			n := normalize(session, lines)

			if n == nil {
				return
			}

			i := n.info
			throw2(insSession.Exec(i.Session, i.Agent, i.Host, i.User, i.Cwd, i.Title, i.FirstTS, i.LastTS, i.Turns, i.Bytes, rowid))

			for _, d := range n.docs {
				throw2(insDoc.Exec(rowid, d.Body, i.Session, d.N, d.Role, d.TS))
				throw2(insKey.Exec(rowid, i.Session, d.N, d.TS, docKey(d), textKey(d)))
				rowid++
			}

			sessions++
			docs += len(n.docs)

			if i.LastTS > lastTS {
				lastTS = i.LastTS
			}
		})

		if exc != nil {
			slog.Warn("index: skipping session", "session", session, "err", exc.Error())
		}
	})

	for session, f := range findForks(tx) {
		throw2(tx.Exec("UPDATE sessions SET parent = ?, copied = ?, parent_n = ? WHERE session = ?", f.parent, f.copied, f.parentN, session))
	}

	for k, v := range map[string]string{
		"built_at": nowRFC3339(),
		"last_ts":  lastTS,
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

// docKey identifies a doc by its time, author and text, all of which a
// fork copies from its parent unchanged.
func docKey(d doc) int64 {
	h := fnv.New64a()
	io.WriteString(h, d.TS+"\x00"+d.Role+"\x00"+d.Body)

	return int64(h.Sum64())
}

// textKey identifies a doc by its author and text alone: the record a
// fork was made at is copied too, but stamped with the time of the fork.
func textKey(d doc) int64 {
	h := fnv.New64a()
	io.WriteString(h, d.Role+"\x00"+d.Body)

	return int64(h.Sum64())
}

// fork is what a session copied from the one it was forked from: its
// first copied docs, which end at the parent's doc parentN.
type fork struct {
	parent  string
	copied  int
	parentN int
}

// forkPairs takes every two sessions s and t that share docs and keeps
// those where the shared docs of s are exactly its first cs: s is then a
// candidate fork of t, and mt is the last of the shared docs in t. The
// other columns tell whether the shared docs start t as well, and when the
// doc that follows them was written in s and in t, empty when none does;
// the last one, whether the doc after them in s has the text of the doc
// after the last shared one in t, the record the fork was made at.
const forkPairs = `
SELECT s, t, cs, mt, ct = mt + 1,
    ifnull((SELECT ts FROM keys WHERE rowid = (SELECT first_rowid FROM sessions WHERE session = s) + cs AND session = s), ''),
    ifnull((SELECT ts FROM keys WHERE rowid = (SELECT first_rowid FROM sessions WHERE session = t) + ct AND session = t), ''),
    ifnull((SELECT text FROM keys WHERE rowid = (SELECT first_rowid FROM sessions WHERE session = s) + cs AND session = s), 0) =
    ifnull((SELECT text FROM keys WHERE rowid = (SELECT first_rowid FROM sessions WHERE session = t) + mt + 1 AND session = t), 1)
FROM (
    SELECT a.session AS s, b.session AS t, count(DISTINCT a.n) AS cs, max(a.n) AS ms, count(DISTINCT b.n) AS ct, max(b.n) AS mt
    FROM keys a JOIN keys b ON b.key = a.key AND b.session != a.session
    GROUP BY a.session, b.session
)
WHERE cs = ms + 1
ORDER BY s, cs DESC, t`

// findForks tells which sessions are forks and of what. Forking a
// conversation in Claude Code starts a new session with a copy of the
// parent's records up to the fork point, times included, so all of the
// fork's first docs turn up in the parent; the parent may hold more docs
// in that stretch, such as tool results the fork did not copy.
func findForks(tx *sql.Tx) map[string]fork {
	throw2(tx.Exec("CREATE INDEX keys_key ON keys (key, session, n)"))
	rows := throw2(tx.Query(forkPairs))
	defer rows.Close()

	type pair struct {
		fork, parent string
	}

	candidates := map[pair]fork{}
	best := map[string]fork{}

	for rows.Next() {
		var s, t, nextS, nextT string
		var cs, mt int
		var prefix, restamped bool
		throw(rows.Scan(&s, &t, &cs, &mt, &prefix, &nextS, &nextT, &restamped))

		// When the shared docs start t too, either could be the fork. The
		// fork stamps the record it was made at with its own time, so the
		// parent is the one whose next doc is older.
		if prefix && cmp.Or(cmp.Compare(nextT, nextS), cmp.Compare(t, s)) > 0 {
			continue
		}

		f := fork{t, cs, mt}

		// The record the fork was made at is a copy too, under a new time.
		if restamped {
			f.copied++
			f.parentN++
		}

		candidates[pair{s, t}] = f

		if f.copied > best[s].copied {
			best[s] = f
		}
	}

	throw(rows.Err())
	forks := map[string]fork{}

	// The parent is the session copied from the most; but a fork of a fork
	// that copied no more than the latter did shares as much with the
	// original, which is then the parent. The climb is bounded in case the
	// data loops.
	for s, f := range best {
		for range len(best) {
			up, ok := best[f.parent]

			if !ok || up.copied < f.copied {
				break
			}

			next, ok := candidates[pair{s, up.parent}]

			if !ok {
				break
			}

			f = next
		}

		forks[s] = f
	}

	return forks
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
