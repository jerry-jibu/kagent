package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// seedEffect commits one ticket carrying a single confirmation effect, so the
// outbox tests always run against the real enqueue path.
func seedEffect(t *testing.T, s *store.Store, source, deterministicID, key string) {
	t.Helper()
	in := acceptInput(source, deterministicID)
	in.Effects[0].IdempotencyKey = key
	if _, err := s.CreateTicket(context.Background(), in); err != nil {
		t.Fatalf("failed to seed effect: %v", err)
	}
}

func TestOutboxEffects(t *testing.T) {
	ctx := context.Background()
	source := "github:acme/api"

	t.Run("Given a committed pending effect, when it is marked delivered with a receipt, assert that it leaves the pending scan and is audit-visible as delivered with the receipt and delivery time", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		seedEffect(t, s, source, "issue-comment:1001", "confirm:key-1")

		pending, err := s.PendingEffects(ctx)
		if err != nil {
			t.Fatalf("PendingEffects failed: %v", err)
		}
		if len(pending) != 1 || pending[0].State != store.EffectPending {
			t.Fatalf("pending effects = %+v, want exactly the pending one", pending)
		}
		if pending[0].IdempotencyKey != "confirm:key-1" {
			t.Fatalf("effect key = %q, want confirm:key-1", pending[0].IdempotencyKey)
		}

		if err := s.MarkEffectDelivered(ctx, pending[0].ID, []byte(`{"comment_id":777}`)); err != nil {
			t.Fatalf("MarkEffectDelivered failed: %v", err)
		}

		stillPending, err := s.PendingEffects(ctx)
		if err != nil {
			t.Fatalf("PendingEffects failed: %v", err)
		}
		if len(stillPending) != 0 {
			t.Fatalf("pending effects = %+v, want none after delivery", stillPending)
		}

		all, err := s.Effects(ctx)
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		if len(all) != 1 || all[0].State != store.EffectDelivered {
			t.Fatalf("effects = %+v, want the delivered one", all)
		}
		// The receipt round-trips through JSONB, so compare the decoded
		// content, not the serialized bytes.
		var receipt map[string]any
		if err := json.Unmarshal(all[0].Receipt, &receipt); err != nil {
			t.Fatalf("receipt %s is not JSON: %v", all[0].Receipt, err)
		}
		if receipt["comment_id"] != float64(777) {
			t.Fatalf("receipt = %v, want the recorded comment id", receipt)
		}
		if all[0].DeliveredAt == nil || time.Since(*all[0].DeliveredAt) > time.Minute {
			t.Fatalf("delivered_at must be recorded at delivery, got %+v", all[0].DeliveredAt)
		}
	})

	t.Run("Given an effect already delivered, when it is marked delivered again, assert that the transition is refused as one-way", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		seedEffect(t, s, source, "issue-comment:1001", "confirm:key-1")

		pending, err := s.PendingEffects(ctx)
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending effects = %+v err=%v", pending, err)
		}
		if err := s.MarkEffectDelivered(ctx, pending[0].ID, []byte(`{}`)); err != nil {
			t.Fatalf("first mark failed: %v", err)
		}
		if err := s.MarkEffectDelivered(ctx, pending[0].ID, []byte(`{"comment_id":1}`)); !errors.Is(err, store.ErrEffectNotPending) {
			t.Fatalf("second mark error = %v, want ErrEffectNotPending", err)
		}
	})

	t.Run("Given an unknown effect id, when it is marked delivered, assert that the store refuses it", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		if err := s.MarkEffectDelivered(ctx, 4242, []byte(`{}`)); !errors.Is(err, store.ErrEffectNotPending) {
			t.Fatalf("unknown effect error = %v, want ErrEffectNotPending", err)
		}
	})

	t.Run("Given a receipt that is not JSON, when an effect is marked delivered with it, assert that the receipt is refused", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		seedEffect(t, s, source, "issue-comment:1001", "confirm:key-1")

		pending, err := s.PendingEffects(ctx)
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending effects = %+v err=%v", pending, err)
		}
		if err := s.MarkEffectDelivered(ctx, pending[0].ID, []byte("not json")); err == nil {
			t.Fatalf("a non-JSON receipt must be refused")
		}
		if rows, err := s.PendingEffects(ctx); err != nil || len(rows) != 1 {
			t.Fatalf("pending effects = %+v err=%v, want the effect still pending", rows, err)
		}
	})
}
