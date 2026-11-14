package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// Delivery states of a ticket's result (migration 0007_delivery). Only a
// ready report makes a result deliverable; ” means there is nothing to
// deliver and the delivery scan never sees the ticket.
const (
	DeliveryNone      = ""
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// DeliveryCandidate is one ticket whose result is owed a PR and a result
// comment: the delivery scan's work item. BaseBranch/BaseCommit are the
// materialization baseline (0003 路线 A) — the deliverer re-clones this exact
// commit so the workspace diff never includes upstream drift. BaseSkipped
// lists the paths materialization could not transport (binary, oversized,
// symlink): they are excluded from the deletion set, since their absence from
// the workspace means "never materialized", not "worker deleted".
type DeliveryCandidate struct {
	Ticket      Ticket
	BaseBranch  string
	BaseCommit  string
	BaseSkipped []string
}

// PendingDeliveries returns the tickets with a pending result, creation order
// first. Delivered and failed deliveries are terminal: they never re-enter
// the scan.
func (s *Store) PendingDeliveries(ctx context.Context) ([]DeliveryCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, thread_source, thread_key, mandate, status, session_id,
		       trigger_source, trigger_deterministic_id, created_at,
		       base_branch, base_commit, base_skipped
		FROM tickets
		WHERE delivery_state = $1
		ORDER BY created_at, id`, DeliveryPending)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending deliveries: %w", err)
	}
	defer rows.Close()

	var out []DeliveryCandidate
	for rows.Next() {
		var c DeliveryCandidate
		var tk Ticket
		var mandate, skipped []byte
		var sessionID *string
		if err := rows.Scan(&tk.ID, &tk.ThreadSource, &tk.ThreadKey, &mandate, &tk.Status, &sessionID,
			&tk.TriggerSource, &tk.TriggerDeterministicID, &tk.CreatedAt,
			&c.BaseBranch, &c.BaseCommit, &skipped); err != nil {
			return nil, fmt.Errorf("failed to scan delivery candidate: %w", err)
		}
		if sessionID != nil {
			tk.SessionID = *sessionID
		}
		if err := json.Unmarshal(mandate, &tk.Mandate); err != nil {
			return nil, fmt.Errorf("ticket %s carries a mandate that does not decode: %w", tk.ID, err)
		}
		if err := json.Unmarshal(skipped, &c.BaseSkipped); err != nil {
			return nil, fmt.Errorf("ticket %s carries a skipped list that does not decode: %w", tk.ID, err)
		}
		c.Ticket = tk
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordTicketBaseline persists the materialization baseline of a ticket's
// workspace: the branch, the exact cloned HEAD, and the paths materialization
// could not transport (the deletion set of the delivery diff excludes them).
// The materializer calls it right after a successful materialization; without
// a baseline the workspace is undeliverable, so a lost write here fails the
// materialization like any other failure (the caller joins it with the
// ticket-failure transition).
func (s *Store) RecordTicketBaseline(ctx context.Context, ticketID, branch, commit string, skipped []string) error {
	if skipped == nil {
		skipped = []string{}
	}
	skippedJSON, err := json.Marshal(skipped)
	if err != nil {
		return fmt.Errorf("failed to encode the skipped list of ticket %s: %w", ticketID, err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE tickets
		SET base_branch = $2, base_commit = $3, base_skipped = $4::jsonb, updated_at = now()
		WHERE id = $1`, ticketID, branch, commit, skippedJSON)
	if err != nil {
		return fmt.Errorf("failed to record the baseline of ticket %s: %w", ticketID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTicketNotFound
	}
	return nil
}

// DeliveredPlan is one completed delivery to persist atomically
// (docs/designs/0001 §6): the delivery state, the queued result-comment
// effect, and the audit row commit together — a crash anywhere leaves either
// the whole delivery or none of it.
type DeliveredPlan struct {
	TicketID string
	PRNumber int
	PRURL    string
	Branch   string
	Commit   string
	Effect   OutboxEffect
	Detail   map[string]any
}

// MarkTicketDelivered records one delivered result: delivery_state
// pending→delivered (one-way guard), the result-comment effect enqueued in
// the same transaction, and the delivery.delivered audit row. A ticket not in
// pending state (a replay after a lost receipt, or a raced scan) reports
// Applied=false and enqueues nothing — the effect's (channel,
// idempotency_key) uniqueness backs the marker idempotency up at the schema
// level.
func (s *Store) MarkTicketDelivered(ctx context.Context, plan DeliveredPlan) (bool, error) {
	if err := plan.validate(); err != nil {
		return false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to begin delivery transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tickets
		SET delivery_state = $2, updated_at = now()
		WHERE id = $1 AND delivery_state = $3`,
		plan.TicketID, DeliveryDelivered, DeliveryPending)
	if err != nil {
		return false, fmt.Errorf("failed to mark ticket %s delivered: %w", plan.TicketID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (channel, address, idempotency_key, payload)
		VALUES ($1, $2, $3, $4)`,
		plan.Effect.Channel, plan.Effect.Address, plan.Effect.IdempotencyKey, plan.Effect.Payload); err != nil {
		return false, fmt.Errorf("failed to enqueue the result-comment effect for ticket %s: %w", plan.TicketID, err)
	}

	// The audit detail describes what actually persisted: the PR identity is
	// store knowledge; the caller may add context on top.
	detail := map[string]any{
		"pr_number": plan.PRNumber,
		"pr_url":    plan.PRURL,
		"branch":    plan.Branch,
		"commit":    plan.Commit,
	}
	for k, v := range plan.Detail {
		detail[k] = v
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'delivery.delivered', $2)`,
		plan.TicketID, marshalDetail(detail)); err != nil {
		return false, fmt.Errorf("failed to record the delivery audit for %s: %w", plan.TicketID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("failed to commit the delivery of %s: %w", plan.TicketID, err)
	}
	return true, nil
}

func (p DeliveredPlan) validate() error {
	switch {
	case p.TicketID == "":
		return fmt.Errorf("delivered plan names no ticket")
	case p.PRURL == "":
		return fmt.Errorf("delivered plan carries no PR url")
	case p.Effect.Channel == "":
		return fmt.Errorf("delivered plan carries an effect with no channel")
	case p.Effect.Address == "":
		return fmt.Errorf("delivered plan carries an effect with no address")
	case p.Effect.IdempotencyKey == "":
		return fmt.Errorf("delivered plan carries an effect with no idempotency key")
	case len(p.Effect.Payload) == 0 || !json.Valid(p.Effect.Payload):
		return fmt.Errorf("delivered plan effect payload is empty or not JSON")
	}
	return nil
}

// FailTicketDelivery fails a pending delivery permanently (patch missing,
// corrupted, or empty — no PR was pushed) and fails the ticket with it, in
// one transaction: the delivery state, the ticket status and the
// delivery.failed audit row commit together. A ticket not in pending state
// (replay) reports Applied=false and writes nothing.
func (s *Store) FailTicketDelivery(ctx context.Context, ticketID, reason string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to begin delivery-failure transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tickets
		SET delivery_state = $2, status = $3, updated_at = now()
		WHERE id = $1 AND delivery_state = $4`,
		ticketID, DeliveryFailed, TicketStatusFailed, DeliveryPending)
	if err != nil {
		return false, fmt.Errorf("failed to fail the delivery of ticket %s: %w", ticketID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit (ticket_id, event, detail)
		VALUES ($1, 'delivery.failed', $2)`,
		ticketID, marshalDetail(map[string]any{"reason": reason})); err != nil {
		return false, fmt.Errorf("failed to record the delivery-failure audit for %s: %w", ticketID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("failed to commit the delivery failure of %s: %w", ticketID, err)
	}
	return true, nil
}
