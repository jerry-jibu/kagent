package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// MaterializeOptions tunes one materialization run.
type MaterializeOptions struct {
	// IncludeGitDir also materializes the .git directory. Default false:
	// the shallow clone's object packs are binary and the agentfs MCP
	// bridge carries text only, so a .git sent through the bridge would
	// land as flagged, unusable fragments. The worker-side git story
	// depends on a byte-safe bulk channel (0003 defers agentfs write
	// mechanics; see the T5 report).
	IncludeGitDir bool
}

// MaterializeResult reports one materialization run.
type MaterializeResult struct {
	// Workspace is the per-ticket workspace that received the tree.
	Workspace string
	// BaseCommit is the resolved HEAD SHA of the cloned branch (0003 路线 A:
	// 浅克隆基线 = issue 指向分支 HEAD); empty unless a clone produced it.
	BaseCommit string
	// Files is the number of files written through the backend.
	Files int
	// Bytes is the total payload written through the backend.
	Bytes int64
	// Skipped lists every source path that did NOT land, with the reason.
	// Untransportable content is reported, never silently dropped.
	Skipped []SkippedFile
}

// SkippedFile is one source path that did not reach the workspace.
type SkippedFile struct {
	Path   string
	Reason string
}

// MaterializeDirectory walks the local directory dir (typically a fresh
// shallow clone) and materializes every file into the named workspace
// through backend, in deterministic path order.
//
// Per-file transport refusals (ErrNotTransportable, e.g. binary or over-cap
// content) are recorded in the result and the run continues; systemic
// failures (workspace creation, backend outage) fail the run.
func MaterializeDirectory(ctx context.Context, backend Backend, wsName, dir string, opts MaterializeOptions) (MaterializeResult, error) {
	res := MaterializeResult{Workspace: wsName}

	if err := backend.EnsureWorkspace(ctx, wsName); err != nil {
		return res, fmt.Errorf("failed to ensure workspace %s: %w", wsName, err)
	}

	paths, err := collectMaterializablePaths(dir, opts.IncludeGitDir)
	if err != nil {
		return res, err
	}

	for _, rel := range paths {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return res, fmt.Errorf("failed to stat %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			res.Skipped = append(res.Skipped, SkippedFile{Path: rel, Reason: "symlink"})
			continue
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return res, fmt.Errorf("failed to read %s: %w", rel, err)
		}
		if err := backend.WriteFile(ctx, wsName, rel, content); err != nil {
			if isNotTransportable(err) {
				res.Skipped = append(res.Skipped, SkippedFile{Path: rel, Reason: err.Error()})
				continue
			}
			return res, fmt.Errorf("materialization aborted at %s: %w", rel, err)
		}
		res.Files++
		res.Bytes += int64(len(content))
	}
	return res, nil
}

// isNotTransportable reports whether err is a per-file transport refusal
// the run can record and continue past.
func isNotTransportable(err error) bool {
	return errors.Is(err, ErrNotTransportable)
}

// collectMaterializablePaths lists the workspace-relative file paths under
// dir, sorted lexically for deterministic runs. The .git directory is
// excluded unless explicitly requested (see MaterializeOptions).
func collectMaterializablePaths(dir string, includeGitDir bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if rel == ".git" && !includeGitDir {
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk %s: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}
