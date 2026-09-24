package main

import (
	"flag"
	"log/slog"
	"strings"
)

func sessionKey(session string) string {
	return "sessions/" + session
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

// merge appends every queued portion to its session, in the order the
// keys list, and drops it from the queue. A crash between the append
// and the delete repeats one portion; the indexer drops duplicates by
// their md5, so this needs no bookkeeping of its own.
func merge(store objectStore) {
	keys := store.list("queue/")
	merged := 0

	for _, key := range keys {
		name := strings.TrimPrefix(key, "queue/")
		session, _, ok := strings.Cut(name, ".")

		if !ok || !uuidRe.MatchString(session) {
			slog.Warn("merge: odd key, skipping", "key", key)

			continue
		}

		store.appendTo(sessionKey(session), store.get(key))
		store.del(key)
		merged++
	}

	slog.Info("merge: done", "portions", merged)
}
