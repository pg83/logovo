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

var (
	uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	agents = map[string]bool{"claude": true, "codex": true}
)

const portionVersion = 1

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

func portionSum(p *Portion) string {
	q := *p

	q.SentAt = ""

	return md5Hex(throw2(json.Marshal(&q)))
}

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

type LineReader struct {
	key  string
	body io.ReadCloser
	dec  *zstd.Decoder
	br   *bufio.Reader
}

func openLines(store ObjectStore, key string) *LineReader {
	body := store.open(key)
	dec, err := zstd.NewReader(body)

	if err != nil {
		throwFmt("%s: %v", key, err)
	}

	return &LineReader{key: key, body: body, dec: dec, br: bufio.NewReader(dec)}
}

func (r *LineReader) next() []byte {
	line, err := r.br.ReadBytes('\n')

	if err != nil && err != io.EOF {
		throwFmt("%s: %v", r.key, err)
	}

	if len(line) == 0 {
		return nil
	}

	return line
}

func (r *LineReader) close() {
	r.dec.Close()
	r.body.Close()
}

func parsePortion(line []byte) *Portion {
	var p Portion
	throw(json.Unmarshal(line, &p))
	p.validate()

	return &p
}

func portionSession(line []byte) string {
	var p struct {
		Session string `json:"session"`
	}

	if json.Unmarshal(line, &p) != nil || !uuidRe.MatchString(p.Session) {
		return ""
	}

	return p.Session
}
