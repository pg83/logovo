package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCollectURL = "https://logovo.lab.mesh/v1/portions"
	maxPortionBytes   = 4 << 20
)

// A version web hands out is a commit sha; anything else, such as a
// captive portal's login page answering in its place, is not an update.
var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

type scanRoot struct {
	agent string
	dir   string
}

type rootFlags []scanRoot

func (r *rootFlags) String() string {
	var parts []string

	for _, x := range *r {
		parts = append(parts, x.agent+"="+x.dir)
	}

	return strings.Join(parts, ",")
}

func (r *rootFlags) Set(v string) error {
	agent, dir, ok := strings.Cut(v, "=")

	if !ok || !agents[agent] || dir == "" {
		return errors.New("want -root claude=/dir or -root codex=/dir")
	}

	*r = append(*r, scanRoot{agent: agent, dir: dir})

	return nil
}

// defaultRoots are where the agents keep their sessions; scan needs no
// configuration on a host that runs them in the usual places.
func defaultRoots() []scanRoot {
	home := throw2(os.UserHomeDir())

	return []scanRoot{
		{agent: "claude", dir: filepath.Join(home, ".claude", "projects")},
		{agent: "codex", dir: filepath.Join(home, ".codex", "sessions")},
	}
}

type scanner struct {
	url    string
	host   string
	user   string
	roots  []scanRoot
	client *http.Client
}

func scanMain(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	url := fs.String("url", defaultCollectURL, "collector URL")
	host := fs.String("host", "", "host name to report (default: this host's name)")
	interval := fs.Duration("interval", time.Minute, "how often to look for new records")
	once := fs.Bool("once", false, "one pass, then exit")
	var roots rootFlags
	fs.Var(&roots, "root", "agent=dir to scan instead of the defaults; repeatable")
	throw(fs.Parse(args))

	if len(roots) == 0 {
		roots = defaultRoots()
	}

	if *host == "" {
		*host = throw2(os.Hostname())
	}

	me := "?"

	if u, err := user.Current(); err == nil {
		me = u.Username
	}

	s := &scanner{
		url:    *url,
		host:   *host,
		user:   me,
		roots:  roots,
		client: &http.Client{Timeout: 2 * time.Minute},
	}

	for {
		if !*once {
			s.update()
		}

		s.pass()

		if *once {
			return
		}

		time.Sleep(*interval)
	}
}

// update swaps the running binary for the one web hands out when web runs
// another version, then exits so launchd or systemd starts the new one.
// A non-200 answer means a server without /v1/client (a bare collector,
// an older web) and is skipped quietly; any other failure is logged and
// scanning goes on with the binary it has.
func (s *scanner) update() {
	try(func() {
		base := strings.TrimSuffix(s.url, "/v1/portions")
		resp := s.getOK(base + "/v1/client/version")

		if resp == nil {
			return
		}

		defer resp.Body.Close()

		want := strings.TrimSpace(string(throw2(io.ReadAll(io.LimitReader(resp.Body, 4096)))))

		if !commitRe.MatchString(want) || want == version {
			return
		}

		bin := s.getOK(base + "/v1/client/" + runtime.GOOS + "-" + runtime.GOARCH)

		if bin == nil {
			return
		}

		defer bin.Body.Close()

		// On the nodes the binary is reached through a symlink; the file it
		// points at is the one to replace.
		exe := throw2(filepath.EvalSymlinks(throw2(os.Executable())))
		installBinary(exe, bin.Body, bin.ContentLength)

		slog.Info("scan: updated, restarting", "from", version, "to", want, "path", exe)
		os.Exit(0)
	}).catch(func(exc *Exception) {
		slog.Warn("scan: update failed", "err", exc.Error())
	})
}

// getOK GETs u and returns the response when it is a 200, nil otherwise.
func (s *scanner) getOK(u string) *http.Response {
	resp := throw2(s.client.Get(u))

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()

		return nil
	}

	return resp
}

// installBinary writes body next to exe and renames it over exe: the same
// directory keeps the rename on one file system, so the swap is atomic and
// the running process keeps its old inode.
func installBinary(exe string, body io.Reader, size int64) {
	tmp := exe + ".update"
	f := throw2(os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755))

	try(func() {
		n, cerr := io.Copy(f, body)
		throw(f.Sync())
		throw(f.Close())
		throw(cerr)
		throw(os.Chmod(tmp, 0755))

		if n != size {
			throwFmt("got %d bytes, server sent %d", n, size)
		}

		throw(os.Rename(tmp, exe))
	}).catch(func(exc *Exception) {
		os.Remove(tmp)
		exc.throw()
	})
}

// pass walks every root once. A file that fails to ship is left where it
// is; the next pass retries it, .latest only moves after a 2xx.
func (s *scanner) pass() {
	for _, root := range s.roots {
		for _, path := range sessionFiles(root) {
			try(func() {
				s.ship(root.agent, path)
			}).catch(func(exc *Exception) {
				slog.Warn("scan: skip", "path", path, "err", exc.Error())
			})
		}
	}
}

// sessionFiles lists the jsonl files whose name carries a session uuid:
// <uuid>.jsonl for Claude, rollout-<stamp>-<uuid>.jsonl for Codex.
func sessionFiles(root scanRoot) []string {
	var files []string

	err := filepath.WalkDir(root.dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if entry.IsDir() {
			// Claude keeps per-session scratch (tool results, subagents)
			// in a directory named after the session; not session logs.
			if p != root.dir && root.agent == "claude" && uuidRe.MatchString(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if sessionID(p) != "" {
			files = append(files, p)
		}

		return nil
	})

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		throw(err)
	}

	return files
}

func sessionID(path string) string {
	name := filepath.Base(path)

	if !strings.HasSuffix(name, ".jsonl") {
		return ""
	}

	name = strings.TrimSuffix(name, ".jsonl")

	if len(name) < 36 {
		return ""
	}

	id := name[len(name)-36:]

	if !uuidRe.MatchString(id) {
		return ""
	}

	return id
}

func latestPath(path string) string {
	return path + ".latest"
}

func readLatest(path string) int64 {
	data, err := os.ReadFile(latestPath(path))

	if err != nil {
		return 0
	}

	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)

	if err != nil || n < 0 {
		return 0
	}

	return n
}

func writeLatest(path string, n int64) {
	tmp := latestPath(path) + ".tmp"
	throw(os.WriteFile(tmp, []byte(itoa(n)+"\n"), 0644))
	throw(os.Rename(tmp, latestPath(path)))
}

func (s *scanner) ship(agent, path string) {
	info := throw2(os.Stat(path))
	offset := readLatest(path)

	if info.Size() < offset {
		slog.Info("scan: file shrank, starting over", "path", path)
		offset = 0
	}

	if info.Size() == offset {
		return
	}

	f := throw2(os.Open(path))
	defer f.Close()

	throw2(f.Seek(offset, io.SeekStart))
	data := throw2(io.ReadAll(f))
	cut := bytes.LastIndexByte(data, '\n')

	if cut < 0 {
		return
	}

	data = data[:cut+1]

	for len(data) > 0 {
		chunk := data

		if len(chunk) > maxPortionBytes {
			end := bytes.LastIndexByte(chunk[:maxPortionBytes], '\n')

			if end < 0 {
				end = bytes.IndexByte(chunk, '\n')
			}

			chunk = chunk[:end+1]
		}

		var records []string

		for _, line := range bytes.Split(bytes.TrimSuffix(chunk, []byte{'\n'}), []byte{'\n'}) {
			records = append(records, string(line))
		}

		s.send(&Portion{
			V:       portionVersion,
			Host:    s.host,
			User:    s.user,
			Agent:   agent,
			Session: sessionID(path),
			Path:    path,
			Offset:  offset,
			SentAt:  nowRFC3339(),
			Records: records,
		})

		offset += int64(len(chunk))
		writeLatest(path, offset)
		data = data[len(chunk):]
	}
}

func (s *scanner) send(p *Portion) {
	p.validate()
	_, sum, frame := encodePortion(p)

	req := throw2(http.NewRequest(http.MethodPost, s.url, bytes.NewReader(frame)))
	req.Header.Set("Content-Type", "application/zstd")
	req.Header.Set("X-Logovo-Md5", sum)
	resp := throw2(s.client.Do(req))
	defer resp.Body.Close()

	body := throw2(io.ReadAll(io.LimitReader(resp.Body, 4096)))

	if resp.StatusCode/100 != 2 {
		throwFmt("collector: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	slog.Info("scan: shipped", "agent", p.Agent, "session", p.Session, "offset", p.Offset, "records", len(p.Records), "bytes", len(frame))
}
