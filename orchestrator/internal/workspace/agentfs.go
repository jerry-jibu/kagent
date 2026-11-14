package workspace

import (
	"context"
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
	connect   sync.Once
	client    *MCPClient
	connectEr error
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

// dial lazily connects to the bridge exactly once so backend construction
// cannot fail and concurrent callers share one MCP session.
func (b *AgentfsBackend) dial(ctx context.Context) (*MCPClient, error) {
	b.connect.Do(func() {
		b.client, b.connectEr = NewMCPClient(ctx, b.endpoint)
	})
	return b.client, b.connectEr
}

// EnsureWorkspace creates the per-ticket workspace (blank sandbox).
func (b *AgentfsBackend) EnsureWorkspace(ctx context.Context, name string) error {
	client, err := b.dial(ctx)
	if err != nil {
		return err
	}
	// agentfs create_workspace refuses duplicates; treat that as the
	// idempotent success the Backend contract promises.
	_, err = client.CallTool(ctx, "agenticfs_create_workspace", map[string]any{
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
	client, err := b.dial(ctx)
	if err != nil {
		return err
	}
	_, err = client.CallTool(ctx, "agenticfs_write_file", map[string]any{
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
// surfaces as ErrFileNotFound: the bridge reports missing files through its
// isError channel ("file not found: <path>"), which is matched here — string
// matching on the bridge's error text, hardened when the agentfs theme gives
// the bridge typed errors.
func (b *AgentfsBackend) ReadFile(ctx context.Context, ws, path string) ([]byte, error) {
	if err := validateWorkspacePath(path); err != nil {
		return nil, err
	}
	client, err := b.dial(ctx)
	if err != nil {
		return nil, err
	}
	text, err := client.CallTool(ctx, "agenticfs_read_file", map[string]any{
		"workspace": ws,
		"path":      path,
	})
	if err != nil {
		if isAgentfsFileNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, path)
		}
		if isAgentfsDirectory(err) {
			return nil, fmt.Errorf("%w: %s", ErrIsDirectory, path)
		}
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	return []byte(text), nil
}

// ListFiles lists the workspace-relative paths under prefix via the bridge's
// agenticfs_list_files tool (newline-joined). The bridge reports an empty
// listing as the literal "(empty)", which maps back to an empty slice. A
// prefix filters by lexical prefix; "" lists the whole workspace.
func (b *AgentfsBackend) ListFiles(ctx context.Context, ws, prefix string) ([]string, error) {
	client, err := b.dial(ctx)
	if err != nil {
		return nil, err
	}
	text, err := client.CallTool(ctx, "agenticfs_list_files", map[string]any{
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

// isAgentfsFileNotFound recognizes the bridge's missing-file refusal so
// read-backs can distinguish absence from outage.
// isAgentfsDirectory reports the bridge's read_file refusal on a directory
// (verified against the live bridge: read_file on a listed child directory
// answers exactly "is a directory").
func isAgentfsDirectory(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is a directory")
}

func isAgentfsFileNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "file not found") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "does not exist")
}
