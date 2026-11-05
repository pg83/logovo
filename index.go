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
	"strconv"
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

func indexMain(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	storeSpec := fs.String("store", "", "dir:/path or s3://bucket")
	out := fs.String("out", "", "also keep the uncompressed database at this path")
	allowPartial := fs.Bool("allow-partial", false, "publish readable data even when an archive object is damaged")

	throw(fs.Parse(args))

	if *storeSpec == "" {
		throwFmt("index: -store is required")
	}

	buildIndex(openStore(*storeSpec), *out, *allowPartial)
}

func buildIndex(store ObjectStore, keep string, allowPartial bool) {
	dir := throw2(os.MkdirTemp(".", "logovo-index-"))

	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "logovo.sqlite")
	db := throw2(sql.Open("sqlite", "file:"+path))

	defer db.Close()

	throw2(db.Exec("PRAGMA journal_mode=OFF"))
	throw2(db.Exec("PRAGMA synchronous=OFF"))
	throw2(db.Exec(schema))

	throw2(db.Exec("PRAGMA temp_store_directory = '" + dir + "'"))

	tx := throw2(db.Begin())

	defer tx.Rollback()

	insSession := throw2(tx.Prepare("INSERT INTO sessions (session, agent, host, user, cwd, title, first_ts, last_ts, turns, bytes, first_rowid) VALUES (?,?,?,?,?,?,?,?,?,?,?)"))
	insDoc := throw2(tx.Prepare("INSERT INTO docs (rowid, body, session, n, role, ts) VALUES (?,?,?,?,?,?)"))

	throw2(tx.Exec("CREATE TEMP TABLE keys (session TEXT, n INTEGER, ts TEXT, key INTEGER, text INTEGER)"))

	insKey := throw2(tx.Prepare("INSERT INTO keys (rowid, session, n, ts, key, text) VALUES (?,?,?,?,?,?)"))
	sessions, docs := 0, 0
	lastTS := ""
	started := time.Now()
	rowid := int64(1)

	var keys []string

	if len(store.list("sessions/")) > 0 {
		throwFmt("index: archive requires migration; run merge before index")
	}

	for _, o := range store.list("pile/") {
		if !strings.HasSuffix(o.key, pileSorted) {
			throwFmt("index: %s requires migration; run merge before index", o.key)
		}

		keys = append(keys, o.key)
	}

	broken := eachSession(store, keys, !allowPartial, func(session string, lines [][]byte) {
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

	for session, f := range findForks(tx) {
		throw2(tx.Exec("UPDATE sessions SET parent = ?, copied = ?, parent_n = ? WHERE session = ?", f.parent, f.copied, f.parentN, session))
	}

	for k, v := range map[string]string{
		"built_at":     nowRFC3339(),
		"last_ts":      lastTS,
		"complete":     strconv.FormatBool(broken == 0),
		"broken_files": itoa(int64(broken)),
		"sessions":     itoa(int64(sessions)),
		"docs":         itoa(int64(docs)),
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

func docKey(d Doc) int64 {
	h := fnv.New64a()

	io.WriteString(h, d.TS+"\x00"+d.Role+"\x00"+d.Body)

	return int64(h.Sum64())
}

func textKey(d Doc) int64 {
	h := fnv.New64a()

	io.WriteString(h, d.Role+"\x00"+d.Body)

	return int64(h.Sum64())
}

type Fork struct {
	parent  string
	copied  int
	parentN int
}

func findForks(tx *sql.Tx) map[string]Fork {
	throw2(tx.Exec("CREATE INDEX keys_key ON keys (key, session, n)"))

	rows := throw2(tx.Query(forkPairs))

	defer rows.Close()

	type Pair struct {
		Fork, parent string
	}

	candidates := map[Pair]Fork{}
	best := map[string]Fork{}

	for rows.Next() {
		var s, t, nextS, nextT string
		var cs, mt int
		var prefix, restamped bool
		throw(rows.Scan(&s, &t, &cs, &mt, &prefix, &nextS, &nextT, &restamped))

		if prefix && cmp.Or(cmp.Compare(nextT, nextS), cmp.Compare(t, s)) > 0 {
			continue
		}

		f := Fork{t, cs, mt}

		if restamped {
			f.copied++
			f.parentN++
		}

		candidates[Pair{s, t}] = f

		if f.copied > best[s].copied {
			best[s] = f
		}
	}

	throw(rows.Err())

	forks := map[string]Fork{}

	for s, f := range best {
		for range len(best) {
			up, ok := best[f.parent]

			if !ok || up.copied < f.copied {
				break
			}

			next, ok := candidates[Pair{s, up.parent}]

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
