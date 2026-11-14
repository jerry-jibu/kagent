package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

func TestRecordDecision(t *testing.T) {
	t.Run("Given a migrated store, when a validated decision is recorded for a trigger, assert that it round-trips with its outcome, gate-verified strength, policy version and reason", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		rec, err := s.RecordDecision(ctx, "github:acme/api", "issue-comment:1001", store.DecisionInput{
			Outcome:       store.DecisionAccept,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "authorized request to fix the flaky test",
		})
		if err != nil {
			t.Fatalf("RecordDecision failed: %v", err)
		}
		if rec.ID == 0 {
			t.Fatalf("decision id must be assigned, got %+v", rec)
		}

		decisions, err := s.Decisions(ctx, "github:acme/api")
		if err != nil {
			t.Fatalf("Decisions failed: %v", err)
		}
		if len(decisions) != 1 {
			t.Fatalf("decisions = %d rows, want 1", len(decisions))
		}
		got := decisions[0]
		if got.Outcome != store.DecisionAccept ||
			got.Strength != "explicit" ||
			got.PolicyVersion != "abc123" ||
			got.Reason != "authorized request to fix the flaky test" ||
			got.Source != "github:acme/api" ||
			got.DeterministicID != "issue-comment:1001" {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
		if got.CreatedAt.IsZero() {
			t.Fatalf("created_at must be persisted, got %+v", got)
		}
	})

	t.Run("Given a trigger that already has an accept decision, when a second accept is recorded for it, assert that the store refuses the duplicate so Ticket creation can never double-fire", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		in := store.DecisionInput{Outcome: store.DecisionAccept, Strength: "explicit", PolicyVersion: "abc123"}
		if _, err := s.RecordDecision(ctx, "github:acme/api", "issue-comment:2001", in); err != nil {
			t.Fatalf("first accept failed: %v", err)
		}
		if _, err := s.RecordDecision(ctx, "github:acme/api", "issue-comment:2001", in); err == nil {
			t.Fatalf("second accept for the same trigger must be refused")
		}
	})

	t.Run("Given a trigger whose evaluation failed once, when a terminal decision is recorded after the unavailable one, assert that both rows persist and TriggerDecision returns the newest", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		if _, err := s.RecordDecision(ctx, "github:acme/api", "issue-comment:3001", store.DecisionInput{
			Outcome:       store.DecisionUnavailable,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "evaluator unreachable",
		}); err != nil {
			t.Fatalf("unavailable record failed: %v", err)
		}
		if _, err := s.RecordDecision(ctx, "github:acme/api", "issue-comment:3001", store.DecisionInput{
			Outcome:       store.DecisionAccept,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "accepted on retry",
		}); err != nil {
			t.Fatalf("accept-after-retry failed: %v", err)
		}

		decisions, err := s.Decisions(ctx, "github:acme/api")
		if err != nil {
			t.Fatalf("Decisions failed: %v", err)
		}
		if len(decisions) != 2 {
			t.Fatalf("decisions = %d rows, want the unavailable row and the accept row", len(decisions))
		}

		latest, ok, err := s.TriggerDecision(ctx, "github:acme/api", "issue-comment:3001")
		if err != nil || !ok {
			t.Fatalf("TriggerDecision failed: ok=%v err=%v", ok, err)
		}
		if latest.Outcome != store.DecisionAccept {
			t.Fatalf("latest decision outcome = %q, want accept", latest.Outcome)
		}
	})

	t.Run("Given store-level record validation, when an input would create an unrepresentable decision, assert that RecordDecision rejects it", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		scenarios := []struct {
			name string
			in   store.DecisionInput
			want string
		}{
			{"outcome outside the vocabulary", store.DecisionInput{Outcome: "maybe", PolicyVersion: "abc123"}, "outcome"},
			{"empty policy version", store.DecisionInput{Outcome: store.DecisionIgnore, Strength: "explicit"}, "policy version"},
			{"accept without a strength", store.DecisionInput{Outcome: store.DecisionAccept, PolicyVersion: "abc123"}, "strength"},
			{"accept with an unknown strength", store.DecisionInput{Outcome: store.DecisionAccept, Strength: "granted", PolicyVersion: "abc123"}, "strength"},
			{"gate denial carrying a strength", store.DecisionInput{Outcome: store.DecisionDenied, Strength: "explicit", PolicyVersion: "abc123"}, "strength"},
		}
		for _, sc := range scenarios {
			t.Run("Given "+sc.name+", when recorded, assert that the store refuses it", func(t *testing.T) {
				_, err := s.RecordDecision(ctx, "github:acme/api", "issue-comment:4001", sc.in)
				if err == nil {
					t.Fatalf("input %+v must be refused", sc.in)
				}
				if !strings.Contains(err.Error(), sc.want) {
					t.Fatalf("error %q must mention %q", err, sc.want)
				}
			})
		}
	})

	t.Run("Given a trigger with no decisions, when TriggerDecision runs, assert that it reports not found", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		_, ok, err := s.TriggerDecision(ctx, "github:acme/api", "issue-comment:5001")
		if err != nil {
			t.Fatalf("TriggerDecision failed: %v", err)
		}
		if ok {
			t.Fatalf("unknown trigger must report no decision")
		}
	})
}
