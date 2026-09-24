package main

import (
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const maxUploadBytes = 16 << 20

func collectMain(args []string) {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	listen := fs.String("listen", "", "address to listen on (a mesh address of this host)")
	storeSpec := fs.String("store", "", "dir:/path or s3://bucket")
	throw(fs.Parse(args))

	if *listen == "" || *storeSpec == "" {
		throwFmt("collect: -listen and -store are required")
	}

	c := &collector{store: openStore(*storeSpec)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/portions", c.handlePortion)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	slog.Info("collect: listening", "addr", *listen)
	throw(http.ListenAndServe(*listen, mux))
}

type collector struct {
	store objectStore
}

func queueKey(session, sum string) string {
	return "queue/" + session + "." + sum
}

// handlePortion checks the frame, then stores it under a key made of the
// session and the md5 of the line inside: the same portion twice lands
// on the same key, so a retry after a lost reply is harmless.
func (c *collector) handlePortion(w http.ResponseWriter, r *http.Request) {
	try(func() {
		frame := throw2(io.ReadAll(http.MaxBytesReader(w, r.Body, maxUploadBytes)))
		lines := decodeFrames(frame)

		if len(lines) != 1 {
			throwFmt("want exactly one portion per upload, got %d", len(lines))
		}

		p := parsePortion(lines[0])
		sum := portionSum(p)

		if want := r.Header.Get("X-Logovo-Md5"); want != "" && !strings.EqualFold(want, sum) {
			throwFmt("md5 mismatch: header %s, body %s", want, sum)
		}

		key := queueKey(p.Session, sum)
		c.store.put(key, frame)

		slog.Info("collect: stored", "key", key, "host", p.Host, "agent", p.Agent, "records", len(p.Records), "bytes", len(frame))

		w.Header().Set("Content-Type", "application/json")
		throw(json.NewEncoder(w).Encode(map[string]any{"key": key}))
	}).catch(func(exc *Exception) {
		slog.Warn("collect: rejected", "from", r.RemoteAddr, "err", exc.Error())
		http.Error(w, exc.Error(), http.StatusBadRequest)
	})
}
