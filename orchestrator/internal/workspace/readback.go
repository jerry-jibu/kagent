package workspace

import (
	"context"
	"errors"
	"fmt"
)

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
	paths, err := backend.ListFiles(ctx, ws, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list workspace %s: %w", ws, err)
	}

	state := make(map[string][]byte, len(paths))
	for _, path := range paths {
		if err := validateWorkspacePath(path); err != nil {
			// A listing carrying an unrepresentable path is malformed
			// durable data: rejected, never silently dropped.
			return nil, fmt.Errorf("workspace %s lists an invalid path: %w", ws, err)
		}
		content, err := backend.ReadFile(ctx, ws, path)
		if err != nil {
			if isNotFound(err) {
				continue // vanished between listing and read: absent now
			}
			return nil, fmt.Errorf("failed to read back %s from workspace %s: %w", path, ws, err)
		}
		state[path] = content
	}
	return state, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrFileNotFound)
}
