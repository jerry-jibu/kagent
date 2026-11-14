// Package workspace is the ticket-workspace materializer (T5, docs/designs/
// 0003): after a Ticket is accepted, the orchestrator shallow-clones the
// target branch and materializes the working copy into the per-ticket
// workspace, and reads files back by path for patch retrieval.
//
// The materialization channel is pluggable (0003: 物化通道可插拔). Backend
// is the contract seam: implementations own the transport into and out of
// the per-ticket workspace. This package ships the agentfs backend, which
// drives the agentfs MCP bridge as an MCP client.
//
// Credential monotonicity (0002 A5 / 0003): git credentials never leave the
// orchestrator side. The clone happens in this package's Cloner with
// transient per-process credentials; the materialized workspace receives
// file contents only — no tokens are embedded anywhere in the workspace.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Backend is the pluggable materialization channel (docs/designs/0003).
// Implementations promise a byte-faithful write/read round trip for every
// path they accept, and refuse — never corrupt — content they cannot carry.
type Backend interface {
	// EnsureWorkspace creates the named per-ticket workspace if it does
	// not exist (blank sandbox per 0001 D3: directory name = ticket ID).
	// Calling it for an existing workspace is a no-op.
	EnsureWorkspace(ctx context.Context, name string) error
	// WriteFile writes one file by path inside the workspace, replacing
	// any previous content, creating parent directories implicitly.
	WriteFile(ctx context.Context, workspace, path string, content []byte) error
	// ReadFile reads one file by path inside the workspace. A path that
	// does not exist reads back as ErrFileNotFound.
	ReadFile(ctx context.Context, workspace, path string) ([]byte, error)
	// ListFiles lists the workspace-relative paths under prefix, sorted
	// lexically. An empty prefix lists the whole workspace; an empty
	// workspace lists empty.
	ListFiles(ctx context.Context, workspace, prefix string) ([]string, error)
}

// ErrNotTransportable marks content a Backend cannot carry losslessly.
// Callers record the path and continue; the workspace must never contain
// silently corrupted bytes.
var ErrNotTransportable = errors.New("content not transportable over this backend")

// ErrFileNotFound marks a read of a path that does not exist in the
// workspace. The delivery read-back treats it as absence (the file is
// genuinely not in the workspace), never as an outage.
var ErrFileNotFound = errors.New("file not found in workspace")

// validateWorkspacePath enforces workspace-relative, slash-separated paths
// with no traversal, absolute reference, or NUL. The agentfs workspace also
// enforces this (read_file/write_file containment); validating at the seam
// keeps every backend honest.
func validateWorkspacePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("workspace path is empty")
	case strings.ContainsRune(path, 0):
		return fmt.Errorf("workspace path contains NUL: %q", path)
	case strings.HasPrefix(path, "/"), strings.HasPrefix(path, `\`):
		return fmt.Errorf("workspace path must be relative: %q", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("workspace path has an invalid segment: %q", path)
		}
	}
	return nil
}
