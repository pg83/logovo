package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func copyFile(src, dst string) {
	in := throw2(os.Open(src))
	defer in.Close()

	out := throw2(os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644))
	_, cerr := io.Copy(out, in)
	throw(out.Close())
	throw(cerr)
}

func fileSize(path string) int64 {
	return throw2(os.Stat(path)).Size()
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// truncate cuts s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	runes := []rune(s)

	if len(runes) <= n {
		return s
	}

	return string(runes[:n]) + "…"
}

// oneLine squeezes whitespace so a record reads as a single line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
