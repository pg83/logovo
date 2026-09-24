package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
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

const pileUnit = 1 << 20

func pileLevel(size int64) int {
	return bits.Len64(uint64(size / pileUnit))
}

func pileKey(level int) string {
	return fmt.Sprintf("pile/%02d-%d", level, time.Now().UnixNano())
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

// repack streams every input through one zstd encoder: the same jsonl
// lines, one frame, one object.
func repack(store objectStore, keys []string) []byte {
	var buf bytes.Buffer
	enc := throw2(zstd.NewWriter(&buf))

	for _, key := range keys {
		dec := throw2(zstd.NewReader(bytes.NewReader(store.get(key))))
		throw2(io.Copy(enc, dec))
		dec.Close()
	}

	throw(enc.Close())

	return buf.Bytes()
}

func merge(store objectStore) {
	// Legacy sessions/ objects are folded in the same way as the queue.
	var inputs []string

	for _, o := range append(store.list("queue/"), store.list("sessions/")...) {
		inputs = append(inputs, o.key)
	}

	if len(inputs) > 0 {
		data := repack(store, inputs)
		key := pileKey(pileLevel(int64(len(data))))
		store.put(key, data)

		for _, k := range inputs {
			store.del(k)
		}

		slog.Info("merge: folded", "inputs", len(inputs), "into", key, "bytes", len(data))
	}

	for {
		byLevel := map[int][]object{}

		for _, o := range store.list("pile/") {
			if _, ok := parsePileLevel(o.key); !ok {
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
		data := repack(store, pair)
		key := pileKey(pileLevel(int64(len(data))))
		store.put(key, data)

		for _, k := range pair {
			store.del(k)
		}

		slog.Info("merge: compacted", "level", level, "into", key, "bytes", len(data))
	}
}
