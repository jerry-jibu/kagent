package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outbox effect states. Effects are enqueued 'pending' inside the state
// transaction and flipped to 'delivered' only after the channel adapter has
// actually delivered them (docs/designs/0001 §6).
const (
	EffectPending   = "pending"
	EffectDelivered = "delivered"
)

// ErrEffectNotPending is returned by MarkEffectDelivered when the effect is
// unknown or no longer pending: a concurrent deliverer won, or the effect was
// already delivered. Either way the delivery must not repeat its side effect.
var ErrEffectNotPending = errors.New("effect is not pending")

// Effect is one queued external side effect with its delivery state.
type Effect struct {
	ID             int64
	Channel        string
	Address        string
	IdempotencyKey string
	Payload        []byte
	Receipt        []byte // channel receipt JSON; nil until delivered
	State          string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

// PendingEffects returns the committed-but-undelivered effects, oldest first.
// P0 runs a single deliverer (single-instance orchestrator), so the scan has
// no claim protocol; MarkEffectDelivered's state guard keeps the
// pending→delivered transition one-way even if that ever changes.
func (s *Store) PendingEffects(ctx context.Context) ([]Effect, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, channel, address, idempotency_key, payload, receipt, state, created_at, delivered_at
		FROM outbox
		WHERE state = $1
		ORDER BY id`, EffectPending)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending effects: %w", err)
	}
	return collectEffects(rows)
}

// Effects returns all outbox rows, enqueue order first. The outbox is audit
// state: "what was owed to the outside world and what came back" is
// answerable from the store alone.
func (s *Store) Effects(ctx context.Context) ([]Effect, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, channel, address, idempotency_key, payload, receipt, state, created_at, delivered_at
		FROM outbox
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to list effects: %w", err)
	}
	return collectEffects(rows)
}

// MarkEffectDelivered records the channel receipt and flips the effect to
// delivered. The transition is one-way: an effect not in pending state
// (already delivered, or unknown) is refused with ErrEffectNotPending.
// Receipt persists what the channel returned, so "did this message actually
// reach GitHub" is answerable after the fact (docs/designs/0001 §6).
func (s *Store) MarkEffectDelivered(ctx context.Context, id int64, receipt []byte) error {
	if receipt == nil {
		receipt = []byte("{}")
	}
	if !json.Valid(receipt) {
		return fmt.Errorf("receipt is not JSON")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox
		SET state = $2, receipt = $3, delivered_at = now()
		WHERE id = $1 AND state = $4`,
		id, EffectDelivered, receipt, EffectPending)
	if err != nil {
		return fmt.Errorf("failed to mark effect %d delivered: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrEffectNotPending
	}
	return nil
}

func collectEffects(rows pgx.Rows) ([]Effect, error) {
	defer rows.Close()
	var out []Effect
	for rows.Next() {
		var e Effect
		if err := rows.Scan(&e.ID, &e.Channel, &e.Address, &e.IdempotencyKey,
			&e.Payload, &e.Receipt, &e.State, &e.CreatedAt, &e.DeliveredAt); err != nil {
			return nil, fmt.Errorf("failed to scan effect: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
