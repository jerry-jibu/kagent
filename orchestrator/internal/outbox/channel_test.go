package outbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/testenv"
)

// fixture wires a migrated store and a fake GitHub the way the production
// deliverer runs: the deliverer drives the real HTTP transport against the
// fake server, so the fake only swaps the remote endpoint.
type fixture struct {
	store *store.Store
	sch   string
	gh    *github.Fake
	d     *outbox.Deliverer
}

// newFixture opens its own store (schema name kept for the crash-replay
// fixture), starts the fake GitHub and builds the deliverer over both.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := testenv.DatabaseURL(t)
	sch := fmt.Sprintf("orch_test_%d", time.Now().UnixNano())
	ctx := context.Background()
	s, err := store.Open(ctx, store.Config{DatabaseURL: url, Schema: sch})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() {
		s.DropSchema(context.Background())
		s.Close()
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}
	gh := github.NewFake()
	t.Cleanup(gh.Close)
	transport := github.NewTransport(github.TransportConfig{BaseURL: gh.URL()})
	return &fixture{store: s, sch: sch, gh: gh, d: outbox.New(s, transport)}
}

// commitTicket creates a ticket for an accept decision through the real
// single-transaction path, enqueuing its confirmation-comment effect.
func (f *fixture) commitTicket(t *testing.T, commentID int64) string {
	t.Helper()
	source := "github:acme/api"
	ticketID := store.TicketIDForTrigger(source, fmt.Sprintf("issue-comment:%d", commentID))
	body := fmt.Sprintf("Acknowledged — opened ticket %s for this request.\n\n%s",
		ticketID, github.EffectMarker("confirm:"+ticketID))
	payload, err := json.Marshal(github.IssueCommentEffect{
		Kind:  github.EffectKindIssueComment,
		Repo:  "acme/api",
		Issue: 42,
		Body:  body,
	})
	if err != nil {
		t.Fatalf("failed to encode effect payload: %v", err)
	}
	_, err = f.store.CreateTicket(context.Background(), store.TicketInput{
		Source:          source,
		DeterministicID: fmt.Sprintf("issue-comment:%d", commentID),
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
			Channel:        github.ChannelGitHub,
			Address:        "acme/api#42",
			IdempotencyKey: "confirm:" + ticketID,
			Payload:        payload,
		}},
	})
	if err != nil {
		t.Fatalf("failed to commit ticket: %v", err)
	}
	return ticketID
}

// loseReceipt resets an outbox row to pending, simulating a crash between the
// GitHub post and the receipt write (docs/designs/0001 §8: 已发未确认).
func (f *fixture) loseReceipt(t *testing.T, effectID int64) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatalf("failed to open recovery pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(),
		fmt.Sprintf(`UPDATE %s.outbox SET state='pending', receipt=NULL, delivered_at=NULL WHERE id=$1`, f.sch),
		effectID); err != nil {
		t.Fatalf("failed to reset effect: %v", err)
	}
}

func TestDeliverPending(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a committed ticket with its confirmation effect, when the deliverer drains the outbox, assert that the confirmation comment lands on the original issue carrying the ticket id and the deterministic marker, and the effect is delivered with the posted comment id as receipt", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.commitTicket(t, 1001)

		stats, err := f.d.DeliverPending(ctx)
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Delivered != 1 {
			t.Fatalf("delivered = %d, want 1", stats.Delivered)
		}

		posted := f.gh.PostedComments()
		if len(posted) != 1 {
			t.Fatalf("posted comments = %d, want exactly the confirmation", len(posted))
		}
		if posted[0].IssueNumber != 42 {
			t.Fatalf("comment landed on issue %d, want the original issue 42", posted[0].IssueNumber)
		}
		if !strings.Contains(posted[0].Body, ticketID) {
			t.Fatalf("confirmation body %q must carry the ticket id %q", posted[0].Body, ticketID)
		}
		if !strings.Contains(posted[0].Body, github.EffectMarker("confirm:"+ticketID)) {
			t.Fatalf("confirmation body must embed the deterministic marker")
		}

		effects, err := f.store.Effects(ctx)
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		if len(effects) != 1 || effects[0].State != store.EffectDelivered {
			t.Fatalf("effects = %+v, want the delivered effect", effects)
		}
		var receipt map[string]any
		if err := json.Unmarshal(effects[0].Receipt, &receipt); err != nil {
			t.Fatalf("receipt is not JSON: %v", err)
		}
		if receipt["comment_id"] != float64(posted[0].ID) {
			t.Fatalf("receipt = %v, want the posted comment id %d", receipt, posted[0].ID)
		}
	})

	t.Run("Given the marker comment already present on the issue, when the deliverer drains the outbox, assert that no second comment is posted and the effect is delivered through the marker match", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.commitTicket(t, 1001)
		f.gh.AddComment(github.IssueComment{
			ID:          555,
			IssueNumber: 42,
			Author:      "orchestrator",
			Body:        fmt.Sprintf("Acknowledged — opened ticket %s.\n\n%s", ticketID, github.EffectMarker("confirm:"+ticketID)),
		})

		stats, err := f.d.DeliverPending(ctx)
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Confirmed != 1 || stats.Delivered != 0 {
			t.Fatalf("stats = %+v, want confirmed=1 delivered=0", stats)
		}
		if len(f.gh.PostedComments()) != 0 {
			t.Fatalf("a replayed delivery must never post a second comment")
		}
		effects, err := f.store.Effects(ctx)
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		if len(effects) != 1 || effects[0].State != store.EffectDelivered {
			t.Fatalf("effects = %+v, want the delivered effect", effects)
		}
	})

	t.Run("Given a crash between the GitHub post and the receipt write, when the deliverer replays the lost effect, assert that the marker search finds the posted comment and still only one comment exists", func(t *testing.T) {
		f := newFixture(t)
		f.commitTicket(t, 1001)
		if _, err := f.d.DeliverPending(ctx); err != nil {
			t.Fatalf("first delivery failed: %v", err)
		}
		effects, err := f.store.Effects(ctx)
		if err != nil || len(effects) != 1 {
			t.Fatalf("effects = %+v err=%v", effects, err)
		}
		f.loseReceipt(t, effects[0].ID)

		stats, err := f.d.DeliverPending(ctx)
		if err != nil {
			t.Fatalf("replay delivery failed: %v", err)
		}
		if stats.Confirmed != 1 {
			t.Fatalf("replay stats = %+v, want the effect confirmed by marker", stats)
		}
		if len(f.gh.PostedComments()) != 1 {
			t.Fatalf("posted comments = %d, want still exactly the original confirmation", len(f.gh.PostedComments()))
		}
		if rows, err := f.store.PendingEffects(ctx); err != nil || len(rows) != 0 {
			t.Fatalf("pending effects = %+v err=%v, want none", rows, err)
		}
	})

	t.Run("Given an outbox with nothing owed, when the deliverer drains it, assert that nothing is posted and no error surfaces", func(t *testing.T) {
		f := newFixture(t)
		stats, err := f.d.DeliverPending(ctx)
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Delivered != 0 || stats.Confirmed != 0 {
			t.Fatalf("stats = %+v, want a no-op", stats)
		}
		if len(f.gh.PostedComments()) != 0 {
			t.Fatalf("an empty outbox must post nothing")
		}
	})

	t.Run("Given an effect payload that is not a GitHub comment effect, when the deliverer drains the outbox, assert that it fails loudly and the effect stays pending", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.store.CreateTicket(context.Background(), store.TicketInput{
			Source:          "github:acme/api",
			DeterministicID: "issue-comment:1001",
			ThreadKey:       "issue:42",
			Decision: store.DecisionInput{
				Outcome:       store.DecisionAccept,
				Strength:      "explicit",
				PolicyVersion: "abc123",
			},
			Mandate: store.Mandate{
				PolicyVersion: "abc123",
				Strength:      "explicit",
				Sender:        "alice",
				Scope:         store.MandateScope{Repo: "acme/api", Issue: 42},
			},
			Effects: []store.OutboxEffect{{
				Channel:        github.ChannelGitHub,
				Address:        "acme/api#42",
				IdempotencyKey: "confirm:weird",
				Payload:        []byte(`{"kind":"unknown-channel-effect"}`),
			}},
		}); err != nil {
			t.Fatalf("failed to commit ticket: %v", err)
		}

		if _, err := f.d.DeliverPending(ctx); err == nil {
			t.Fatalf("a malformed effect must fail the delivery loudly")
		}
		if rows, err := f.store.PendingEffects(ctx); err != nil || len(rows) != 1 {
			t.Fatalf("pending effects = %+v err=%v, want the malformed effect still pending", rows, err)
		}
		if len(f.gh.PostedComments()) != 0 {
			t.Fatalf("a malformed effect must not post anything")
		}
	})

	t.Run("Given a GitHub outage, when the deliverer drains the outbox, assert that the failure surfaces and the effect stays pending for the next cycle", func(t *testing.T) {
		f := newFixture(t)
		f.commitTicket(t, 1001)
		f.gh.FailWith(502, `{"message":"Server error"}`)

		if _, err := f.d.DeliverPending(ctx); err == nil {
			t.Fatalf("a GitHub outage must fail the delivery")
		}
		if rows, err := f.store.PendingEffects(ctx); err != nil || len(rows) != 1 {
			t.Fatalf("pending effects = %+v err=%v, want the effect still pending", rows, err)
		}

		f.gh.SetOK()
		if _, err := f.d.DeliverPending(ctx); err != nil {
			t.Fatalf("delivery after recovery failed: %v", err)
		}
		if len(f.gh.PostedComments()) != 1 {
			t.Fatalf("posted comments = %d, want the recovered confirmation", len(f.gh.PostedComments()))
		}
	})
}
