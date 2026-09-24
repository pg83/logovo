package main

import (
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultAPI = "https://logovo.lab.mesh"

func apiBase(flagValue string) string {
	if flagValue != "" {
		return strings.TrimRight(flagValue, "/")
	}

	if v := os.Getenv("LOGOVO_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}

	return defaultAPI
}

func fetchText(u string) {
	client := &http.Client{Timeout: time.Minute}
	resp := throw2(client.Get(u))
	defer resp.Body.Close()

	body := throw2(io.ReadAll(resp.Body))

	if resp.StatusCode != http.StatusOK {
		throwFmt("%s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	os.Stdout.Write(body)
}

func searchMain(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	api := fs.String("api", "", "logovo API base URL (default $LOGOVO_URL or "+defaultAPI+")")
	n := fs.Int("n", defaultLimit, "how many hits")
	asJSON := fs.Bool("json", false, "JSON instead of text")
	throw(fs.Parse(args))

	if fs.NArg() == 0 {
		throwFmt("search: query is required")
	}

	q := url.Values{"q": {strings.Join(fs.Args(), " ")}, "limit": {strconv.Itoa(*n)}}

	if *asJSON {
		q.Set("format", "json")
	}

	fetchText(apiBase(*api) + "/v1/search?" + q.Encode())
}

func showMain(args []string) {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	api := fs.String("api", "", "logovo API base URL (default $LOGOVO_URL or "+defaultAPI+")")
	from := fs.Int("from", 0, "first turn")
	to := fs.Int("to", 1<<30, "last turn")
	asJSON := fs.Bool("json", false, "JSON instead of text")
	throw(fs.Parse(args))

	// Flags may follow the session id: `show <uuid> -from 10 -to 30`.
	if fs.NArg() > 1 {
		rest := fs.Args()[1:]
		id := fs.Arg(0)
		throw(fs.Parse(rest))
		args = append([]string{id}, fs.Args()...)
	} else {
		args = fs.Args()
	}

	if len(args) != 1 || !uuidRe.MatchString(args[0]) {
		throwFmt("show: one session uuid is required")
	}

	q := url.Values{"from": {strconv.Itoa(*from)}, "to": {strconv.Itoa(*to)}}

	if *asJSON {
		q.Set("format", "json")
	}

	fetchText(apiBase(*api) + "/v1/sessions/" + args[0] + "?" + q.Encode())
}
