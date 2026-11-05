package main

import (
	"encoding/json"
	"sort"
	"strings"
)

const (
	maxArgsChars   = 400
	maxResultChars = 1500
	maxTitleChars  = 80
)

type Doc struct {
	N    int    `json:"n"`
	Role string `json:"role"`
	TS   string `json:"ts"`
	Body string `json:"body"`
}

type SessionInfo struct {
	Session string `json:"session"`
	Agent   string `json:"agent"`
	Host    string `json:"host"`
	User    string `json:"user"`
	Cwd     string `json:"cwd"`
	Title   string `json:"title"`
	FirstTS string `json:"first_ts"`
	LastTS  string `json:"last_ts"`
	Turns   int    `json:"turns"`
	Bytes   int64  `json:"bytes"`

	Parent  string    `json:"parent"`
	Copied  int       `json:"copied"`
	ParentN int       `json:"parent_n"`
	Forks   []ForkRef `json:"forks"`
}

type ForkRef struct {
	Session string `json:"session"`
	Copied  int    `json:"copied"`
}

type Normalized struct {
	info SessionInfo
	docs []Doc
}

func normalize(session string, lines [][]byte) *Normalized {
	seen := map[string]bool{}

	var portions []*Portion
	var bytes int64

	for _, line := range lines {
		exc := try(func() {
			p := parsePortion(line)

			if p.Session != session {
				throwFmt("portion for %s inside session %s", p.Session, session)
			}

			sum := portionSum(p)

			if seen[sum] {
				return
			}

			seen[sum] = true
			portions = append(portions, p)
			bytes += int64(len(line))
		})

		if exc != nil {
			continue
		}
	}

	if len(portions) == 0 {
		return nil
	}

	sort.SliceStable(portions, func(i, j int) bool {
		return portions[i].Offset < portions[j].Offset
	})

	var records []string
	var covered int64

	for _, p := range portions {
		pos := p.Offset

		for _, r := range p.Records {
			if pos >= covered {
				records = append(records, r)
			}

			pos += int64(len(r)) + 1
		}

		if pos > covered {
			covered = pos
		}
	}

	n := &Normalized{info: SessionInfo{
		Session: session,
		Agent:   portions[0].Agent,
		Host:    portions[0].Host,
		User:    portions[0].User,
		Bytes:   bytes,
	}}

	switch n.info.Agent {
	case "claude":
		parseClaude(n, records)
	case "codex":
		parseCodex(n, records)
	}

	for i := range n.docs {
		n.docs[i].N = i
	}

	if len(n.docs) > 0 {
		n.info.FirstTS = n.docs[0].TS
		n.info.LastTS = n.docs[len(n.docs)-1].TS
	}

	n.info.Turns = len(n.docs)

	if n.info.Title == "" {
		for _, d := range n.docs {
			if d.Role == "user" && !commandMarkup(d.Body) {
				n.info.Title = truncate(oneLine(d.Body), maxTitleChars)

				break
			}
		}
	}

	return n
}

func (n *Normalized) add(role, ts, body string) {
	body = strings.TrimSpace(body)

	if body == "" {
		return
	}

	n.docs = append(n.docs, Doc{Role: role, TS: ts, Body: body})
}

func commandMarkup(text string) bool {
	for _, prefix := range []string{"<command-name>", "<command-message>", "<local-command-stdout>", "<local-command-stderr>"} {
		if strings.HasPrefix(strings.TrimSpace(text), prefix) {
			return true
		}
	}

	return false
}

func toolCall(name string, input json.RawMessage) string {
	var args map[string]any

	if json.Unmarshal(input, &args) == nil {
		for _, key := range []string{"command", "cmd", "file_path", "path", "pattern", "query", "url"} {
			if v, ok := args[key].(string); ok && v != "" {
				return "▶ " + name + ": " + truncate(oneLine(v), maxArgsChars)
			}
		}
	}

	return "▶ " + name + ": " + truncate(oneLine(string(input)), maxArgsChars)
}

func toolResult(text string) string {
	return "◀ " + truncate(strings.TrimSpace(text), maxResultChars)
}

func blockText(raw json.RawMessage) string {
	var s string

	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	var parts []string

	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}

	return strings.Join(parts, "\n")
}

func parseClaude(n *Normalized, records []string) {
	type Block struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
	}

	type Record struct {
		Type             string `json:"type"`
		Timestamp        string `json:"timestamp"`
		Cwd              string `json:"cwd"`
		IsMeta           bool   `json:"isMeta"`
		IsCompactSummary bool   `json:"isCompactSummary"`
		AiTitle          string `json:"aiTitle"`
		CustomTitle      string `json:"customTitle"`
		Summary          string `json:"summary"`
		Origin           struct {
			Kind string `json:"kind"`
		} `json:"origin"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Attachment struct {
			Type        string          `json:"type"`
			Prompt      json.RawMessage `json:"prompt"`
			CommandMode string          `json:"commandMode"`
			Origin      struct {
				Kind string `json:"kind"`
			} `json:"origin"`
		} `json:"attachment"`
	}

	queued := map[string]bool{}
	customTitle := false

	requeued := func(text string) bool {
		text = strings.TrimSpace(text)

		if !queued[text] {
			return false
		}

		delete(queued, text)

		return true
	}

	for _, line := range records {
		var r Record

		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}

		if n.info.Cwd == "" && r.Cwd != "" {
			n.info.Cwd = r.Cwd
		}

		switch r.Type {
		case "custom-title":
			if r.CustomTitle != "" {
				n.info.Title = truncate(oneLine(r.CustomTitle), maxTitleChars)
				customTitle = true
			}
		case "ai-title":
			if !customTitle && r.AiTitle != "" {
				n.info.Title = truncate(oneLine(r.AiTitle), maxTitleChars)
			}
		case "summary":
			if n.info.Title == "" && r.Summary != "" {
				n.info.Title = truncate(oneLine(r.Summary), maxTitleChars)
			}
		case "attachment":
			a := r.Attachment
			human := a.Origin.Kind == "human" || a.Origin.Kind == "" && a.CommandMode == "prompt"

			if a.Type != "queued_command" || !human {
				continue
			}

			text := blockText(a.Prompt)

			queued[strings.TrimSpace(text)] = true
			n.add("user", r.Timestamp, text)
		case "user", "assistant":
			if r.Type == "assistant" {
				clear(queued)
			}

			if r.IsMeta {
				continue
			}

			if k := r.Origin.Kind; k != "" && k != "human" {
				r.Message.Role = k
			}

			if r.IsCompactSummary {
				r.Message.Role = "summary"
			}

			var s string

			if json.Unmarshal(r.Message.Content, &s) == nil {
				if r.Message.Role != "user" || !requeued(s) {
					n.add(r.Message.Role, r.Timestamp, s)
				}

				continue
			}

			var blocks []Block

			if json.Unmarshal(r.Message.Content, &blocks) != nil {
				continue
			}

			var text, tools []string

			for _, b := range blocks {
				switch b.Type {
				case "text":
					text = append(text, b.Text)
				case "tool_use":
					tools = append(tools, toolCall(b.Name, b.Input))
				case "tool_result":
					tools = append(tools, toolResult(blockText(b.Content)))
				}
			}

			if body := strings.Join(text, "\n"); r.Message.Role != "user" || !requeued(body) {
				n.add(r.Message.Role, r.Timestamp, body)
			}

			for _, t := range tools {
				role := "assistant"

				if strings.HasPrefix(t, "◀") {
					role = "tool"
				}

				n.add(role, r.Timestamp, t)
			}
		}
	}
}

func parseCodex(n *Normalized, records []string) {
	type Item struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
		Output    json.RawMessage `json:"output"`
		Cwd       string          `json:"cwd"`
		Timestamp string          `json:"timestamp"`
		Action    struct {
			Query   string   `json:"query"`
			Command []string `json:"command"`
		} `json:"action"`
	}

	type Record struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   Item   `json:"payload"`
	}

	for _, line := range records {
		var r Record

		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}

		p := r.Payload

		switch r.Type {
		case "session_meta":
			if n.info.Cwd == "" {
				n.info.Cwd = p.Cwd
			}
		case "response_item":
			switch p.Type {
			case "message":
				text := blockText(p.Content)

				if p.Role != "user" && p.Role != "assistant" {
					continue
				}

				if p.Role == "user" && strings.HasPrefix(strings.TrimSpace(text), "<") {
					continue
				}

				n.add(p.Role, r.Timestamp, text)
			case "function_call":
				n.add("assistant", r.Timestamp, toolCall(p.Name, json.RawMessage(p.Arguments)))
			case "local_shell_call":
				if len(p.Action.Command) > 0 {
					n.add("assistant", r.Timestamp, "▶ shell: "+truncate(oneLine(strings.Join(p.Action.Command, " ")), maxArgsChars))
				}
			case "custom_tool_call":
				n.add("assistant", r.Timestamp, "▶ "+p.Name+": "+truncate(oneLine(p.Input), maxArgsChars))
			case "function_call_output", "custom_tool_call_output":
				out := blockText(p.Output)

				var wrapped struct {
					Output string `json:"output"`
				}

				if json.Unmarshal([]byte(out), &wrapped) == nil && wrapped.Output != "" {
					out = wrapped.Output
				}

				n.add("tool", r.Timestamp, toolResult(out))
			case "web_search_call":
				if p.Action.Query != "" {
					n.add("assistant", r.Timestamp, "▶ web_search: "+truncate(oneLine(p.Action.Query), maxArgsChars))
				}
			}
		}
	}
}
