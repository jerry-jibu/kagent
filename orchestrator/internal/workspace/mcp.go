package workspace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// mcpProtocolVersion is the MCP protocol version negotiated with the agentfs
// bridge. The bridge (deploy/kagent-bridge) handshakes its upstream at
// "2024-11-05" and synthesizes its initialize response locally, so this is
// the version the wire actually speaks.
const mcpProtocolVersion = "2024-11-05"

// MCPClient is a minimal MCP streamable-HTTP client for the agentfs bridge
// (docs/env.md: 集群内 agentfs Pod 暴露 MCP 桥). It speaks JSON-RPC 2.0 over
// POST and keeps the negotiated Mcp-Session-Id on every request.
//
// Deliberate deviation from the MCP spec: notifications/initialized is NOT
// sent after initialize. The kagent-bridge forwards every non-initialize
// method to its stdio upstream and waits for a response (120s); a
// notification gets none, so sending it would hang each new session.
// The bridge does not require it (it synthesizes initialize locally and
// accepts tools/call directly).
type MCPClient struct {
	endpoint string
	hc       *http.Client
	session  string
	nextID   atomic.Int64
}

// NewMCPClient dials the MCP endpoint and performs the initialize handshake.
func NewMCPClient(ctx context.Context, endpoint string) (*MCPClient, error) {
	c := &MCPClient{
		endpoint: endpoint,
		hc:       &http.Client{},
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "kagent-orchestrator", "version": "0.1.0"},
	}, &result); err != nil {
		return nil, fmt.Errorf("MCP initialize failed: %w", err)
	}
	// Session id arrives with the initialize response headers; rpc() stored it.
	if c.session == "" {
		return nil, fmt.Errorf("MCP server did not assign a session id")
	}
	return c, nil
}

// CallTool invokes one MCP tool and returns its first text content. A tool
// that reports isError surfaces as an error carrying the server's text.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	if err := c.rpc(ctx, "tools/call", params, &result); err != nil {
		return "", fmt.Errorf("MCP tool %s failed: %w", name, err)
	}
	var text string
	for _, c := range result.Content {
		if c.Type == "text" {
			text = c.Text
			break
		}
	}
	if result.IsError {
		return text, fmt.Errorf("tool %s reported an error: %s", name, text)
	}
	return text, nil
}

// Close releases the HTTP session. Errors are irrelevant at teardown.
func (c *MCPClient) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", c.session)
	_, _ = c.hc.Do(req)
}

// rpc posts one JSON-RPC request and decodes the result into out.
// Per the streamable HTTP transport the response may be a bare JSON body or
// an SSE stream; both are handled.
func (c *MCPClient) rpc(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Session assignment: the initialize response carries it (MCP spec).
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	var raw json.RawMessage
	if strings.HasPrefix(contentType, "text/event-stream") {
		raw, err = readSSEMessage(resp.Body, id)
	} else {
		raw, err = readJSONResponse(resp.Body, id)
	}
	if err != nil {
		return err
	}

	var framed struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &framed); err != nil {
		return fmt.Errorf("failed to decode response envelope: %w", err)
	}
	if framed.Error != nil {
		return fmt.Errorf("JSON-RPC error %d: %s", framed.Error.Code, framed.Error.Message)
	}
	if out != nil && len(framed.Result) > 0 {
		if err := json.Unmarshal(framed.Result, out); err != nil {
			return fmt.Errorf("failed to decode result: %w", err)
		}
	}
	return nil
}

// readJSONResponse extracts the JSON-RPC message with the given id from a
// bare JSON body (the kagent-bridge always answers this way).
func readJSONResponse(r io.Reader, wantID int64) (json.RawMessage, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	return matchResponseID(body, wantID)
}

// readSSEMessage extracts the JSON-RPC message with the given id from an
// SSE stream (spec-compliant servers may answer this way). It scans
// data: lines until the event carrying our id arrives.
func readSSEMessage(r io.Reader, wantID int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var dataLines []string
	flush := func() (json.RawMessage, bool) {
		if len(dataLines) == 0 {
			return nil, false
		}
		msg := []byte(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		out, err := matchResponseID(msg, wantID)
		if err != nil {
			return nil, false
		}
		return out, true
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if msg, ok := flush(); ok {
				return msg, nil
			}
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data:"))
		}
	}
	if msg, ok := flush(); ok {
		return msg, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("failed to read SSE stream: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without a response for id %d", wantID)
}

// matchResponseID validates that msg is the response for wantID.
func matchResponseID(msg []byte, wantID int64) (json.RawMessage, error) {
	var framed struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg, &framed); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	var id int64
	if err := json.Unmarshal(framed.ID, &id); err != nil || id != wantID {
		return nil, fmt.Errorf("response for a different request id")
	}
	return json.RawMessage(msg), nil
}
