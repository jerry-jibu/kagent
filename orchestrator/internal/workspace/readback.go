package workspace

import (
	"context"
	"errors"
	"fmt"
)

// Read-back work bounds (PR #11 review, Required): mirrored from the
// materialization side. A worker that generated tens of thousands of files
// must fail its delivery loudly, not run one beat for hours.
const (
	// DefaultMaxReadFiles caps the read-back file count.
	DefaultMaxReadFiles = 50_000
	// DefaultMaxReadBytes caps the total read-back payload.
	DefaultMaxReadBytes = 500 << 20
)

// errReadBudgetExceeded marks a read-back that crossed its budget.
var errReadBudgetExceeded = fmt.Errorf("workspace read-back exceeded its work budget")

// ReadWorkspaceState reads the whole workspace back: every listed path with
// its byte-identical content. This is the deliverer's view of the worker's
// sandbox (0003 Alternative E: the orchestrator computes the patch from the
// workspace read-back state — the worker never produces one).
//
// A file listed but vanished before its read-back (checkpoint rollback, or
// the worker's own last-moment cleanup) is treated as absent: the read-back
// snapshot is what the workspace truly holds at read time, and the patcher
// turns absence into a deletion of the baseline file. Backend outages surface
// as errors for the caller to retry — an incomplete snapshot must never read
// as complete.
func ReadWorkspaceState(ctx context.Context, backend Backend, ws string) (map[string][]byte, error) {
	return ReadWorkspaceStateBounded(ctx, backend, ws, DefaultMaxReadFiles, DefaultMaxReadBytes)
}

// ReadWorkspaceStateBounded is ReadWorkspaceState with explicit budgets
// (negative values disable a bound).
func ReadWorkspaceStateBounded(ctx context.Context, backend Backend, ws string, maxFiles int, maxBytes int64) (map[string][]byte, error) {
	state := map[string][]byte{}
	var files int64
	var bytes int64
	if err := readBackInto(ctx, backend, ws, "", state, &files, &bytes, maxFiles, maxBytes); err != nil {
		return nil, err
	}
	return state, nil
}

// readBackInto collects every file under prefix into state. The real agentfs
// bridge lists IMMEDIATE children (directories as bare names), so a listed
// directory recurses through ListFiles(prefix); a listed file is read.
func readBackInto(ctx context.Context, backend Backend, ws, prefix string, state map[string][]byte,
	files *int64, bytes *int64, maxFiles int, maxBytes int64) error {
	paths, err := backend.ListFiles(ctx, ws, prefix)
	if err != nil {
		return fmt.Errorf("failed to list workspace %s: %w", ws, err)
	}
	if maxFiles >= 0 && *files+int64(len(paths)) > int64(maxFiles) {
		return fmt.Errorf("%w: at least %d files under %s/, the cap is %d", errReadBudgetExceeded, *files+int64(len(paths)), prefix, maxFiles)
	}

	for _, path := range paths {
		full := path
		if prefix != "" {
			full = prefix + "/" + path
		}
		if err := validateWorkspacePath(full); err != nil {
			// A listing carrying an unrepresentable path is malformed
			// durable data: rejected, never silently dropped.
			return fmt.Errorf("workspace %s lists an invalid path: %w", ws, err)
		}
		content, err := backend.ReadFile(ctx, ws, full)
		if err != nil {
			switch {
			case isNotFound(err):
				continue // vanished between listing and read: absent now
			case errors.Is(err, ErrIsDirectory):
				if err := readBackInto(ctx, backend, ws, full, state, files, bytes, maxFiles, maxBytes); err != nil {
					return err
				}
				continue
			default:
				return fmt.Errorf("failed to read back %s from workspace %s: %w", full, ws, err)
			}
		}
		if maxBytes >= 0 && *bytes+int64(len(content)) > maxBytes {
			return fmt.Errorf("%w: %d bytes read so far, the cap is %d (at %s)", errReadBudgetExceeded, *bytes, maxBytes, full)
		}
		*files++
		*bytes += int64(len(content))
		state[full] = content
	}
	return nil
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrFileNotFound)
}
