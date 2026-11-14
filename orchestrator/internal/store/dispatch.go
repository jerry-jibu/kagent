package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrAssignmentNotPending is returned when a dispatch transition targets an
// assignment that is no longer pending: a concurrent dispatch won, or the
// transition is a replay. Either way the side effect must not repeat.
var ErrAssignmentNotPending = errors.New("assignment is not pending")

// ErrAssignmentNotRunning is returned when a terminal transition targets an
// assignment that is neither running nor already settled: a dispatch that
// never landed has no task to reconcile.
var ErrAssignmentNotRunning = errors.New("assignment is not running")

// ReportProgress is the report state the follow-up scan continues
// (docs/designs/0001 §3: progress = work continues).
const ReportProgress = "progress"

// AssignmentByTaskID resolves the assignment bound to an A2A task id — the
// push receiver's lookup key. ok=false when no assignment carries the task.
func (s *Store) AssignmentByTaskID(ctx context.Context, taskID string) (Assignment, Ticket, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+assignmentJoinColumns+`,
		       tk.id, tk.thread_source, tk.thread_key, tk.mandate, tk.status, tk.session_id,
		       tk.trigger_source, tk.trigger_deterministic_id, tk.created_at
		FROM assignments a
		JOIN tickets tk ON tk.id = a.ticket_id
		WHERE a.task_id = $1`, taskID)
	a, tk, err := scanCandidateRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assignment{}, Ticket{}, false, nil
	}
	if err != nil {
		return Assignment{}, Ticket{}, false, err
	}
	return a, tk, true, nil
}

// DispatchCandidate is one pending assignment on a live (non-terminal)
// ticket: the dispatch scan's work item.
type DispatchCandidate struct {
	Assignment Assignment
	Ticket     Ticket
}

// ReconcileCandidate is one running assignment holding a dispatched task on a
// live ticket: the Pass beat's reconcile work item.
type ReconcileCandidate struct {
	Assignment Assignment
	Ticket     Ticket
}

// FollowupCandidate is one running ticket whose assignments all ended, the
// latest under a progress report: the follow-up scan's work item.
type FollowupCandidate struct {
	TicketID    string
	Followups   int       // followup-kind assignments already spent
	LastEndedAt time.Time // when the last assignment settled
}

// PendingDispatches returns the pending assignments bound to live tickets,
// oldest first. Failed and ended tickets are never dispatched: the dead
// state is terminal (docs/designs/0001 §8).
func (s *Store) PendingDispatches(ctx context.Context) ([]DispatchCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+assignmentJoinColumns+`,
		       tk.id, tk.thread_source, tk.thread_key, tk.mandate, tk.status, tk.session_id,
		       tk.trigger_source, tk.trigger_deterministic_id, tk.created_at
		FROM assignments a
		JOIN tickets tk ON tk.id = a.ticket_id
		WHERE a.status = $1 AND tk.status NOT IN ($2, $3)
		ORDER BY a.created_at, a.id`,
		AssignmentStatusPending, TicketStatusFailed, TicketStatusEnded)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending dispatches: %w", err)
	}
	return collectCandidates(rows, scanDispatchCandidate)
}

// RunningAssignmentCandidates returns the running assignments that hold a
// dispatched task on a live ticket, oldest first — the reconcile scan's
// universe (docs/designs/0001 §4.3: 对账 running Assignment 与 task 真实终态).
func (s *Store) RunningAssignmentCandidates(ctx context.Context) ([]ReconcileCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+assignmentJoinColumns+`,
		       tk.id, tk.thread_source, tk.thread_key, tk.mandate, tk.status, tk.session_id,
		       tk.trigger_source, tk.trigger_deterministic_id, tk.created_at
		FROM assignments a
		JOIN tickets tk ON tk.id = a.ticket_id
		WHERE a.status = $1 AND a.task_id IS NOT NULL AND tk.status NOT IN ($2, $3)
		ORDER BY a.created_at, a.id`,
		AssignmentStatusRunning, TicketStatusFailed, TicketStatusEnded)
	if err != nil {
		return nil, fmt.Errorf("failed to list reconcile candidates: %w", err)
	}
	return collectCandidates(rows, scanReconcileCandidate)
}

// FollowupCandidates returns the live running tickets whose assignments all
// ended, the latest under a progress report — the continuation queue. The
// follow-up count lets the caller cap continuations per ticket.
func (s *Store) FollowupCandidates(ctx context.Context) ([]FollowupCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tk.id,
		       COALESCE((SELECT COUNT(*) FROM assignments f
		                 WHERE f.ticket_id = tk.id AND f.kind = $1), 0),
		       COALESCE((SELECT MAX(e.updated_at) FROM assignments e
		                 WHERE e.ticket_id = tk.id AND e.status = $2), tk.created_at)
		FROM tickets tk
		WHERE tk.status = $3
		  AND NOT EXISTS (SELECT 1 FROM assignments a
		                  WHERE a.ticket_id = tk.id AND a.status IN ($4, $5))
		  AND EXISTS (SELECT 1 FROM assignments p
		              WHERE p.ticket_id = tk.id AND p.status = $2 AND p.report = $6)
		ORDER BY tk.created_at, tk.id`,
		AssignmentKindFollowup, AssignmentStatusEnded, TicketStatusRunning,
		AssignmentStatusPending, AssignmentStatusRunning, ReportProgress)
	if err != nil {
		return nil, fmt.Errorf("failed to list followup candidates: %w", err)
	}
	defer rows.Close()

	var out []FollowupCandidate
	for rows.Next() {
		var c FollowupCandidate
		if err := rows.Scan(&c.TicketID, &c.Followups, &c.LastEndedAt); err != nil {
			return nil, fmt.Errorf("failed to scan followup candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func collectCandidates[T any](rows pgx.Rows, scan func(rows rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanCandidateRow decodes one assignment⋈ticket row: pgx requires every
// column of the result in a single Scan, so the pair scans together.
func scanCandidateRow(rows rowScanner) (Assignment, Ticket, error) {
	var (
		a   Assignment
		tk  Ticket
		man []byte
		sid *string
	)
	if err := rows.Scan(&a.ID, &a.TicketID, &a.TaskID, &a.Status, &a.Report,
		&a.DispatchAttempts, &a.LastError, &a.Kind, &a.StartedDeadline, &a.CreatedAt, &a.UpdatedAt,
		&tk.ID, &tk.ThreadSource, &tk.ThreadKey, &man, &tk.Status, &sid,
		&tk.TriggerSource, &tk.TriggerDeterministicID, &tk.CreatedAt); err != nil {
		return Assignment{}, Ticket{}, fmt.Errorf("failed to scan candidate row: %w", err)
	}
	if sid != nil {
		tk.SessionID = *sid
	}
	if err := json.Unmarshal(man, &tk.Mandate); err != nil {
		return Assignment{}, Ticket{}, fmt.Errorf("ticket %s carries a mandate that does not decode: %w", tk.ID, err)
	}
	return a, tk, nil
}

func scanDispatchCandidate(rows rowScanner) (DispatchCandidate, error) {
	a, tk, err := scanCandidateRow(rows)
	if err != nil {
		return DispatchCandidate{}, err
	}
	return DispatchCandidate{Assignment: a, Ticket: tk}, nil
}

func scanReconcileCandidate(rows rowScanner) (ReconcileCandidate, error) {
	a, tk, err := scanCandidateRow(rows)
	if err != nil {
		return ReconcileCandidate{}, err
	}
	return ReconcileCandidate{Assignment: a, Ticket: tk}, nil
}

// MarkAssignmentDispatched records the landed dispatch atomically
// (docs/designs/0001 §6): the assignment takes its task id and runs, and the
// ticket moves pending→running with the task's A2A contextId as its session
// continuity key. The contextId is never overwritten — every later dispatch
// of the ticket continues the same session. startDeadline (G7) is the time
// by which the task must have produced an outcome; a zero time stores none.
// An assignment that is no longer pending is refused with
// ErrAssignmentNotPending, so a racing retry cannot bind a second task.
func (s *Store) MarkAssignmentDispatched(ctx context.Context, assignmentID, ticketID, taskID, contextID string, startDeadline time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin dispatch transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var deadline any
	if !startDeadline.IsZero() {
		deadline = startDeadline
	}
	tag, err := tx.Exec(ctx, `
		UPDATE assignments
		SET task_id = $2, status = $3, started_deadline = $5, updated_at = now()
		WHERE id = $1 AND status = $4`,
		assignmentID, taskID, AssignmentStatusRunning, AssignmentStatusPending, deadline)
	if err != nil {
		return fmt.Errorf("failed to mark assignment %s dispatched: %w", assignmentID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAssignmentNotPending
	}

	// The ticket runs once dispatched; a ticket that already ended or failed
	// must not resurrect through a dispatch.
	if _, err := tx.Exec(ctx, `
		UPDATE tickets
		SET status = CASE WHEN status = $2 THEN $3 ELSE status END,
		    session_id = COALESCE(NULLIF($4, ''), session_id),
		    updated_at = now()
		WHERE id = $1 AND status NOT IN ($5, $6)`,
		ticketID, TicketStatusPending, TicketStatusRunning, contextID,
		TicketStatusFailed, TicketStatusEnded); err != nil {
		return fmt.Errorf("failed to run ticket %s: %w", ticketID, err)
	}

	return tx.Commit(ctx)
}

// RecordDispatchFailure records one failed dispatch attempt: the ledger
// (attempt counter + error) and the audit row land together, and the
// assignment stays pending for the next Pass tick. The retry past the first
// failure carries the recovery annotation (docs/designs/0001 §4.3: 恢复注记).
func (s *Store) RecordDispatchFailure(ctx context.Context, assignmentID, errText string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin dispatch-failure transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ticketID string
	if err := tx.QueryRow(ctx, `
		UPDATE assignments
		SET dispatch_attempts = dispatch_attempts + 1,
		    last_error = $2,
		    updated_at = now()
		WHERE id = $1
		RETURNING ticket_id`, assignmentID, errText).Scan(&ticketID); err != nil {
		return fmt.Errorf("failed to record dispatch failure of %s: %w", assignmentID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'dispatch.failed', $2)`,
		ticketID, marshalDetail(map[string]any{
			"assignment_id": assignmentID,
			"error":         errText,
		})); err != nil {
		return fmt.Errorf("failed to record dispatch-failure audit for %s: %w", ticketID, err)
	}

	return tx.Commit(ctx)
}

// TerminalOutcome reports what ApplyAssignmentTerminal did.
type TerminalOutcome struct {
	// Applied is true when this call performed the assignment transition.
	Applied bool
	// TicketChanged is true when the ticket status moved in this call.
	TicketChanged bool
}

// TerminalPlan is one terminal outcome to persist atomically: the
// assignment's settle state, the optional ticket transition, and the audit
// row — all in one transaction (docs/designs/0001 §6: persist transitions
// atomically). A plan without TicketStatus settles the assignment only.
type TerminalPlan struct {
	AssignmentID     string
	AssignmentStatus string // ended | unresolved
	ReportState      string // the report the assignment settled under
	TicketID         string
	TicketStatus     string // ended | failed | needs-attention | "" (no change)
	Event            string
	Detail           map[string]any
}

// ApplyAssignmentTerminal settles one running assignment idempotently: the
// transition applies exactly once (the running→settled guard absorbs the
// push-then-reconcile double delivery), the audit row lands if and only if
// the transition did, and a terminal ticket (ended/failed) is never
// resurrected. Replaying a settled plan is a no-op that reports Applied=false.
func (s *Store) ApplyAssignmentTerminal(ctx context.Context, plan TerminalPlan) (TerminalOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TerminalOutcome{}, fmt.Errorf("failed to begin terminal transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE assignments
		SET status = $2, report = $3, updated_at = now()
		WHERE id = $1 AND status = $4`,
		plan.AssignmentID, plan.AssignmentStatus, plan.ReportState, AssignmentStatusRunning)
	if err != nil {
		return TerminalOutcome{}, fmt.Errorf("failed to settle assignment %s: %w", plan.AssignmentID, err)
	}
	if tag.RowsAffected() == 0 {
		// Already settled (replay) — or never ran, which is malformed: a
		// terminal outcome for a task that was never dispatched cannot exist.
		var status string
		if err := tx.QueryRow(ctx,
			"SELECT status FROM assignments WHERE id = $1", plan.AssignmentID).Scan(&status); err != nil {
			return TerminalOutcome{}, fmt.Errorf("failed to read assignment %s: %w", plan.AssignmentID, err)
		}
		if status != AssignmentStatusEnded && status != AssignmentStatusUnresolved {
			return TerminalOutcome{}, fmt.Errorf("%w: %s is %s", ErrAssignmentNotRunning, plan.AssignmentID, status)
		}
		return TerminalOutcome{}, tx.Commit(ctx)
	}

	var ticketChanged bool
	if plan.TicketStatus != "" {
		// The ticket moves only from a live state; ended/failed never
		// resurrect through a late report.
		ticketTag, err := tx.Exec(ctx, `
			UPDATE tickets
			SET status = $2, updated_at = now()
			WHERE id = $1 AND status NOT IN ($3, $4)`,
			plan.TicketID, plan.TicketStatus, TicketStatusFailed, TicketStatusEnded)
		if err != nil {
			return TerminalOutcome{}, fmt.Errorf("failed to transition ticket %s to %s: %w",
				plan.TicketID, plan.TicketStatus, err)
		}
		ticketChanged = ticketTag.RowsAffected() == 1
	}

	if plan.Event != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit (ticket_id, event, detail)
			VALUES ($1, $2, $3)`,
			plan.TicketID, plan.Event, marshalDetail(plan.Detail)); err != nil {
			return TerminalOutcome{}, fmt.Errorf("failed to record %s audit for %s: %w",
				plan.Event, plan.TicketID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return TerminalOutcome{}, fmt.Errorf("failed to commit terminal transition of %s: %w", plan.AssignmentID, err)
	}
	return TerminalOutcome{Applied: true, TicketChanged: ticketChanged}, nil
}

// MarkTicketNeedsAttention moves a live ticket to needs-attention
// (docs/designs/0001 §8: unresolved/blocked work waits for a person). The
// reserved input-required detection point (docs/designs/0002 §3) and the
// red-line unresolved path both land here. The transition and its audit row
// commit together; an already-attending ticket is not re-audited. The
// assignment keeps running — the task itself stays alive awaiting the
// person's answer.
func (s *Store) MarkTicketNeedsAttention(ctx context.Context, ticketID, reason string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to begin needs-attention transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tickets
		SET status = $2, updated_at = now()
		WHERE id = $1 AND status NOT IN ($3, $4, $2)`,
		ticketID, TicketStatusNeedsAttention, TicketStatusFailed, TicketStatusEnded)
	if err != nil {
		return false, fmt.Errorf("failed to mark ticket %s needs-attention: %w", ticketID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'ticket.needs_attention', $2)`,
		ticketID, marshalDetail(map[string]any{"reason": reason})); err != nil {
		return false, fmt.Errorf("failed to record needs-attention audit for %s: %w", ticketID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("failed to commit needs-attention of %s: %w", ticketID, err)
	}
	return true, nil
}

// CreateFollowupAssignment mints the continuation assignment of a running
// ticket (docs/designs/0001 §4.3: 到期/重派入口). The ticket must be running:
// follow-ups continue live work only.
func (s *Store) CreateFollowupAssignment(ctx context.Context, ticketID string) (Assignment, error) {
	id, err := newAssignmentID()
	if err != nil {
		return Assignment{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO assignments (id, ticket_id, status, kind)
		SELECT $1, id, $2, $3 FROM tickets WHERE id = $4 AND status = $5`,
		id, AssignmentStatusPending, AssignmentKindFollowup, ticketID, TicketStatusRunning)
	if err != nil {
		return Assignment{}, fmt.Errorf("failed to insert followup assignment for ticket %s: %w", ticketID, err)
	}
	if tag.RowsAffected() == 0 {
		return Assignment{}, fmt.Errorf("ticket %s is not running; no follow-up assignment minted", ticketID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+assignmentColumns+`
		FROM assignments WHERE id = $1`, id)
	if err != nil {
		return Assignment{}, fmt.Errorf("failed to read back followup assignment %s: %w", id, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Assignment{}, fmt.Errorf("followup assignment %s vanished after insert", id)
	}
	return scanAssignment(rows)
}

// newAssignmentID mints a collision-resistant assignment id for
// non-trigger-derived assignments (follow-ups).
func newAssignmentID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("failed to mint assignment id: %w", err)
	}
	return "assign-" + hex.EncodeToString(b[:]), nil
}

// marshalDetail encodes an audit detail map; the audit table rejects nothing
// JSON, so a failure to encode is a programming error surfaced loudly.
func marshalDetail(detail map[string]any) []byte {
	if detail == nil {
		detail = map[string]any{}
	}
	blob, err := json.Marshal(detail)
	if err != nil {
		// A detail map that cannot encode is a bug; persist the fact.
		return []byte(fmt.Sprintf(`{"encode_error":%q}`, err.Error()))
	}
	return blob
}
