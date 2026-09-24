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

// merge appends the queued portions to their sessions, all of a
// session's portions in one append in the order the keys list, and
// drops them from the queue. A crash between the append and the delete
// repeats portions; the indexer drops duplicates by their md5, so this
// needs no bookkeeping of its own.
func merge(store objectStore) {
	keys := store.list("queue/")
	var order []string
	bySession := map[string][]string{}

	for _, key := range keys {
		name := strings.TrimPrefix(key, "queue/")
		session, _, ok := strings.Cut(name, ".")

		if !ok || !uuidRe.MatchString(session) {
			slog.Warn("merge: odd key, skipping", "key", key)

			continue
		}

		if _, seen := bySession[session]; !seen {
			order = append(order, session)
		}

		bySession[session] = append(bySession[session], key)
	}

	merged := 0

	for _, session := range order {
		var frames []byte

		for _, key := range bySession[session] {
			frames = append(frames, store.get(key)...)
		}

		store.appendTo(sessionKey(session), frames)

		for _, key := range bySession[session] {
			store.del(key)
		}

		merged += len(bySession[session])
	}

	slog.Info("merge: done", "portions", merged, "sessions", len(order))
}
