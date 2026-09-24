package main

import (
	_ "embed"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

//go:embed web/index.html
var webIndex []byte

// web is the search page; the API calls it makes go straight through to
// serve, so one name covers both the page and the CLI.
func webMain(args []string) {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8072", "address to listen on")
	api := fs.String("api", "http://127.0.0.1:8071", "serve URL to proxy /v1/ to")
	throw(fs.Parse(args))

	target := throw2(url.Parse(*api))
	proxy := httputil.NewSingleHostReverseProxy(target)

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webIndex)
	})

	slog.Info("web: listening", "addr", *listen, "api", *api)
	throw(http.ListenAndServe(*listen, mux))
}
