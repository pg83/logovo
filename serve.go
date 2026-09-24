package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	defaultLimit = 20
	maxLimit     = 200
)

type server struct {
	store   objectStore
	dir     string
	mu      sync.RWMutex
	db      *sql.DB
	path    string
	version string
}

func serveMain(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8071", "address to listen on")
	storeSpec := fs.String("store", "", "dir:/path or s3://bucket the index is fetched from")
	local := fs.String("index", "", "serve this sqlite file instead of fetching from the store")
	dir := fs.String("dir", "", "where fetched indexes are kept (default: a temp dir)")
	refresh := fs.Duration("refresh", time.Minute, "how often to look for a new index")
	throw(fs.Parse(args))

	s := &server{dir: *dir}

	if *local != "" {
		s.open(*local, "local")
	} else {
		if *storeSpec == "" {
			throwFmt("serve: -store or -index is required")
		}

		s.store = openStore(*storeSpec)

		if s.dir == "" {
			s.dir = throw2(os.MkdirTemp("", "logovo-serve-"))
		}

		throw(os.MkdirAll(s.dir, 0755))
		s.refresh()

		go func() {
			for {
				time.Sleep(*refresh)

				try(s.refresh).catch(func(exc *Exception) {
					slog.Warn("serve: refresh failed", "err", exc.Error())
				})
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleSession)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	slog.Info("serve: listening", "addr", *listen)
	throw(http.ListenAndServe(*listen, mux))
}

// refresh swaps in the index from the store when its version changed.
func (s *server) refresh() {
	version, ok := s.store.stat(indexKey)

	if !ok {
		if s.db == nil {
			slog.Warn("serve: no index in the store yet")
		}

		return
	}

	if version == s.version {
		return
	}

	dec := throw2(zstd.NewReader(bytes.NewReader(s.store.get(indexKey))))
	defer dec.Close()

	path := filepath.Join(s.dir, "index-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".sqlite")
	f := throw2(os.Create(path))
	_, cerr := io.Copy(f, dec)
	throw(f.Close())
	throw(cerr)
	s.open(path, version)
}

func (s *server) open(path, version string) {
	db := throw2(sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1"))
	var docs string
	throw(db.QueryRow("SELECT value FROM meta WHERE key = 'docs'").Scan(&docs))

	s.mu.Lock()
	old, oldPath := s.db, s.path
	s.db, s.path, s.version = db, path, version
	s.mu.Unlock()

	if old != nil {
		old.Close()

		if oldPath != path && s.store != nil {
			os.Remove(oldPath)
		}
	}

	slog.Info("serve: index loaded", "path", path, "version", version, "docs", docs)
}

func (s *server) current() *sql.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.db == nil {
		throwFmt("no index loaded yet")
	}

	return s.db
}

type hit struct {
	Session string  `json:"session"`
	N       int     `json:"n"`
	Role    string  `json:"role"`
	TS      string  `json:"ts"`
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
	Agent   string  `json:"agent"`
	Host    string  `json:"host"`
	Cwd     string  `json:"cwd"`
	Title   string  `json:"title"`
}

// quoteQuery turns free text into an FTS5 query of quoted words, the
// fallback when the raw query is not valid FTS5 syntax.
func quoteQuery(q string) string {
	var parts []string

	for _, w := range strings.Fields(q) {
		parts = append(parts, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}

	return strings.Join(parts, " ")
}

func (s *server) search(q string, limit int) []hit {
	db := s.current()
	const query = `
SELECT h.session, h.n, h.role, h.ts, h.score, h.snip, s.agent, s.host, s.cwd, s.title
FROM (
    SELECT session, n, role, ts, bm25(docs) AS score, snippet(docs, 0, '[', ']', '…', 24) AS snip
    FROM docs WHERE docs MATCH ? ORDER BY bm25(docs) LIMIT ?
) h JOIN sessions s ON s.session = h.session
ORDER BY h.score`

	rows, err := db.Query(query, q, limit)

	if err != nil {
		rows = throw2(db.Query(query, quoteQuery(q), limit))
	}

	defer rows.Close()
	hits := []hit{}

	for rows.Next() {
		var h hit
		throw(rows.Scan(&h.Session, &h.N, &h.Role, &h.TS, &h.Score, &h.Snippet, &h.Agent, &h.Host, &h.Cwd, &h.Title))
		hits = append(hits, h)
	}

	throw(rows.Err())

	return hits
}

func shortTS(ts string) string {
	if len(ts) >= 16 {
		return strings.Replace(ts[:16], "T", " ", 1)
	}

	return ts
}

func (h hit) text() string {
	return fmt.Sprintf("%s %s %s %s [%d] %s: %s\n    %s: %s\n",
		h.Session, h.Agent, h.Host, shortTS(h.TS), h.N, h.Title, filepath.Base(h.Cwd), h.Role, oneLine(h.Snippet))
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	try(func() {
		q := strings.TrimSpace(r.URL.Query().Get("q"))

		if q == "" {
			throwFmt("q is required")
		}

		limit := defaultLimit

		if v := r.URL.Query().Get("limit"); v != "" {
			limit = throw2(strconv.Atoi(v))
		}

		if limit < 1 || limit > maxLimit {
			limit = defaultLimit
		}

		hits := s.search(q, limit)

		if r.URL.Query().Get("format") == "json" {
			w.Header().Set("Content-Type", "application/json")
			throw(json.NewEncoder(w).Encode(map[string]any{"hits": hits}))

			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		for _, h := range hits {
			w.Write([]byte(h.text()))
		}
	}).catch(func(exc *Exception) {
		http.Error(w, exc.Error(), http.StatusBadRequest)
	})
}

func (s *server) session(id string, from, to int) (sessionInfo, []doc) {
	db := s.current()
	var info sessionInfo
	err := db.QueryRow("SELECT session, agent, host, user, cwd, title, first_ts, last_ts, turns, bytes FROM sessions WHERE session = ?", id).
		Scan(&info.Session, &info.Agent, &info.Host, &info.User, &info.Cwd, &info.Title, &info.FirstTS, &info.LastTS, &info.Turns, &info.Bytes)

	if err == sql.ErrNoRows {
		throwFmt("no such session %s", id)
	}

	throw(err)

	rows := throw2(db.Query("SELECT n, role, ts, body FROM docs WHERE session = ? AND n BETWEEN ? AND ? ORDER BY n", id, from, to))
	defer rows.Close()
	docs := []doc{}

	for rows.Next() {
		var d doc
		throw(rows.Scan(&d.N, &d.Role, &d.TS, &d.Body))
		docs = append(docs, d)
	}

	throw(rows.Err())

	return info, docs
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	try(func() {
		id := r.PathValue("id")

		if !uuidRe.MatchString(id) {
			throwFmt("bad session id")
		}

		from, to := 0, 1<<30

		if v := r.URL.Query().Get("from"); v != "" {
			from = throw2(strconv.Atoi(v))
		}

		if v := r.URL.Query().Get("to"); v != "" {
			to = throw2(strconv.Atoi(v))
		}

		info, docs := s.session(id, from, to)

		if r.URL.Query().Get("format") == "json" {
			w.Header().Set("Content-Type", "application/json")
			throw(json.NewEncoder(w).Encode(map[string]any{"session": info, "docs": docs}))

			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "# %s %s %s@%s %s\n# %s .. %s, %d turns\n# %s\n\n",
			info.Session, info.Agent, info.User, info.Host, info.Cwd, shortTS(info.FirstTS), shortTS(info.LastTS), info.Turns, info.Title)

		for _, d := range docs {
			fmt.Fprintf(w, "[%d] %s %s:\n%s\n\n", d.N, shortTS(d.TS), d.Role, d.Body)
		}
	}).catch(func(exc *Exception) {
		status := http.StatusBadRequest

		if strings.HasPrefix(exc.Error(), "no such session") {
			status = http.StatusNotFound
		}

		http.Error(w, exc.Error(), status)
	})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	try(func() {
		db := s.current()
		rows := throw2(db.Query("SELECT key, value FROM meta"))
		defer rows.Close()
		meta := map[string]string{}

		for rows.Next() {
			var k, v string
			throw(rows.Scan(&k, &v))
			meta[k] = v
		}

		s.mu.RLock()
		meta["version"] = s.version
		s.mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		throw(json.NewEncoder(w).Encode(meta))
	}).catch(func(exc *Exception) {
		http.Error(w, exc.Error(), http.StatusServiceUnavailable)
	})
}
