package main

import (
	"log/slog"
	"os"
)

// version is the commit the binary was built from, set with
// -ldflags "-X main.version=<sha>"; empty in local builds.
var version string

func printUsage() {
	os.Stderr.WriteString(`Usage: logovo command [flags]

Client:
  scan [-url URL] [-root agent=dir ...] [-interval 60s] [-once] [-host NAME]
        ship new session log portions to the collector; without -once, when
        web runs another version, replaces its own binary with web's build
        from /v1/client/ and exits for its service manager to restart it
  search [-api URL] [-n 20] [-json] query...   full text search over sessions
  show [-api URL] session [-from N] [-to N]     print a session transcript

Lab:
  collect [-listen ADDR] -store STORE     accept portions, write them to queue/
  merge -store STORE                      append queue/ portions to sessions/
  index -store STORE [-out FILE]          rebuild index/ from every session
  serve [-listen ADDR] -store STORE       search API over the latest index
  web [-listen ADDR] [-api URL] [-collect URL] [-clients DIR]
        the page; proxies /v1/portions to collect and the rest of /v1/ to serve;
        hands scan its binaries from DIR at /v1/client/

STORE is dir:/path or s3://bucket (S3_ENDPOINT, AWS_ACCESS_KEY_ID,
AWS_SECRET_ACCESS_KEY, AWS_REGION from the environment).
`)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	args := os.Args[2:]

	try(func() {
		switch os.Args[1] {
		case "scan":
			scanMain(args)
		case "collect":
			collectMain(args)
		case "merge":
			mergeMain(args)
		case "index":
			indexMain(args)
		case "serve":
			serveMain(args)
		case "search":
			searchMain(args)
		case "show":
			showMain(args)
		case "web":
			webMain(args)
		default:
			printUsage()
			os.Exit(1)
		}
	}).catch(func(exc *Exception) {
		slog.Error(exc.Error())
		os.Exit(1)
	})
}
