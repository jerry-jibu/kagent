package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// acceptInput builds the canonical CreateTicket input for trigger
// github:acme/api / issue-comment:1001: an accept decision with the
// gate-verified values, the mandate they authorize, and one GitHub
// confirmation-comment effect.
func acceptInput(source, deterministicID string) store.TicketInput {
	return store.TicketInput{
		Source:          source,
		DeterministicID: deterministicID,
		ThreadKey:       "issue:42",
		Decision: store.DecisionInput{
			Outcome:       store.DecisionAccept,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "authorized coding request",
		},
		Mandate: store.Mandate{
			PolicyVersion: "abc123",
			Strength:      "explicit",
			Sender:        "alice",
			Scope:         store.MandateScope{Repo: "acme/api", Issue: 42},
		},
		Effects: []store.OutboxEffect{{
			Channel:        "github",
			Address:        "acme/api#42",
			IdempotencyKey: "confirm:" + store.TicketIDForTrigger(source, deterministicID),
			Payload:        []byte(`{"kind":"github-issue-comment","repo":"acme/api","issue":42,"body":"Acknowledged"}`),
		}},
	}
}

func TestCreateTicket(t *testing.T) {
	ctx := context.Background()
	source := "github:acme/api"

	t.Run("Given an accept decision for an ingested trigger, when the store creates the ticket, assert that decision, ticket, assignment and outbox effect all land in one consistent state", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)

		in := acceptInput(source, "issue-comment:1001")
		created, err := s.CreateTicket(ctx, in)
		if err != nil {
			t.Fatalf("CreateTicket failed: %v", err)
		}
		tk := created.Ticket
		if created.Decision.ID == 0 || created.Decision.Outcome != store.DecisionAccept {
			t.Fatalf("returned decision %+v must be the committed accept row", created.Decision)
		}

		// The accept decision row exists exactly once.
		decisions, err := s.Decisions(ctx, source)
		if err != nil {
			t.Fatalf("Decisions failed: %v", err)
		}
		if len(decisions) != 1 || decisions[0].Outcome != store.DecisionAccept {
			t.Fatalf("decisions = %+v, want exactly the accept", decisions)
		}

		// The ticket row round-trips the mandate.
		got, err := s.Ticket(ctx, tk.ID)
		if err != nil {
			t.Fatalf("Ticket failed: %v", err)
		}
		if got.ID != tk.ID || got.ThreadSource != source || got.ThreadKey != "issue:42" {
			t.Fatalf("ticket identity mismatch: %+v", got)
		}
		if got.Status != store.TicketStatusPending {
			t.Fatalf("status = %q, want pending", got.Status)
		}
		if got.Mandate != in.Mandate {
			t.Fatalf("mandate = %+v, want %+v", got.Mandate, in.Mandate)
		}

		// The pending assignment binds the ticket and no task yet.
		assignments, err := s.Assignments(ctx, tk.ID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if len(assignments) != 1 {
			t.Fatalf("assignments = %d rows, want exactly the pending one", len(assignments))
		}
		a := assignments[0]
		if a.Status != "pending" || a.TaskID != nil {
			t.Fatalf("assignment = %+v, want pending with no task", a)
		}
		if a.ID != store.AssignmentIDForTrigger(source, "issue-comment:1001") {
			t.Fatalf("assignment id %q is not the deterministic one", a.ID)
		}

		// The confirmation effect is queued inside the same transaction.
		effects, err := s.Effects(ctx)
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		if len(effects) != 1 || effects[0].State != store.EffectPending {
			t.Fatalf("effects = %+v, want exactly one pending effect", effects)
		}
		if effects[0].Channel != "github" || effects[0].Address != "acme/api#42" {
			t.Fatalf("effect channel/address = %s/%s, want github acme/api#42", effects[0].Channel, effects[0].Address)
		}
		var payload map[string]any
		if err := json.Unmarshal(effects[0].Payload, &payload); err != nil {
			t.Fatalf("effect payload is not JSON: %v", err)
		}
		if payload["body"] != "Acknowledged" {
			t.Fatalf("effect payload body = %v, want the posted text", payload["body"])
		}
	})

	t.Run("Given the same trigger offered twice, when CreateTicket runs again after the first commit, assert that the replay is refused by the accept-once invariant and no second ticket or effect exists", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)

		in := acceptInput(source, "issue-comment:1001")
		if _, err := s.CreateTicket(ctx, in); err != nil {
			t.Fatalf("first CreateTicket failed: %v", err)
		}
		if _, err := s.CreateTicket(ctx, in); !errors.Is(err, store.ErrDuplicateAccept) {
			t.Fatalf("replay error = %v, want ErrDuplicateAccept", err)
		}

		tickets, err := s.Tickets(ctx)
		if err != nil {
			t.Fatalf("Tickets failed: %v", err)
		}
		if len(tickets) != 1 {
			t.Fatalf("tickets = %d rows, want exactly one", len(tickets))
		}
		effects, err := s.Effects(ctx)
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		if len(effects) != 1 {
			t.Fatalf("effects = %d rows, want exactly one", len(effects))
		}
	})

	t.Run("Given an effect whose idempotency key already belongs to a committed effect, when CreateTicket runs, assert that the transaction rolls back whole — no decision, no ticket, no assignment", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)

		// Commit a first ticket so its effect key exists.
		first := acceptInput(source, "issue-comment:1001")
		if _, err := s.CreateTicket(ctx, first); err != nil {
			t.Fatalf("first CreateTicket failed: %v", err)
		}

		// A second trigger whose effect reuses the committed key: the
		// outbox UNIQUE (channel, idempotency_key) fires mid-transaction.
		second := acceptInput(source, "issue-comment:1002")
		second.Effects[0].IdempotencyKey = first.Effects[0].IdempotencyKey
		if _, err := s.CreateTicket(ctx, second); err == nil {
			t.Fatalf("a colliding effect key must abort the creation")
		}

		tickets, err := s.Tickets(ctx)
		if err != nil {
			t.Fatalf("Tickets failed: %v", err)
		}
		if len(tickets) != 1 {
			t.Fatalf("tickets = %d rows, want only the first ticket — no half-built second", len(tickets))
		}
		assignments, err := s.Assignments(ctx, store.TicketIDForTrigger(source, "issue-comment:1002"))
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if len(assignments) != 0 {
			t.Fatalf("assignments = %+v, want none for the rolled-back ticket", assignments)
		}
		decisions, err := s.Decisions(ctx, source)
		if err != nil {
			t.Fatalf("Decisions failed: %v", err)
		}
		if len(decisions) != 1 {
			t.Fatalf("decisions = %d rows, want only the first accept — the second must have rolled back", len(decisions))
		}
		effects, err := s.Effects(ctx)
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		if len(effects) != 1 {
			t.Fatalf("effects = %d rows, want only the first effect — no comment was owed", len(effects))
		}
	})

	t.Run("Given store-level input validation, when CreateTicket is offered a shape that would create an unrepresentable authorization, assert that it refuses the input and writes nothing", func(t *testing.T) {
		scenarios := []struct {
			name string
			mut  func(*store.TicketInput)
			want string
		}{
			{"decision outcome is not accept", func(in *store.TicketInput) {
				in.Decision.Outcome = store.DecisionIgnore
			}, "accept"},
			{"mandate strength disagrees with the decision strength", func(in *store.TicketInput) {
				in.Mandate.Strength = "inferred"
			}, "strength"},
			{"mandate policy version disagrees with the decision", func(in *store.TicketInput) {
				in.Mandate.PolicyVersion = "deadbeef"
			}, "policy version"},
			{"mandate names no sender", func(in *store.TicketInput) {
				in.Mandate.Sender = ""
			}, "sender"},
			{"mandate scope names no repository", func(in *store.TicketInput) {
				in.Mandate.Scope.Repo = ""
			}, "scope"},
			{"mandate scope names no issue", func(in *store.TicketInput) {
				in.Mandate.Scope.Issue = 0
			}, "scope"},
			{"effect names no channel", func(in *store.TicketInput) {
				in.Effects[0].Channel = ""
			}, "channel"},
			{"effect names no idempotency key", func(in *store.TicketInput) {
				in.Effects[0].IdempotencyKey = ""
			}, "idempotency key"},
			{"effect payload is not JSON", func(in *store.TicketInput) {
				in.Effects[0].Payload = []byte("not json")
			}, "payload"},
		}
		for _, sc := range scenarios {
			t.Run("Given "+sc.name+", when CreateTicket runs, assert that it refuses with a reason and persists nothing", func(t *testing.T) {
				s := openTestStore(t)
				mustMigrate(t, s)

				in := acceptInput(source, "issue-comment:1001")
				sc.mut(&in)
				if _, err := s.CreateTicket(ctx, in); err == nil {
					t.Fatalf("input must be refused")
				} else if !strings.Contains(err.Error(), sc.want) {
					t.Fatalf("error %q must mention %q", err, sc.want)
				}

				if rows, err := s.Tickets(ctx); err != nil || len(rows) != 0 {
					t.Fatalf("tickets = %+v err=%v, want none", rows, err)
				}
				if rows, err := s.Decisions(ctx, source); err != nil || len(rows) != 0 {
					t.Fatalf("decisions = %+v err=%v, want none", rows, err)
				}
			})
		}
	})
}

func TestTicketIdentity(t *testing.T) {
	t.Run("Given one trigger, when its ticket id is derived repeatedly, assert that the id is stable", func(t *testing.T) {
		first := store.TicketIDForTrigger("github:acme/api", "issue-comment:1001")
		if first != store.TicketIDForTrigger("github:acme/api", "issue-comment:1001") {
			t.Fatal("ticket identity must be deterministic")
		}
		if !strings.HasPrefix(first, "ticket-") {
			t.Fatalf("ticket id %q must carry the ticket- prefix", first)
		}
	})

	t.Run("Given two different triggers, when their ticket ids are derived, assert that the ids differ", func(t *testing.T) {
		a := store.TicketIDForTrigger("github:acme/api", "issue-comment:1001")
		b := store.TicketIDForTrigger("github:acme/api", "issue-comment:1002")
		if a == b {
			t.Fatal("distinct triggers must mint distinct tickets")
		}
	})

	t.Run("Given one trigger, when its assignment id is derived, assert that it shares the trigger's identity space but is not the ticket id", func(t *testing.T) {
		source, id := "github:acme/api", "issue-comment:1001"
		tk := store.TicketIDForTrigger(source, id)
		as := store.AssignmentIDForTrigger(source, id)
		if as == tk || as == "" {
			t.Fatalf("assignment id %q must differ from the ticket id %q", as, tk)
		}
		if store.AssignmentIDForTrigger(source, id) != as {
			t.Fatal("assignment identity must be deterministic")
		}
	})
}
