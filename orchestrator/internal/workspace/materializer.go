package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// Materializer runs the D10 route-A preparation for one ticket (docs/designs/
// 0003): after the Engine accepts the ticket, it shallow-clones the
// issue-pointed branch (credentials orchestrator-side only) and materializes
// the working copy into the per-ticket workspace (name = ticket ID, 0001 D3).
// The dispatcher later reads the Report's patch paths back through the same
// Backend (0003: patches/<ticket>-<n>.patch).
//
// Failure semantics (T5 AC): a clone or materialization failure transitions
// the ticket to failed WITH its audit record in one transaction and returns
// promptly — no retries, no hang. The ticket pipeline (Engine/Dispatcher)
// owns what happens next.
type Materializer struct {
	cloner  *Cloner
	backend Backend
	store   *store.Store
	opts    MaterializeOptions
	log     *slog.Logger
}

// MaterializerOptions wires the materializer. Cloner, Backend and Store are
// required; Log falls back to slog.Default().
type MaterializerOptions struct {
	Cloner  *Cloner
	Backend Backend
	Store   *store.Store
	// Options tune the tree walk (e.g. IncludeGitDir).
	Options MaterializeOptions
	// Log receives run summaries and persistence failures.
	Log *slog.Logger
}

// NewMaterializer builds a materializer; it panics on missing wiring because
// a partially wired one could transition tickets without doing the work.
func NewMaterializer(o MaterializerOptions) *Materializer {
	if o.Cloner == nil || o.Backend == nil || o.Store == nil {
		panic("workspace.NewMaterializer: Cloner, Backend and Store are required")
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Materializer{
		cloner:  o.Cloner,
		backend: o.Backend,
		store:   o.Store,
		opts:    o.Options,
		log:     log,
	}
}

// MaterializeTicketWorkspace clones req.Branch at req.RepoURL and
// materializes the working copy into the ticket's workspace. On success it
// audits the run with the base commit; on any failure it transitions the
// ticket to failed (with audit, one transaction) and returns the error —
// joined with any persistence error of that transition, so a caller can
// never mistake a still-accepted ticket for a failed one.
func (m *Materializer) MaterializeTicketWorkspace(ctx context.Context, ticketID string, req CloneRequest) (MaterializeResult, error) {
	outcome, err := m.cloner.Clone(ctx, req)
	if err != nil {
		failure := fmt.Errorf("materialize ticket %s: %w", ticketID, err)
		return MaterializeResult{Workspace: ticketID}, errors.Join(failure, m.failTicket(ctx, ticketID, err.Error()))
	}
	defer os.RemoveAll(outcome.Dir)

	res, err := MaterializeDirectory(ctx, m.backend, ticketID, outcome.Dir, m.opts)
	if err != nil {
		failure := fmt.Errorf("materialize ticket %s: %w", ticketID, err)
		return res, errors.Join(failure, m.failTicket(ctx, ticketID, err.Error()))
	}

	m.log.Info("ticket workspace materialized",
		"ticket", ticketID,
		"workspace", ticketID,
		"base_commit", outcome.BaseCommit,
		"files", res.Files,
		"bytes", res.Bytes,
		"skipped", len(res.Skipped))
	// The baseline is load-bearing for delivery (T7): the deliverer re-clones
	// this exact commit to compute the workspace diff, and excludes the
	// untransportable paths from its deletion set. A ticket without a
	// persisted baseline can never be delivered, so a lost write fails the
	// materialization like any other failure.
	skipped := make([]string, 0, len(res.Skipped))
	for _, s := range res.Skipped {
		skipped = append(skipped, s.Path)
	}
	if err := m.store.RecordTicketBaseline(ctx, ticketID, req.Branch, outcome.BaseCommit, skipped); err != nil {
		failure := fmt.Errorf("failed to record the delivery baseline of ticket %s: %w", ticketID, err)
		return res, errors.Join(failure, m.failTicket(ctx, ticketID, failure.Error()))
	}
	if err := m.store.RecordAudit(ctx, ticketID, "workspace.materialized", map[string]any{
		"workspace":   ticketID,
		"base_commit": outcome.BaseCommit,
		"branch":      req.Branch,
		"files":       res.Files,
		"bytes":       res.Bytes,
		"skipped":     len(res.Skipped),
	}); err != nil {
		// Auditing the success path is best effort; the workspace exists and
		// the ticket pipeline can proceed. A lost audit row is not a failed
		// materialization.
		m.log.Error("failed to record materialization audit",
			"ticket", ticketID, "error", err)
	}

	res.BaseCommit = outcome.BaseCommit
	return res, nil
}

// failTicket transitions the ticket to failed with the audit record and
// returns the persistence error, if any. Callers join it with the original
// failure so a lost transition is never invisible.
func (m *Materializer) failTicket(ctx context.Context, ticketID, reason string) error {
	if err := m.store.FailTicket(ctx, ticketID, reason); err != nil {
		m.log.Error("failed to transition ticket to failed",
			"ticket", ticketID, "reason", reason, "error", err)
		return fmt.Errorf("failed to mark ticket %s failed: %w", ticketID, err)
	}
	m.log.Warn("ticket failed during workspace materialization",
		"ticket", ticketID, "reason", reason)
	return nil
}
