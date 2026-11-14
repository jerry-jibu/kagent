package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The minimal admin surface (docs/designs 0001 §3: waitsForPerson/
// followupOwners 管理入口; T8 issue #9): list the needs-attention tickets,
// retry a held ticket, stop live work. Every mutating action is a single
// Engine-style transaction — the admin is a writer like any other, and the
// guards below make the states it must not produce unrepresentable.

// ErrTicketNotHeld is returned by RetryTicket when the ticket is not sitting
// in needs-attention: a retry is the person's answer to a hold, not a
// general-purpose re-dispatch of live work.
var ErrTicketNotHeld = errors.New("ticket is not in needs-attention")

// ErrTicketTerminal is returned by StopTicket when the ticket already settled
// (failed/ended): there is no live work left to stop.
var ErrTicketTerminal = errors.New("ticket is already terminal")

// AttentionTicket is one needs-attention ticket with the context a person
// needs to decide (the admin list row, docs/designs 0001 §3 Attention).
type AttentionTicket struct {
	Ticket           Ticket
	LatestAssignment Assignment // the ticket's latest assignment (zero value when none)
	WaitsForPerson   bool       // the waitsForPerson predicate holds for the ticket
	LastEvent        string     // latest audit event name ("" when none)
	LastEventAt      time.Time
}

// NeedsAttentionTickets returns every ticket sitting in needs-attention with
// its latest assignment, waitsForPerson flag and last audit event, oldest
// first — the admin list. The waitsForPerson arms mirror
// WaitsForPersonTickets ($1..$6 are the same vocabulary arguments).
func (s *Store) NeedsAttentionTickets(ctx context.Context) ([]AttentionTicket, error) {
	rows, err := s.pool.Query(ctx, latestAssignmentCTE+`
	SELECT tk.id, tk.thread_source, tk.thread_key, tk.mandate, tk.status, tk.session_id,
	       tk.trigger_source, tk.trigger_deterministic_id, tk.created_at,
	       COALESCE(l.assignment_id, ''), COALESCE(l.status, ''), COALESCE(l.report, ''),
	       COALESCE(l.settled_at, tk.created_at),
	       l.status IS NOT NULL AND
	         (l.status = $1 OR (l.status = $2 AND l.report IN ($3, $4, $5, $6))),
	       COALESCE((SELECT ev.event FROM audit ev WHERE ev.ticket_id = tk.id
	                 ORDER BY ev.id DESC LIMIT 1), ''),
	       COALESCE((SELECT ev.created_at FROM audit ev WHERE ev.ticket_id = tk.id
	                 ORDER BY ev.id DESC LIMIT 1), tk.created_at)
	FROM tickets tk
	LEFT JOIN latest l ON l.ticket_id = tk.id
	WHERE tk.status = $7
	ORDER BY tk.created_at, tk.id`,
		AssignmentStatusUnresolved, AssignmentStatusEnded,
		"blocked", "failed", "waiting", "dropped",
		TicketStatusNeedsAttention)
	if err != nil {
		return nil, fmt.Errorf("failed to list needs-attention tickets: %w", err)
	}
	defer rows.Close()

	var out []AttentionTicket
	for rows.Next() {
		var (
			row      AttentionTicket
			man      []byte
			sid      *string
			asStatus string
			asReport string
		)
		if err := rows.Scan(&row.Ticket.ID, &row.Ticket.ThreadSource, &row.Ticket.ThreadKey,
			&man, &row.Ticket.Status, &sid,
			&row.Ticket.TriggerSource, &row.Ticket.TriggerDeterministicID, &row.Ticket.CreatedAt,
			&row.LatestAssignment.ID, &asStatus, &asReport, &row.LatestAssignment.UpdatedAt,
			&row.WaitsForPerson, &row.LastEvent, &row.LastEventAt); err != nil {
			return nil, fmt.Errorf("failed to scan attention ticket: %w", err)
		}
		if sid != nil {
			row.Ticket.SessionID = *sid
		}
		if err := json.Unmarshal(man, &row.Ticket.Mandate); err != nil {
			return nil, fmt.Errorf("ticket %s carries a mandate that does not decode: %w", row.Ticket.ID, err)
		}
		row.LatestAssignment.Status = asStatus
		row.LatestAssignment.Report = asReport
		row.LatestAssignment.TicketID = row.Ticket.ID
		out = append(out, row)
	}
	return out, rows.Err()
}

// RetryTicket applies the person's retry decision atomically (T8 issue #9):
// the held ticket runs again, a fresh follow-up assignment is minted for the
// Pass to dispatch, and the action is audited — all in one transaction. The
// needs-attention guard makes a double-click replay a refusal, never a
// second assignment.
func (s *Store) RetryTicket(ctx context.Context, ticketID, actor string) (Assignment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Assignment{}, fmt.Errorf("failed to begin retry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tickets
		SET status = $2, updated_at = now()
		WHERE id = $1 AND status = $3`,
		ticketID, TicketStatusRunning, TicketStatusNeedsAttention)
	if err != nil {
		return Assignment{}, fmt.Errorf("failed to resume ticket %s: %w", ticketID, err)
	}
	if tag.RowsAffected() == 0 {
		var status string
		err := tx.QueryRow(ctx,
			"SELECT status FROM tickets WHERE id = $1", ticketID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return Assignment{}, ErrTicketNotFound
		}
		if err != nil {
			return Assignment{}, fmt.Errorf("failed to read ticket %s: %w", ticketID, err)
		}
		return Assignment{}, fmt.Errorf("%w (ticket is %s)", ErrTicketNotHeld, status)
	}

	assignmentID, err := newAssignmentID()
	if err != nil {
		return Assignment{}, err
	}
	var minted Assignment
	err = tx.QueryRow(ctx, `
		INSERT INTO assignments (id, ticket_id, status, kind)
		VALUES ($1, $2, $3, $4)
		RETURNING `+assignmentColumns,
		assignmentID, ticketID, AssignmentStatusPending, AssignmentKindFollowup).
		Scan(&minted.ID, &minted.TicketID, &minted.TaskID, &minted.Status, &minted.Report,
			&minted.DispatchAttempts, &minted.LastError, &minted.Kind, &minted.StartedDeadline,
			&minted.CreatedAt, &minted.UpdatedAt)
	if err != nil {
		return Assignment{}, fmt.Errorf("failed to mint the retry assignment of %s: %w", ticketID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'admin.retry', $2)`,
		ticketID, marshalDetail(map[string]any{
			"actor":         actor,
			"assignment_id": assignmentID,
		})); err != nil {
		return Assignment{}, fmt.Errorf("failed to audit the retry of %s: %w", ticketID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Assignment{}, fmt.Errorf("failed to commit the retry of %s: %w", ticketID, err)
	}
	return minted, nil
}

// StopTicket applies the person's stop decision atomically: the ticket leaves
// play as failed, every non-terminal assignment settles ended under a dropped
// report (the person's cancellation — the task may still finish at the
// gateway, and its late terminal report is absorbed by the settled guards and
// the ticket's no-resurrection guard), and the action is audited — one
// transaction. A2A carries no cancel in P0: stopping marks, it does not kill
// the remote task.
func (s *Store) StopTicket(ctx context.Context, ticketID, actor string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin stop transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tickets
		SET status = $2, updated_at = now()
		WHERE id = $1 AND status NOT IN ($2, $3)`,
		ticketID, TicketStatusFailed, TicketStatusEnded)
	if err != nil {
		return fmt.Errorf("failed to stop ticket %s: %w", ticketID, err)
	}
	if tag.RowsAffected() == 0 {
		var status string
		err := tx.QueryRow(ctx,
			"SELECT status FROM tickets WHERE id = $1", ticketID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return fmt.Errorf("failed to read ticket %s: %w", ticketID, err)
		}
		return fmt.Errorf("%w (ticket is %s)", ErrTicketTerminal, status)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE assignments
		SET status = $2, report = $3, updated_at = now()
		WHERE ticket_id = $1 AND status IN ($4, $5)`,
		ticketID, AssignmentStatusEnded, "dropped",
		AssignmentStatusPending, AssignmentStatusRunning); err != nil {
		return fmt.Errorf("failed to settle the live assignments of %s: %w", ticketID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'admin.stop', $2)`,
		ticketID, marshalDetail(map[string]any{"actor": actor})); err != nil {
		return fmt.Errorf("failed to audit the stop of %s: %w", ticketID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit the stop of %s: %w", ticketID, err)
	}
	return nil
}

// RecordFollowupInput records a person's message on an owned thread as the
// owner ticket's follow-up input together with the terminal decision that
// keeps the trigger from re-evaluating — one transaction (docs/designs 0001
// §4.2: 该 Thread 有活 Ticket → 订阅扇出免评估; P0 has no fan-out channel, so
// the message is recorded on the owner and rides along on its next recovery
// dispatch). The owner must exist: an input for a vanished ticket is
// malformed input, refused loudly.
func (s *Store) RecordFollowupInput(ctx context.Context, ownerTicketID, source, deterministicID string, in DecisionInput) (Decision, error) {
	if err := in.validate(); err != nil {
		return Decision{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Decision{}, fmt.Errorf("failed to begin followup-input transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM tickets WHERE id = $1)", ownerTicketID).Scan(&exists); err != nil {
		return Decision{}, fmt.Errorf("failed to check owner %s: %w", ownerTicketID, err)
	}
	if !exists {
		return Decision{}, fmt.Errorf("%w: followup owner %s", ErrTicketNotFound, ownerTicketID)
	}

	var d Decision
	if err := tx.QueryRow(ctx, `
		INSERT INTO decisions (source, deterministic_id, outcome, strength, policy_version, reason)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		source, deterministicID, in.Outcome, in.Strength, in.PolicyVersion, in.Reason,
	).Scan(&d.ID, &d.CreatedAt); err != nil {
		return Decision{}, fmt.Errorf("failed to record the followup-input decision for trigger %s/%s: %w",
			source, deterministicID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'ticket.followup_input', $2)`,
		ownerTicketID, marshalDetail(map[string]any{
			"source":           source,
			"deterministic_id": deterministicID,
			"decision":         in.Outcome,
			"reason":           in.Reason,
		})); err != nil {
		return Decision{}, fmt.Errorf("failed to audit the followup input of %s: %w", ownerTicketID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Decision{}, fmt.Errorf("failed to commit the followup input of %s: %w", ownerTicketID, err)
	}

	d.Source = source
	d.DeterministicID = deterministicID
	d.Outcome = in.Outcome
	d.Strength = in.Strength
	d.PolicyVersion = in.PolicyVersion
	d.Reason = in.Reason
	return d, nil
}
