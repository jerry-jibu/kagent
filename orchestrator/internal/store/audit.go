package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrTicketNotFound is returned when a ticket id has no row.
var ErrTicketNotFound = errors.New("ticket not found")

// AuditEvent is one orchestrator-observed lifecycle record for a ticket.
type AuditEvent struct {
	Event     string
	Detail    map[string]any
	CreatedAt time.Time
}

// FailTicket transitions a ticket to failed and appends the audit record in
// ONE transaction (docs/designs/0001 §6: persist transitions atomically).
// The audit row lands if and only if the transition did — an audit trail
// must never describe a state change that did not happen.
func (s *Store) FailTicket(ctx context.Context, ticketID, reason string) error {
	detail, err := json.Marshal(map[string]any{"reason": reason})
	if err != nil {
		return fmt.Errorf("failed to encode failure detail: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin fail-ticket transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tickets
		SET status = 'failed', updated_at = now()
		WHERE id = $1`, ticketID)
	if err != nil {
		return fmt.Errorf("failed to mark ticket %s failed: %w", ticketID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTicketNotFound
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'ticket.failed', $2)`, ticketID, detail); err != nil {
		return fmt.Errorf("failed to record failure audit for %s: %w", ticketID, err)
	}
	return tx.Commit(ctx)
}

// RecordAudit appends one lifecycle event for the ticket. It does not
// verify the ticket exists: audit rows are observations, and the caller
// (materializer/dispatcher) has already resolved the ticket. The detail is
// encoded verbatim — typed structs at the producer side (STYLE.md: typed
// data over generic maps) or legacy map literals, whichever the caller owns.
func (s *Store) RecordAudit(ctx context.Context, ticketID, event string, detail any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	blob, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("failed to encode audit detail: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, $2, $3)`, ticketID, event, blob); err != nil {
		return fmt.Errorf("failed to record audit event %s for %s: %w", event, ticketID, err)
	}
	return nil
}

// AuditEvents returns the ticket's audit trail in insertion order.
func (s *Store) AuditEvents(ctx context.Context, ticketID string) ([]AuditEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT event, detail, created_at
		FROM audit
		WHERE ticket_id = $1
		ORDER BY id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("failed to read audit trail for %s: %w", ticketID, err)
	}
	defer rows.Close()

	var events []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		var detail []byte
		if err := rows.Scan(&ev.Event, &detail, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan audit event: %w", err)
		}
		if err := json.Unmarshal(detail, &ev.Detail); err != nil {
			return nil, fmt.Errorf("failed to decode audit detail: %w", err)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// TicketStatus returns the ticket's current status.
func (s *Store) TicketStatus(ctx context.Context, ticketID string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		"SELECT status FROM tickets WHERE id = $1", ticketID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTicketNotFound
	}
	if err != nil {
		return "", fmt.Errorf("failed to read status of ticket %s: %w", ticketID, err)
	}
	return status, nil
}
