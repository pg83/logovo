package main

import (
	"strconv"
	"strings"
	"time"
)

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
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
