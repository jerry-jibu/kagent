package workspace_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// fakeAgentfs emulates the agentfs kagent-bridge MCP face so the materializer
// is tested against the real wire contract (docs/env.md: agentfs Pod exposes
// an MCP bridge at /mcp). It reproduces the protocol behaviors the
// orchestrator depends on, verified against the upstream sources:
//   - streamable HTTP, one JSON response per POST (kagent-bridge main.go
//     handler: Content-Type application/json, Mcp-Session-Id on initialize)
//   - tool names namespaced with the agenticfs_ prefix
//   - handler errors flow through the MCP isError channel, unknown tools
//     through JSON-RPC -32602, unknown methods through -32601
//   - write_file requires intention; create_workspace refuses duplicates
//   - content travels as a JSON string (agentfs read_file caps at 4MiB and
//     returns a string; the fake stores exactly the decoded string)
type fakeAgentfs struct {
	srv *httptest.Server

	mu         sync.Mutex
	session    string
	workspaces map[string]map[string]string // workspace name -> path -> content
	calls      []string                     // "name" per tools/call, in order
}

func newFakeAgentfs() *fakeAgentfs {
	f := &fakeAgentfs{
		session:    fmt.Sprintf("fake-agentfs-%d", time.Now().UnixNano()),
		workspaces: map[string]map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeAgentfs) close() { f.srv.Close() }

func (f *fakeAgentfs) endpoint() string { return f.srv.URL + "/mcp" }

func (f *fakeAgentfs) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAgentfs) workspaceFiles(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for p, c := range f.workspaces[name] {
		out[p] = c
	}
	return out
}

func (f *fakeAgentfs) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`))
		return
	}

	switch req.Method {
	case "initialize":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", f.session)
		f.reply(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agentfs-bridge", "version": "0.1.0"},
		})
	case "tools/call":
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				f.rpcError(w, req.ID, -32602, "invalid arguments")
				return
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, req.Params.Name)
		f.mu.Unlock()
		text, isErr := f.callTool(req.Params.Name, args)
		f.mu.Lock()
		defer f.mu.Unlock()
		result := map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}
		if isErr {
			result["isError"] = true
		}
		f.reply(w, req.ID, result)
	default:
		f.rpcError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// callTool mirrors the agentfs tool semantics the materializer relies on.
func (f *fakeAgentfs) callTool(name string, args map[string]any) (text string, isErr bool) {
	str := func(key string) string {
		s, _ := args[key].(string)
		return s
	}
	switch name {
	case "agenticfs_create_workspace":
		if str("intention") == "" {
			return "intention is required", true
		}
		n := str("name")
		if _, exists := f.workspaces[n]; exists {
			return fmt.Sprintf("workspace %q already exists", n), true
		}
		f.workspaces[n] = map[string]string{}
		return fmt.Sprintf("workspace %q created", n), false
	case "agenticfs_write_file":
		if str("intention") == "" {
			return "intention is required", true
		}
		ws, ok := f.workspaces[str("workspace")]
		if !ok {
			return "workspace not found: " + str("workspace"), true
		}
		ws[str("path")] = str("content")
		return "written", false
	case "agenticfs_read_file":
		ws, ok := f.workspaces[str("workspace")]
		if !ok {
			return "workspace not found: " + str("workspace"), true
		}
		content, ok := ws[str("path")]
		if !ok {
			return "file not found: " + str("path"), true
		}
		return content, false
	case "agenticfs_list_files":
		ws, ok := f.workspaces[str("workspace")]
		if !ok {
			return "workspace not found: " + str("workspace"), true
		}
		prefix := strings.TrimPrefix(str("path"), "/")
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		var out []string
		for p := range ws {
			if prefix == "/" || strings.HasPrefix(p, prefix) {
				out = append(out, strings.TrimPrefix(p, prefix))
			}
		}
		if len(out) == 0 {
			return "(empty)", false
		}
		return strings.Join(out, "\n"), false
	default:
		return "unknown tool " + name, true
	}
}

func (f *fakeAgentfs) reply(w http.ResponseWriter, id json.RawMessage, result map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAgentfs) rpcError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
	_ = json.NewEncoder(w).Encode(body)
}
