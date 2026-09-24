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

// web is the one thing behind the logovo name: the search page, the
// search API proxied to serve, and uploads proxied to collect. lab_proxy
// routes by host name to a single port, so this is where paths fan out.
func webMain(args []string) {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8072", "address to listen on")
	api := fs.String("api", "http://127.0.0.1:8071", "serve URL to proxy /v1/ to")
	collect := fs.String("collect", "http://127.0.0.1:8070", "collect URL to proxy /v1/portions to")
	throw(fs.Parse(args))

	mux := http.NewServeMux()
	mux.Handle("POST /v1/portions", httputil.NewSingleHostReverseProxy(throw2(url.Parse(*collect))))
	mux.Handle("/v1/", httputil.NewSingleHostReverseProxy(throw2(url.Parse(*api))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webIndex)
	})

	slog.Info("web: listening", "addr", *listen, "api", *api, "collect", *collect)
	throw(http.ListenAndServe(*listen, mux))
}
