package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const mcpInstructions = `logovo is the searchable archive of past Claude Code and Codex sessions from every host in the lab: what was done, which commands ran, what they printed, how it ended. Use it when the user refers to earlier work ("like last time", "how did we deploy X", "what was that error with Y") or when you need prior context that is not in the repository or the wiki: call search first, then read the relevant stretch of a session with show.`

type mcpRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(s *server, args json.RawMessage) string
}

var mcpTools = []mcpTool{
	{
		Name: "search",
		Description: `Full-text search over every message of every past Claude Code and Codex session from every host: user text, assistant text, tool calls ("▶ name: command") and truncated tool results ("◀ output"). ` +
			`The query is FTS5: space-separated words must all match, "quoted phrase" matches the words in sequence, OR (upper case) matches either side, prefix* matches word beginnings; invalid syntax falls back to plain words. ` +
			`Hits come best match first, two lines each: the session id, agent, host, time, [turn number], session title, a colon and the working directory's name; then the role (user, assistant or tool) and a snippet with the matches in [brackets]. ` +
			`Read the conversation around a hit with show, passing its session id and turn number.`,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": `FTS5 query, e.g. grafana deploy, "connection refused", nginx OR caddy, kube*`,
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("How many hits to return, 1..%d; default %d, also used for any value outside that range.", maxLimit, defaultLimit),
				},
			},
			"required": []string{"query"},
		},
		run: (*server).mcpSearch,
	},
	{
		Name: "show",
		Description: `Read one past session: a header (session id, agent, user@host, working directory, first .. last time, total turns, title), then turns from..to inclusive, each as "[n] time role:" followed by its text. ` +
			`Take the session id and turn number n from a search hit and ask for a window around it, e.g. from n-5 to n+20; call again to move or widen the window. ` +
			`A session can run to thousands of turns, so never ask for all of it.`,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Session id (uuid) from a search hit.",
				},
				"from": map[string]any{
					"type":        "integer",
					"description": "First turn to show.",
				},
				"to": map[string]any{
					"type":        "integer",
					"description": "Last turn to show, inclusive.",
				},
			},
			"required": []string{"session", "from", "to"},
		},
		run: (*server).mcpShow,
	},
}

// handleMCP speaks MCP over Streamable HTTP, statelessly: one JSON-RPC
// message per POST, a JSON response, no SSE stream. A client of a later
// protocol version is told 400 so that it falls back to initialize.
func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpVersions, v) {
		http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)

		return
	}

	var req mcpRequest

	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeMCP(w, mcpResponse{ID: json.RawMessage("null"), Error: &mcpError{Code: -32700, Message: "parse error"}})

		return
	}

	if req.ID == nil || req.Method == "" {
		w.WriteHeader(http.StatusAccepted)

		return
	}

	writeMCP(w, s.mcpCall(req))
}

func writeMCP(w http.ResponseWriter, resp mcpResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *server) mcpCall(req mcpRequest) mcpResponse {
	resp := mcpResponse{ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = mcpInitialize(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		resp.Result, resp.Error = s.mcpToolCall(req.Params)
	default:
		resp.Error = &mcpError{Code: -32601, Message: "method not found: " + req.Method}
	}

	return resp
}

func mcpInitialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}

	json.Unmarshal(params, &p)
	version := mcpVersions[0]

	if slices.Contains(mcpVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}

	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]string{"name": "logovo", "version": "0.1"},
		"instructions":    mcpInstructions,
	}
}

func (s *server) mcpToolCall(params json.RawMessage) (any, *mcpError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}

	json.Unmarshal(params, &p)
	i := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == p.Name })

	if i < 0 {
		return nil, &mcpError{Code: -32602, Message: "unknown tool: " + p.Name}
	}

	text, isError := "", false

	try(func() {
		text = mcpTools[i].run(s, p.Arguments)
	}).catch(func(exc *Exception) {
		text, isError = exc.Error(), true
	})

	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
		"isError": isError,
	}, nil
}

func (s *server) mcpSearch(args json.RawMessage) string {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}

	throw(json.Unmarshal(args, &a))
	q := strings.TrimSpace(a.Query)

	if q == "" {
		throwFmt("query is required")
	}

	limit := a.Limit

	if limit < 1 || limit > maxLimit {
		limit = defaultLimit
	}

	var b strings.Builder

	for _, h := range s.search(q, limit) {
		b.WriteString(h.text())
	}

	if b.Len() == 0 {
		return "no hits for " + q + "\n"
	}

	return b.String()
}

func (s *server) mcpShow(args json.RawMessage) string {
	var a struct {
		Session string `json:"session"`
		From    int    `json:"from"`
		To      int    `json:"to"`
	}

	throw(json.Unmarshal(args, &a))

	if !uuidRe.MatchString(a.Session) {
		throwFmt("bad session id")
	}

	info, docs := s.session(a.Session, a.From, a.To)
	var b strings.Builder
	writeSession(&b, info, docs)

	return b.String()
}
