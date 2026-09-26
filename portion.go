package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"

	"github.com/klauspost/compress/zstd"
)

// A portion is what scan ships and what collect stores: one JSON object
// per line with the raw session records inside. Sessions are just these
// lines concatenated, each still in its own zstd frame; nothing on the
// lab side needs to look inside until the indexer does.
type Portion struct {
	V       int      `json:"v"`
	Host    string   `json:"host"`
	User    string   `json:"user"`
	Agent   string   `json:"agent"`
	Session string   `json:"session"`
	Path    string   `json:"path"`
	Offset  int64    `json:"offset"`
	SentAt  string   `json:"sent_at"`
	Records []string `json:"records"`
}

const portionVersion = 1

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var agents = map[string]bool{"claude": true, "codex": true}

func (p *Portion) validate() {
	if p.V != portionVersion {
		throwFmt("portion: unsupported version %d", p.V)
	}

	if !uuidRe.MatchString(p.Session) {
		throwFmt("portion: bad session id %q", p.Session)
	}

	if !agents[p.Agent] {
		throwFmt("portion: unknown agent %q", p.Agent)
	}

	if len(p.Records) == 0 {
		throwFmt("portion: no records")
	}
}

// portionSum identifies a portion by what it carries, not by when it was
// sent: the same bytes shipped twice get the same sum, hence the same
// queue key, and the indexer drops the second copy.
func portionSum(p *Portion) string {
	q := *p
	q.SentAt = ""

	return md5Hex(throw2(json.Marshal(&q)))
}

// encodePortion returns the line (JSON plus newline), its sum and the
// zstd frame that travels and is stored.
func encodePortion(p *Portion) (line []byte, sum string, frame []byte) {
	line = append(throw2(json.Marshal(p)), '\n')
	sum = portionSum(p)

	var buf bytes.Buffer
	enc := throw2(zstd.NewWriter(&buf))
	throw2(enc.Write(line))
	throw(enc.Close())

	return line, sum, buf.Bytes()
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)

	return hex.EncodeToString(sum[:])
}

// decodeFrames inflates a stream of zstd frames and returns the lines,
// one portion each.
func decodeFrames(data []byte) [][]byte {
	dec := throw2(zstd.NewReader(bytes.NewReader(data)))
	defer dec.Close()

	raw := throw2(io.ReadAll(dec))
	var lines [][]byte

	for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}

		lines = append(lines, line)
	}

	return lines
}

// lineReader pulls the lines of a stored stream of zstd frames one at a
// time: the lines decodeFrames returns, without holding them all.
type lineReader struct {
	body io.ReadCloser
	dec  *zstd.Decoder
	br   *bufio.Reader
}

func openLines(store objectStore, key string) *lineReader {
	body := store.open(key)
	dec := throw2(zstd.NewReader(body))

	return &lineReader{body: body, dec: dec, br: bufio.NewReader(dec)}
}

// next returns the next line, nil at the end.
func (r *lineReader) next() []byte {
	line, err := r.br.ReadBytes('\n')

	if err != nil && err != io.EOF {
		throw(err)
	}

	if len(line) == 0 {
		return nil
	}

	return line
}

func (r *lineReader) close() {
	r.dec.Close()
	r.body.Close()
}

func parsePortion(line []byte) *Portion {
	var p Portion
	throw(json.Unmarshal(line, &p))
	p.validate()

	return &p
}

// portionSession peeks at the session id without validating the rest;
// a line that is not a portion at all yields "".
func portionSession(line []byte) string {
	var p struct {
		Session string `json:"session"`
	}

	if json.Unmarshal(line, &p) != nil || !uuidRe.MatchString(p.Session) {
		return ""
	}

	return p.Session
}
