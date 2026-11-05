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

// Sessions live in pile/: a handful of files whose sizes climb by powers
// of two, each one zstd-compressed jsonl of portions from any session.
// merge folds the queue into a new smallest file and then, while two
// files share a size class, repacks them into one of the next class, so
// the pile stays at about log2(total) files and every byte is rewritten
// only that many times. Nothing here is idempotent on purpose: a crash
// between a write and the deletes leaves duplicate portions, which the
// indexer drops and zstd squeezes.
//
// Every pile file (*.sorted) holds its lines grouped by session, sessions
// in ascending order, so repacking two of them and indexing all of them
// are streaming merges that hold one session at a time.

const pileUnit = 1 << 20

const pileSorted = ".sorted"

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

// repack writes a new pile file: fill streams jsonl lines into one zstd
// encoder, one frame, one object. The object is spooled through a local
// file, never held in memory; it returns the new key and its size.
func repack(store objectStore, fill func(w io.Writer)) (string, int64) {
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

// eachSession reads pile files, each sorted by session, as one stream
// and calls cb for every session in ascending order with all its lines:
// the first file's, then the second's, and so on. Lines that are not
// portions are skipped. A file that cannot be read to its end fails the
// call when strict; otherwise it is logged and counts as ended there.
func eachSession(store objectStore, keys []string, strict bool, cb func(session string, lines [][]byte)) {
	readers := make([]*lineReader, len(keys))
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
			return
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

// unpack sends an object from before the pile was sorted back to the
// queue, one portion per object as collect stores it, and deletes it;
// lines that are not portions are dropped.
func unpack(store objectStore, key string) {
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

func merge(store objectStore) {
	// Pile files from before the pile was sorted and legacy sessions/
	// objects go back to the queue and are folded anew.
	for _, o := range append(store.list("pile/"), store.list("sessions/")...) {
		if !strings.HasSuffix(o.key, pileSorted) {
			unpack(store, o.key)
		}
	}

	// Queue keys are <session>.<md5>, so the queue in key order is
	// already sorted by session.
	var inputs []string

	for _, o := range store.list("queue/") {
		inputs = append(inputs, o.key)
	}

	if len(inputs) > 0 {
		key, size := repack(store, func(w io.Writer) {
			for _, k := range inputs {
				// A queue object is one portion: it is decoded whole before
				// any of it goes into the pile, and a broken one is kept in
				// bad/ instead of stopping the fold for good.
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
		byLevel := map[int][]object{}

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
