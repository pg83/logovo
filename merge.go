package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	pileUnit   = 1 << 20
	pileSorted = ".sorted"
)

func pileLevel(size int64) int {
	return bits.Len64(uint64(size / pileUnit))
}

func pileKey(level int) string {
	return fmt.Sprintf("pile/%02d-%d%s", level, time.Now().UnixNano(), pileSorted)
}

func parsePileLevel(key string) (int, bool) {
	name := strings.TrimPrefix(key, "pile/")

	var level int
	var seq int64

	if _, err := fmt.Sscanf(name, "%02d-%d", &level, &seq); err != nil {
		return 0, false
	}

	return level, true
}

func mergeMain(args []string) {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	storeSpec := fs.String("store", "", "dir:/path or s3://bucket")

	throw(fs.Parse(args))

	if *storeSpec == "" {
		throwFmt("merge: -store is required")
	}

	merge(openStore(*storeSpec))
}

func repack(store ObjectStore, fill func(w io.Writer)) (string, int64) {
	f := throw2(os.CreateTemp(".", "logovo-merge-"))

	defer os.Remove(f.Name())

	enc := throw2(zstd.NewWriter(f))

	fill(enc)
	throw(enc.Close())
	throw(f.Close())

	size := fileSize(f.Name())
	key := pileKey(pileLevel(size))

	store.putFile(key, f.Name())

	return key, size
}

func eachSession(store ObjectStore, keys []string, strict bool, cb func(session string, lines [][]byte)) int {
	readers := make([]*LineReader, len(keys))
	broken := 0
	heads := make([][]byte, len(keys))
	sessions := make([]string, len(keys))

	for i, key := range keys {
		readers[i] = openLines(store, key)

		defer readers[i].close()
	}

	read := func(i int) []byte {
		if strict {
			return readers[i].next()
		}

		var line []byte

		try(func() { line = readers[i].next() }).catch(func(exc *Exception) {
			broken++
			slog.Error("pile file broken, read up to the break", "key", keys[i], "err", exc.Error())
		})

		return line
	}

	advance := func(i int) {
		for line := read(i); line != nil; line = read(i) {
			session := portionSession(line)

			if session == "" {
				continue
			}

			if session < sessions[i] {
				throwFmt("%s is not sorted by session", keys[i])
			}

			heads[i], sessions[i] = line, session

			return
		}

		heads[i] = nil
	}

	for i := range keys {
		advance(i)
	}

	for {
		session := ""

		for i := range keys {
			if heads[i] != nil && (session == "" || sessions[i] < session) {
				session = sessions[i]
			}
		}

		if session == "" {
			return broken
		}

		var lines [][]byte

		for i := range keys {
			for heads[i] != nil && sessions[i] == session {
				lines = append(lines, heads[i])
				advance(i)
			}
		}

		cb(session, lines)
	}
}

func unpack(store ObjectStore, key string) {
	r := openLines(store, key)

	defer r.close()

	enc := throw2(zstd.NewWriter(nil))

	defer enc.Close()

	for line := r.next(); line != nil; line = r.next() {
		var p *Portion

		if try(func() { p = parsePortion(line) }) != nil {
			continue
		}

		store.put(queueKey(p.Session, portionSum(p)), enc.EncodeAll(line, nil))
	}

	store.del(key)
}

func merge(store ObjectStore) {
	for _, o := range append(store.list("pile/"), store.list("sessions/")...) {
		if !strings.HasSuffix(o.key, pileSorted) {
			unpack(store, o.key)
		}
	}

	var inputs []string

	for _, o := range store.list("queue/") {
		inputs = append(inputs, o.key)
	}

	if len(inputs) > 0 {
		key, size := repack(store, func(w io.Writer) {
			for _, k := range inputs {
				raw := store.get(k)

				var lines [][]byte

				exc := try(func() { lines = decodeFrames(raw) })

				if exc == nil && len(lines) == 0 {
					exc = exceptionf("no lines")
				}

				if exc != nil {
					slog.Error("merge: bad queue object, kept in bad/", "key", k, "err", exc.Error())
					store.put("bad/"+k, raw)

					continue
				}

				for _, line := range lines {
					throw2(w.Write(line))
				}
			}
		})

		for _, k := range inputs {
			store.del(k)
		}

		slog.Info("merge: folded", "inputs", len(inputs), "into", key, "bytes", size)
	}

	for {
		byLevel := map[int][]Object{}

		for _, o := range store.list("pile/") {
			if _, ok := parsePileLevel(o.key); !ok || !strings.HasSuffix(o.key, pileSorted) {
				continue
			}

			level := pileLevel(o.size)

			byLevel[level] = append(byLevel[level], o)
		}

		level := -1

		for l, files := range byLevel {
			if len(files) >= 2 && (level < 0 || l < level) {
				level = l
			}
		}

		if level < 0 {
			break
		}

		files := byLevel[level]

		sort.Slice(files, func(i, j int) bool { return files[i].key < files[j].key })

		pair := []string{files[0].key, files[1].key}

		key, size := repack(store, func(w io.Writer) {
			eachSession(store, pair, true, func(session string, lines [][]byte) {
				for _, line := range lines {
					throw2(w.Write(line))
				}
			})
		})

		for _, k := range pair {
			store.del(k)
		}

		slog.Info("merge: compacted", "level", level, "into", key, "bytes", size)
	}
}
