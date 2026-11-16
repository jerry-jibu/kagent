package workspace

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// agentfsMaxFileBytes mirrors the agentfs read_file cap (agentfs
// internal/workspace/manager.go: capBytes = 4 << 20). Files beyond it
// cannot be read back through the bridge, so the backend refuses them
// symmetrically at write time.
const agentfsMaxFileBytes = 4 << 20

// AgentfsBackend is the P0 materialization channel: it drives the agentfs
// kagent-bridge MCP endpoint as an MCP client (docs/env.md: 集群内 agentfs
// Pod 暴露 MCP 桥，RemoteMCPServer agenticfs-mcp 指向
// http://agentfs.kagent.svc.cluster.local:9765/mcp). Workspaces are the
// per-ticket blank sandboxes of 0001 D3.
//
// Transport constraint (verified against the agentfs sources): MCP tool
// arguments/results travel as JSON strings, and agentfs read_file returns
// a string capped at 4MiB. The backend therefore refuses — with
// ErrNotTransportable — content that is not valid UTF-8 or exceeds the
// cap, so callers can report those paths instead of materializing
// corrupted bytes.
type AgentfsBackend struct {
	endpoint  string
	mu        sync.Mutex // guards client — a failed dial must stay retryable
	client    *MCPClient
	intention string
}

// NewAgentfsBackend connects lazily to the agentfs MCP bridge at endpoint.
// The fixed intention string is recorded by the agentfs audit ledger for
// every write (agentfs write_file requires intention; the orchestrator
// acts as the materialization service, so its intention names that role).
func NewAgentfsBackend(endpoint string) *AgentfsBackend {
	return &AgentfsBackend{
		endpoint:  endpoint,
		intention: "orchestrator workspace materialization (superagent-on-kagent P0)",
	}
}

// dial lazily connects to the bridge, caching ONLY successful dials. A
// cached failure would brick the materialize and delivery loops until
// process restart (PR #11 review, Required): a bridge outage at first dial
// must stay retryable, and a session invalidated by a bridge restart
// (ErrSessionLost) re-dials a fresh session.
func (b *AgentfsBackend) dial(ctx context.Context) (*MCPClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return b.client, nil
	}
	client, err := NewMCPClient(ctx, b.endpoint)
	if err != nil {
		return nil, err
	}
	b.client = client
	return b.client, nil
}

// redial closes the current client (best effort) and forgets it, so the
// next call dials a fresh session.
func (b *AgentfsBackend) redial() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		b.client.Close()
		b.client = nil
	}
}

// call runs one tool call with session-loss re-dial: an ErrSessionLost
// error forgets the cached client and retries once on a fresh session —
// a bridge restart invalidates sessions but the backend must recover.
func (b *AgentfsBackend) call(ctx context.Context, tool string, args map[string]any) (string, error) {
	client, err := b.dial(ctx)
	if err != nil {
		return "", err
	}
	text, err := client.CallTool(ctx, tool, args)
	if errors.Is(err, ErrSessionLost) {
		b.redial()
		client, err = b.dial(ctx)
		if err != nil {
			return "", err
		}
		text, err = client.CallTool(ctx, tool, args)
	}
	return text, err
}

// EnsureWorkspace creates the per-ticket workspace (blank sandbox).
func (b *AgentfsBackend) EnsureWorkspace(ctx context.Context, name string) error {
	// agentfs create_workspace refuses duplicates; treat that as the
	// idempotent success the Backend contract promises.
	_, err := b.call(ctx, "agenticfs_create_workspace", map[string]any{
		"name":      name,
		"intention": b.intention,
	})
	if err == nil {
		return nil
	}
	if isAgentfsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("failed to create workspace %s: %w", name, err)
}

// WriteFile writes one file into the workspace delta layer. Parent
// directories are created implicitly by agentfs (go-billy OpenFile with
// O_CREATE creates parents).
func (b *AgentfsBackend) WriteFile(ctx context.Context, ws, path string, content []byte) error {
	if err := validateWorkspacePath(path); err != nil {
		return err
	}
	if !utf8.Valid(content) {
		return fmt.Errorf("%w: %s is binary; the agentfs MCP bridge carries text only", ErrNotTransportable, path)
	}
	if len(content) > agentfsMaxFileBytes {
		return fmt.Errorf("%w: %s is %d bytes; the agentfs read cap is %d", ErrNotTransportable, path, len(content), agentfsMaxFileBytes)
	}
	_, err := b.call(ctx, "agenticfs_write_file", map[string]any{
		"workspace": ws,
		"path":      path,
		"content":   string(content),
		"intention": b.intention,
	})
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}

// ReadFile reads one file back by path. A path absent from the workspace
// surfaces as ErrFileNotFound — but ONLY when the bridge's error carries the
// engine's exact absence format naming THIS path ("file not found: <path>",
// see isAgentfsFileNotFound). Anything else, however much it smells like a
// missing file, stays an outage: absence is what feeds the patcher's
// deletions of baseline files, so imprecision there is destructive.
func (b *AgentfsBackend) ReadFile(ctx context.Context, ws, path string) ([]byte, error) {
	if err := validateWorkspacePath(path); err != nil {
		return nil, err
	}
	text, err := b.call(ctx, "agenticfs_read_file", map[string]any{
		"workspace": ws,
		"path":      path,
	})
	if err != nil {
		if isAgentfsFileNotFound(err, path) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, path)
		}
		if isAgentfsDirectory(err) {
			return nil, fmt.Errorf("%w: %s", ErrIsDirectory, path)
		}
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	return []byte(text), nil
}

// ListFiles lists the IMMEDIATE children of prefix via the bridge's
// agenticfs_list_files tool (newline-joined): directories as bare names,
// files as their names, sorted lexically — the bridge does NOT recurse, and
// "" lists the children of the workspace root. The bridge reports an empty
// listing as the literal "(empty)", which maps back to an empty slice.
func (b *AgentfsBackend) ListFiles(ctx context.Context, ws, prefix string) ([]string, error) {
	text, err := b.call(ctx, "agenticfs_list_files", map[string]any{
		"workspace": ws,
		"path":      prefix,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list workspace %s: %w", ws, err)
	}
	if text == "" || text == "(empty)" {
		return nil, nil
	}
	paths := strings.Split(strings.TrimRight(text, "\n"), "\n")
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// isAgentfsAlreadyExists recognizes the agentfs duplicate-workspace
// refusal so EnsureWorkspace stays idempotent.
func isAgentfsAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "already in use")
}

// isAgentfsDirectory reports the bridge's read_file refusal on a directory
// (verified against the live bridge: read_file on a listed child directory
// answers exactly "is a directory").
func isAgentfsDirectory(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is a directory")
}

// agentfsFileNotFoundPrefix is the absence format the agentfs engine answers
// a read_file refusal with ("file not found: <path>"; verified against the
// engine and the deployed bridge, which forwards the text verbatim).
const agentfsFileNotFoundPrefix = "file not found: "

// isAgentfsFileNotFound recognizes the bridge's missing-file refusal at PATH
// PRECISION (PR #11 review, Required): the message must carry the engine's
// exact absence format AND name exactly the path that was requested. Absence
// is authoritative for the read-back (readback.go turns ErrFileNotFound into
// deletions of baseline files), so a loose classification is destructive: a
// workspace-level failure whose text merely contains "does not exist" or
// "no such file" mid-read-back would masquerade as absence and delete the
// baseline tree. Those generic substrings therefore NEVER classify as
// absence — without the exact format plus the path equality, the error is an
// outage (transient, loud). The engine text arrives as the LAST segment of
// the wrapped error chain (mcp.go and the callers prefix context onto it),
// hence LastIndex of the marker; only that well-defined prefix is stripped.
func isAgentfsFileNotFound(err error, path string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	at := strings.LastIndex(msg, agentfsFileNotFoundPrefix)
	if at < 0 {
		return false
	}
	reported := strings.TrimSpace(msg[at+len(agentfsFileNotFoundPrefix):])
	return reported == path
}
