// Command kagentbridge is the kagent-bridge: a streamable-HTTP MCP server
// (the agentfs.kagent.svc:9765/mcp face) that adapts HTTP MCP clients to the
// real agentfs engine — one `agentfs mcp` (stdio) subprocess per HTTP
// session, doing the actual workspace work.
//
// Session contract (verified against orchestrator/internal/workspace/mcp.go
// and the kagent gateway's MCP proxying):
//   - POST /mcp; initialize is synthesized locally (the upstream is
//     initialized by the bridge itself) and assigns Mcp-Session-Id;
//   - notifications (no id) are accepted with no response — the upstream
//     never answers them and forwarding would hang the caller;
//   - tools/call is forwarded to the session's upstream and the response
//     is correlated by id and returned verbatim;
//   - DELETE /mcp ends the session (MCP spec); an idle timeout reaps
//     abandoned sessions;
//   - an unknown session answers 404 so clients re-dial (the orchestrator's
//     client treats 404 as session loss and redials).
//
// The agentfs engine carries the workspace data, checkpoint/rollback and
// audit ledger — this process holds no state beyond session liveness.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// mcpProtocolVersion is the MCP version negotiated with clients and upstream
// (the version the wire actually speaks — orchestrator mcp.go constant).
const mcpProtocolVersion = "2024-11-05"

// idleSessionTTL reaps sessions whose last request is older than this.
const idleSessionTTL = 30 * time.Minute

// rpcRequest is one JSON-RPC request (client → bridge or bridge → upstream).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is one JSON-RPC response (upstream → bridge → client).
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// session is one HTTP MCP session: its upstream `agentfs mcp` subprocess and
// a serializer for the stdio pipe (one request in flight at a time — the
// upstream is a line-oriented single-client process).
type session struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser    // the cmd's stdin pipe writer
	stdout   *bufio.Scanner
	nextID   int64
	lastSeen time.Time
}

// upstreamRequest sends one JSON-RPC request to the `agentfs mcp` subprocess
// and returns its response, correlating by internal id.
func (s *session) upstreamRequest(method string, params json.RawMessage) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	log.Printf("upstream %s begin", method)
	defer func() { log.Printf("upstream %s done in %s", method, time.Since(start)) }()

	s.nextID++
	id := s.nextID
	req := rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", id)), Method: method, Params: params}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}
	if _, err := fmt.Fprintf(s.stdin, "%s\n", line); err != nil {
		return nil, fmt.Errorf("upstream stdin write: %w", err)
	}

	deadline := time.Now().Add(120 * time.Second)
	for s.stdout.Scan() {
		raw := s.stdout.Bytes()
		if len(raw) == 0 {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("upstream timeout waiting for id %d", id)
			}
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			continue // not a JSON-RPC frame (log noise) — keep scanning
		}
		var gotID int64
		if err := json.Unmarshal(resp.ID, &gotID); err != nil || gotID != id {
			continue // response for a different request — keep scanning
		}
		// The agentfs engine omits the jsonrpc tag on responses; downstream
		// MCP clients (the DSH bridge) require "2.0" — normalize at the
		// adapter (PR #11 e2e).
		resp.JSONRPC = "2.0"
		out, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("re-encode upstream response: %w", err)
		}
		return out, nil
	}
	if err := s.stdout.Err(); err != nil {
		return nil, fmt.Errorf("upstream stdout: %w", err)
	}
	return nil, fmt.Errorf("upstream exited before answering id %d", id)
}

// bridge owns the session table.
type bridge struct {
	agentfsBin string
	home       string
	basesDir   string

	mu       sync.Mutex
	sessions map[string]*session
}

func newBridge(agentfsBin, home string) *bridge {
	b := &bridge{agentfsBin: agentfsBin, home: home,
		basesDir: filepath.Join(home, "bases"),
		sessions: map[string]*session{}}
	_ = os.MkdirAll(b.basesDir, 0o755)
	return b
}

func (b *bridge) newSessionID() string {
	return fmt.Sprintf("bridge-%d", time.Now().UnixNano())
}

// startSession spawns the real agentfs engine subprocess (stdio MCP).
func (b *bridge) startSession() (string, *session, error) {
	cmd := exec.Command(b.agentfsBin, "mcp", "--home", b.home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", nil, fmt.Errorf("upstream stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, fmt.Errorf("upstream stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("spawn %s mcp: %w", b.agentfsBin, err)
	}

	s := &session{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewScanner(stdout),
		nextID:   0,
		lastSeen: time.Now(),
	}
	s.stdout.Buffer(make([]byte, 64*1024), 64*1024*1024)

	// Initialize the upstream ourselves: the agentfs stdio MCP needs the
	// handshake before tools/call.
	initParams, _ := json.Marshal(map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"clientInfo":      map[string]any{"name": "kagent-bridge", "version": "0.1.0"},
	})
	if _, err := s.upstreamRequest("initialize", initParams); err != nil {
		_ = cmd.Process.Kill()
		return "", nil, fmt.Errorf("upstream initialize: %w", err)
	}

	id := b.newSessionID()
	b.mu.Lock()
	b.sessions[id] = s
	b.mu.Unlock()
	return id, s, nil
}

func (b *bridge) getSession(id string) (*session, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[id]
	if ok {
		s.lastSeen = time.Now()
	}
	return s, ok
}

func (b *bridge) endSession(id string) {
	b.mu.Lock()
	s, ok := b.sessions[id]
	delete(b.sessions, id)
	b.mu.Unlock()
	if ok {
		_ = s.cmd.Process.Kill()
	}
}

// reapLoop kills sessions abandoned without DELETE (the orchestrator's
// client sends DELETE on Close; a crashed client leaves the subprocess —
// the TTL keeps the pod honest).
func (b *bridge) reapLoop() {
	for range time.Tick(time.Minute) {
		b.mu.Lock()
		var stale []string
		for id, s := range b.sessions {
			if time.Since(s.lastSeen) > idleSessionTTL {
				stale = append(stale, id)
			}
		}
		b.mu.Unlock()
		for _, id := range stale {
			log.Printf("reaping idle session %s", id)
			b.endSession(id)
		}
	}
}

func (b *bridge) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		b.handlePost(w, r)
	case http.MethodDelete:
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			b.endSession(sid)
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		// The standalone SSE stream (MCP spec §2.2.3): hold an
		// event-stream open for the session's lifetime, sending nothing.
		// Clients that require the stream (the DSH bridge's go-sdk treats
		// a failed GET as a session failure) keep their logical session
		// alive; keep-alive comments defeat proxy idle timeouts.
		sid := r.Header.Get("Mcp-Session-Id")
		_, ok := b.getSession(sid)
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, canFlush := w.(http.Flusher)
		if canFlush {
			flusher.Flush()
		}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if canFlush {
					fmt.Fprint(w, ": keep-alive\n\n")
					flusher.Flush()
				}
			}
		}
	default:
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

func (b *bridge) handlePost(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON-RPC body", http.StatusBadRequest)
		return
	}
	log.Printf("http %s session=%q", req.Method, r.Header.Get("Mcp-Session-Id"))

	// Notifications carry no id: accept with no response body (202) — the
	// upstream never answers them (see mcp.go's documented deviation).
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	sid := r.Header.Get("Mcp-Session-Id")
	if req.Method == "initialize" {
		// Synthesized locally: the upstream is already initialized by the
		// bridge. Assign the session and answer per the MCP spec.
		id, _, err := b.startSession()
		if err != nil {
			http.Error(w, fmt.Sprintf("session start failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Mcp-Session-Id", id)
		w.Header().Set("Content-Type", "application/json")
		result, _ := json.Marshal(map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agentfs", "version": "0.1.0"},
		})
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	s, ok := b.getSession(sid)
	if !ok {
		// 404 = session loss: the orchestrator's client redials on this.
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	// Tool-namespace mapping (PR #11 e2e): the deployed contract is the
	// agenticfs_* namespace (docs/env.md: RemoteMCPServer agenticfs-mcp
	// serves agenticfs_create_workspace/write_file/read_file/...), while
	// the engine names its tools bare (create_workspace, write_file, ...).
	// The bridge owns the aliasing in both directions.
	if req.Method == "tools/list" {
		out, err := s.upstreamRequest(req.Method, req.Params)
		if err != nil {
			b.endSession(sid)
			http.Error(w, fmt.Sprintf("upstream failure: %v", err), http.StatusBadGateway)
			return
		}
		out = rewriteResponseID(out, req.ID)
		out = prefixToolNames(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		return
	}
	if req.Method == "tools/call" {
		if mapped, stripped := stripToolPrefix(req.Params); stripped {
			req.Params = mapped
		}
		// The engine's overlay requires ≥1 base; the orchestrator's
		// contract (0001 D3) is a ZERO-SOURCE blank sandbox the
		// materializer populates via write_file. create_workspace without
		// sources gets a fresh empty base directory under the bridge's
		// data root — a real agentfs workspace over a real (empty) base.
		if req.Method == "tools/call" && strings.Contains(string(req.Params), "create_workspace") {
			req.Params = b.withBlankBase(req.Params)
		}
	}

	out, err := s.upstreamRequest(req.Method, req.Params)
	if err != nil {
		// Upstream death or timeout: end the session so the next call
		// re-dials instead of hammering a dead pipe.
		b.endSession(sid)
		http.Error(w, fmt.Sprintf("upstream failure: %v", err), http.StatusBadGateway)
		return
	}
	out = rewriteResponseID(out, req.ID)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// withBlankBase rewrites an agenticfs_create_workspace call that carries no
// sources: the bridge materializes an empty base directory under its data
// root and passes it as the workspace's single source. Semantics per 0001
// D3 (零 source 空白沙箱) on an engine whose overlay requires a base.
func (b *bridge) withBlankBase(params json.RawMessage) json.RawMessage {
	var call struct {
		Name      string   `json:"name"`
		Arguments struct {
			Name    string   `json:"name"`
			Sources []string `json:"sources"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return params
	}
	if len(call.Arguments.Sources) > 0 {
		return params // caller-provided bases win
	}
	if call.Arguments.Name == "" {
		return params // let the engine refuse the missing name
	}
	base := filepath.Join(b.basesDir, call.Arguments.Name)
	if err := os.MkdirAll(base, 0o755); err != nil {
		log.Printf("blank base mkdir for %s: %v", call.Arguments.Name, err)
		return params
	}
	args, err := json.Marshal(map[string]any{
		"name":      call.Arguments.Name,
		"sources":   []string{base},
		"intention": "kagent-bridge: blank base for a zero-source workspace (0001 D3)",
	})
	if err != nil {
		return params
	}
	out, err := json.Marshal(map[string]any{"name": call.Name, "arguments": json.RawMessage(args)})
	if err != nil {
		return params
	}
	return out
}

// rewriteResponseID replaces the response's JSON-RPC id with the CLIENT's
// request id: the upstream was asked with the bridge's internal numbering,
// and the go-sdk client matches responses to pending calls by id — a
// mismatched id is silently discarded and the call waits out its deadline.
func rewriteResponseID(resp json.RawMessage, clientID json.RawMessage) json.RawMessage {
	// Whole-envelope rewrite via RawMessage map: the result/error content
	// must survive untouched.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(resp, &fields); err != nil {
		return resp
	}
	fields["jsonrpc"] = json.RawMessage(`"2.0"`)
	if clientID != nil {
		fields["id"] = clientID
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return resp
	}
	return out
}

// prefixToolNames rewrites a tools/list RESPONSE ENVELOPE: every tool name
// gains the agenticfs_ prefix (the deployed MCP surface). The envelope's
// jsonrpc/id fields are preserved and the version tag is forced to "2.0" —
// downstream clients (the DSH bridge's go-sdk) validate it on every frame.
func prefixToolNames(result json.RawMessage) json.RawMessage {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(result, &envelope); err != nil {
		return result // not a tools/list shape — pass through
	}
	rawTools, ok := envelope["result"]
	if !ok {
		return result
	}
	var resultList struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(rawTools, &resultList); err != nil {
		return result
	}
	for i := range resultList.Tools {
		name, _ := resultList.Tools[i]["name"].(string)
		resultList.Tools[i]["name"] = "agenticfs_" + name
		if _, ok := resultList.Tools[i]["inputSchema"]; !ok {
			// The engine declares some tools without an inputSchema; the
			// go-sdk's server-side AddTool refuses schema-less tools —
			// inject the empty-object schema these tools actually take.
			resultList.Tools[i]["inputSchema"] = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
	}
	patched, err := json.Marshal(resultList)
	if err != nil {
		return result
	}
	envelope["result"] = patched
	envelope["jsonrpc"] = json.RawMessage(`"2.0"`)
	out, err := json.Marshal(envelope)
	if err != nil {
		return result
	}
	return out
}

// stripToolPrefix maps an agenticfs_* tools/call argument back to the
// engine's bare tool name.
func stripToolPrefix(params json.RawMessage) (json.RawMessage, bool) {
	var call struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return params, false
	}
	const ns = "agenticfs_"
	if !strings.HasPrefix(call.Name, ns) {
		return params, false
	}
	// Re-encode preserving the arguments object verbatim.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil {
		return params, false
	}
	out, err := json.Marshal(map[string]any{
		"name":      strings.TrimPrefix(call.Name, ns),
		"arguments": raw["arguments"],
	})
	if err != nil {
		return params, false
	}
	return out, true
}

func main() {
	agentfsBin := flag.String("agentfs", "/usr/local/bin/agentfs", "path to the agentfs engine binary")
	home := flag.String("home", "/data/.agenticfs", "agentfs state directory (workspace data, audit ledger)")
	addr := flag.String("addr", ":9765", "listen address")
	flag.Parse()

	if _, err := os.Stat(*agentfsBin); err != nil {
		log.Fatalf("agentfs binary not found at %s: %v", *agentfsBin, err)
	}
	if err := os.MkdirAll(*home, 0o755); err != nil {
		log.Fatalf("create home %s: %v", *home, err)
	}
	homeAbs, err := filepath.Abs(*home)
	if err != nil {
		log.Fatalf("resolve home: %v", err)
	}

	b := newBridge(*agentfsBin, homeAbs)
	go b.reapLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", b.handleMCP)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("kagent-bridge serving on %s (upstream %s mcp --home %s)", *addr, *agentfsBin, homeAbs)
	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
