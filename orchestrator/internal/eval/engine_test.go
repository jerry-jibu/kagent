package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/eval"
	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/testenv"
)

// testPolicy builds the policy the engine tests judge under.
func testPolicy() policy.Policy {
	p := policy.Policy{
		Repositories: []string{"acme/api"},
		Admission: []policy.AdmissionRule{
			{ID: "alice", Strength: policy.StrengthExplicit},
			{ID: "bob", Strength: policy.StrengthInferred},
		},
		Bots: []string{"acme-bot"},
	}
	p.Version = p.Hash()
	return p
}

// comment builds an issue-comment payload as intake persists it.
func comment(author, body, repo string) intake.CommentPayload {
	return intake.CommentPayload{
		Kind:      "issue-comment",
		Repo:      repo,
		Issue:     42,
		CommentID: 1001,
		Author:    author,
		Body:      body,
		URL:       "https://github.com/" + repo + "/issues/42#issuecomment-1001",
		CreatedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
	}
}

// engineFixture wires a migrated store, a fake A2A transport and the eval
// Engine the way a production run would (fake only swaps the transport).
type engineFixture struct {
	store     *store.Store
	transport *a2a.Fake
	evaluator *eval.Evaluator
	engine    *eval.Engine
	pol       policy.Policy
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	f := &engineFixture{
		store:     testenv.NewMigratedStore(t),
		transport: a2a.NewFake(),
		pol:       testPolicy(),
	}
	t.Cleanup(f.transport.Close)
	f.evaluator = eval.NewEvaluator(f.transport, "evaluator")
	f.engine = eval.NewEngine(f.store, f.evaluator)
	return f
}

// seedTrigger ingests one issue-comment trigger through the store's real
// ingest path and returns the stored trigger row, so the engine consumes
// exactly what intake persisted (decode contract, not a hand-built row).
func (f *engineFixture) seedTrigger(t *testing.T, pl intake.CommentPayload) (string, store.Trigger) {
	t.Helper()
	ctx := context.Background()
	payload, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("failed to encode trigger payload: %v", err)
	}
	source := github.SourceName(pl.Repo)
	if _, err := f.store.IngestEvents(ctx,
		store.SourceRef{Name: source, Kind: "github"},
		[]store.Event{{
			ThreadKey:       fmt.Sprintf("issue:%d", pl.Issue),
			DeterministicID: fmt.Sprintf("issue-comment:%d", pl.CommentID),
			Payload:         payload,
			OccurredAt:      pl.CreatedAt,
		}}, intake.DefaultSightline); err != nil {
		t.Fatalf("failed to ingest trigger: %v", err)
	}
	triggers, err := f.store.Triggers(ctx, source)
	if err != nil || len(triggers) != 1 {
		t.Fatalf("failed to read back trigger: %v (rows=%d)", err, len(triggers))
	}
	return source, triggers[0]
}

// acceptEcho scripts the evaluator to answer with the accept Decision the
// outputSchema contract requires: it echoes the gate's strength and policy
// version verbatim.
func (f *engineFixture) acceptEcho(reason string) {
	raw := fmt.Sprintf(`{"outcome":"accept","strength":%q,"policy_version":%q,"reason":%q}`,
		f.pol.Admission[0].Strength, f.pol.Version, reason)
	f.transport.Script("evaluator", raw)
}

// mustTrigger reads back the stored trigger row of one deterministic id, for
// tests that seed several triggers and drive a single one directly.
func (f *engineFixture) mustTrigger(t *testing.T, source, deterministicID string) store.Trigger {
	t.Helper()
	triggers, err := f.store.Triggers(context.Background(), source)
	if err != nil {
		t.Fatalf("failed to read triggers: %v", err)
	}
	for _, tr := range triggers {
		if tr.DeterministicID == deterministicID {
			return tr
		}
	}
	t.Fatalf("trigger %s/%s not found after seeding", source, deterministicID)
	return store.Trigger{}
}

func (f *engineFixture) decisions(t *testing.T, source string) []store.Decision {
	t.Helper()
	rows, err := f.store.Decisions(context.Background(), source)
	if err != nil {
		t.Fatalf("failed to read decisions: %v", err)
	}
	return rows
}

func (f *engineFixture) tickets(t *testing.T) []store.Ticket {
	t.Helper()
	rows, err := f.store.Tickets(context.Background())
	if err != nil {
		t.Fatalf("failed to read tickets: %v", err)
	}
	return rows
}

func (f *engineFixture) effects(t *testing.T) []store.Effect {
	t.Helper()
	rows, err := f.store.Effects(context.Background())
	if err != nil {
		t.Fatalf("failed to read effects: %v", err)
	}
	return rows
}

func TestEngineEvaluateTrigger(t *testing.T) {
	// addressedComment builds the canonical authorized trigger: explicit
	// sender alice, @bot mention, allowlisted repo.
	addressedComment := func(repo string) intake.CommentPayload {
		return comment("alice", "@acme-bot please fix the flaky login test", repo)
	}

	t.Run("Given an allowlisted repository and an @bot comment by an explicit sender, when the evaluator returns an accept echoing the gate verdict, assert that an accept decision carrying the gate strength and policy version is persisted", func(t *testing.T) {
		f := newEngineFixture(t)
		f.acceptEcho("authorized coding request")
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		dec, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol)
		if err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 {
			t.Fatalf("decisions = %d rows, want exactly the accept", len(rows))
		}
		got := rows[0]
		if got.Outcome != store.DecisionAccept {
			t.Fatalf("outcome = %q, want accept", got.Outcome)
		}
		if got.Strength != policy.StrengthExplicit {
			t.Fatalf("persisted strength = %q, want the gate strength explicit", got.Strength)
		}
		if got.PolicyVersion != f.pol.Version {
			t.Fatalf("persisted policy version = %q, want the gate policy version", got.PolicyVersion)
		}
		if got.Reason != "authorized coding request" {
			t.Fatalf("reason = %q, want the evaluator's judgment", got.Reason)
		}
		if got.ID != dec.ID {
			t.Fatalf("returned decision %+v does not match the persisted row %+v", dec, got)
		}

		// The accept transition commits the whole ticket in the same
		// transaction as the decision (docs/designs/0001 §6): ticket with
		// mandate, pending assignment, and the queued confirmation effect.
		tickets := f.tickets(t)
		if len(tickets) != 1 {
			t.Fatalf("tickets = %+v, want exactly the accepted ticket", tickets)
		}
		tk := tickets[0]
		if tk.ID != store.TicketIDForTrigger(source, tr.DeterministicID) {
			t.Fatalf("ticket id %q is not the deterministic one for the trigger", tk.ID)
		}
		if tk.Status != store.TicketStatusPending {
			t.Fatalf("ticket status = %q, want pending", tk.Status)
		}
		if tk.Mandate.Sender != "alice" || tk.Mandate.Strength != policy.StrengthExplicit ||
			tk.Mandate.PolicyVersion != f.pol.Version ||
			tk.Mandate.Scope != (store.MandateScope{Repo: "acme/api", Issue: 42}) {
			t.Fatalf("mandate = %+v, want policy hash + sender + scope + strength", tk.Mandate)
		}
		assignments, err := f.store.Assignments(context.Background(), tk.ID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if len(assignments) != 1 || assignments[0].Status != "pending" || assignments[0].TaskID != nil {
			t.Fatalf("assignments = %+v, want exactly one pending assignment with no task", assignments)
		}
		effects := f.effects(t)
		if len(effects) != 1 || effects[0].State != store.EffectPending {
			t.Fatalf("effects = %+v, want exactly one pending confirmation effect", effects)
		}
		confirm, err := github.ParseIssueCommentEffect(effects[0].Payload)
		if err != nil {
			t.Fatalf("confirmation effect payload does not decode: %v", err)
		}
		if confirm.Repo != "acme/api" || confirm.Issue != 42 {
			t.Fatalf("confirmation effect targets %s#%d, want the original issue", confirm.Repo, confirm.Issue)
		}
		if !strings.Contains(confirm.Body, tk.ID) {
			t.Fatalf("confirmation body %q must carry the ticket id", confirm.Body)
		}
		if !strings.Contains(confirm.Body, github.EffectMarker(effects[0].IdempotencyKey)) {
			t.Fatalf("confirmation body must embed the deterministic marker for its idempotency key")
		}

		// The evaluator invoke is the assessment input contract: the agent
		// must have seen the trigger text and the gate verdict.
		if f.transport.CallCount("evaluator") != 1 {
			t.Fatalf("evaluator invoked %d times, want exactly once", f.transport.CallCount("evaluator"))
		}
		call := f.transport.Calls()[0]
		if call.Agent != "evaluator" {
			t.Fatalf("invoke went to agent %q, want the evaluator", call.Agent)
		}
		if !strings.Contains(call.Message, "please fix the flaky login test") {
			t.Fatalf("assessment message %q must carry the trigger text", call.Message)
		}
		if !strings.Contains(call.Message, f.pol.Version) {
			t.Fatalf("assessment message %q must carry the policy version", call.Message)
		}
		if !strings.Contains(call.Message, policy.StrengthExplicit) {
			t.Fatalf("assessment message %q must carry the gate strength", call.Message)
		}
	})

	t.Run("Given a comment in a repository outside the allowlist, when the trigger is evaluated, assert that a denied decision is recorded and the evaluator is never invoked", func(t *testing.T) {
		f := newEngineFixture(t)
		f.acceptEcho("should never be reached")
		source, tr := f.seedTrigger(t, addressedComment("acme/secret"))

		_, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol)
		if err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionDenied {
			t.Fatalf("decisions = %+v, want exactly one denied row", rows)
		}
		if rows[0].PolicyVersion != f.pol.Version {
			t.Fatalf("denial must record the policy version it was judged under, got %q", rows[0].PolicyVersion)
		}
		if rows[0].Reason == "" {
			t.Fatalf("denial must record the gate's reason")
		}
		if f.transport.CallCount("evaluator") != 0 {
			t.Fatalf("a denied trigger must never reach the evaluator, got %d invokes",
				f.transport.CallCount("evaluator"))
		}
		if tickets := f.tickets(t); len(tickets) != 0 {
			t.Fatalf("a denial must never create a ticket, got %+v", tickets)
		}
	})

	t.Run("Given a sender that is not admitted by the policy, when the trigger is evaluated, assert that the request is silently denied with an audit row and without an evaluator invoke", func(t *testing.T) {
		f := newEngineFixture(t)
		source, tr := f.seedTrigger(t, comment("eve", "@acme-bot please delete the database", "acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionDenied {
			t.Fatalf("decisions = %+v, want exactly one denied row", rows)
		}
		if f.transport.CallCount("evaluator") != 0 {
			t.Fatalf("an unauthorized sender must never reach the evaluator, got %d invokes",
				f.transport.CallCount("evaluator"))
		}
	})

	t.Run("Given an evaluator that returns an accept carrying a forged strength, when the trigger is evaluated, assert that the decision is persisted as rejected with the gate-verified values and no accept ever exists", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Script("evaluator", fmt.Sprintf(
			`{"outcome":"accept","strength":"inferred","policy_version":%q,"reason":"I decide this is fine"}`,
			f.pol.Version))
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		_, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol)
		if err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 {
			t.Fatalf("decisions = %d rows, want exactly the rejection record", len(rows))
		}
		got := rows[0]
		if got.Outcome != store.DecisionRejected {
			t.Fatalf("outcome = %q, want rejected", got.Outcome)
		}
		if got.Strength != policy.StrengthExplicit {
			t.Fatalf("persisted strength = %q, want the gate-verified explicit, never the forged claim", got.Strength)
		}
		if got.PolicyVersion != f.pol.Version {
			t.Fatalf("persisted policy version = %q, want the gate-verified version", got.PolicyVersion)
		}
		if !strings.Contains(got.Reason, "strength") {
			t.Fatalf("rejection reason %q must name the forgery", got.Reason)
		}
	})

	t.Run("Given an evaluator that returns an accept carrying a forged policy version, when the trigger is evaluated, assert that the decision is persisted as rejected", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Script("evaluator", `{"outcome":"accept","strength":"explicit","policy_version":"deadbeef","reason":"under my own policy"}`)
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionRejected {
			t.Fatalf("decisions = %+v, want exactly one rejected row", rows)
		}
		if !strings.Contains(rows[0].Reason, "policy version") {
			t.Fatalf("rejection reason %q must name the forged policy version", rows[0].Reason)
		}
	})

	t.Run("Given an evaluator that is unreachable, when the trigger is evaluated, assert that an unavailable decision carrying the transport error is persisted as the retryable record", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Fail("evaluator", errors.New("connection refused"))
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionUnavailable {
			t.Fatalf("decisions = %+v, want exactly one unavailable row", rows)
		}
		if !strings.Contains(rows[0].Reason, "connection refused") {
			t.Fatalf("unavailable reason %q must carry the transport error", rows[0].Reason)
		}
		if !rows[0].Retryable() {
			t.Fatalf("an unavailable decision is the retryable record")
		}
	})

	t.Run("Given an evaluator that hangs past the invoke timeout, when the trigger is evaluated, assert that the timeout surfaces as an unavailable decision", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Hang("evaluator")
		f.evaluator.Timeout = 20 * time.Millisecond
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionUnavailable {
			t.Fatalf("decisions = %+v, want exactly one unavailable row", rows)
		}
		if !strings.Contains(rows[0].Reason, "deadline") && !strings.Contains(rows[0].Reason, "context") {
			t.Fatalf("unavailable reason %q must carry the timeout", rows[0].Reason)
		}
	})

	t.Run("Given a trigger whose evaluation was unavailable, when the evaluator recovers and the trigger is re-evaluated, assert that an accept is persisted alongside the unavailable record and the retry is honored", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Fail("evaluator", errors.New("connection refused"))
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("first evaluation failed: %v", err)
		}
		f.transport.Fail("evaluator", nil)
		f.acceptEcho("accepted on retry")
		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("retried evaluation failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 2 {
			t.Fatalf("decisions = %d rows, want the unavailable record and the accept", len(rows))
		}
		if rows[0].Outcome != store.DecisionUnavailable || rows[1].Outcome != store.DecisionAccept {
			t.Fatalf("outcomes = %q then %q, want unavailable then accept", rows[0].Outcome, rows[1].Outcome)
		}
	})

	t.Run("Given a trigger that already has a terminal decision, when it is re-evaluated, assert that no new evaluator invoke happens and no new decision row appears", func(t *testing.T) {
		f := newEngineFixture(t)
		f.acceptEcho("authorized coding request")
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("first evaluation failed: %v", err)
		}
		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("re-evaluation failed: %v", err)
		}

		if f.transport.CallCount("evaluator") != 1 {
			t.Fatalf("evaluator invoked %d times, want exactly one invoke across both runs",
				f.transport.CallCount("evaluator"))
		}
		if rows := f.decisions(t, source); len(rows) != 1 {
			t.Fatalf("decisions = %d rows, want the single accept", len(rows))
		}
		if tickets := f.tickets(t); len(tickets) != 1 {
			t.Fatalf("tickets = %d rows, want the single accepted ticket across both runs", len(tickets))
		}
		if effects := f.effects(t); len(effects) != 1 {
			t.Fatalf("effects = %d rows, want exactly the one confirmation", len(effects))
		}
	})

	t.Run("Given an evaluator that returns ignore for insufficient evidence, when the trigger is evaluated, assert that an ignore decision carrying the gate strength and reason is persisted", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Script("evaluator", `{"outcome":"ignore","reason":"insufficient evidence to act"}`)
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionIgnore {
			t.Fatalf("decisions = %+v, want exactly one ignore row", rows)
		}
		if rows[0].Strength != policy.StrengthExplicit {
			t.Fatalf("persisted strength = %q, want the gate strength", rows[0].Strength)
		}
		if rows[0].Reason != "insufficient evidence to act" {
			t.Fatalf("reason = %q, want the evaluator's reason", rows[0].Reason)
		}
		if tickets := f.tickets(t); len(tickets) != 0 {
			t.Fatalf("an ignore decision must never create a ticket, got %+v", tickets)
		}
		if effects := f.effects(t); len(effects) != 0 {
			t.Fatalf("no confirmation comment is owed without a ticket, got %+v", effects)
		}
	})

	t.Run("Given an evaluator that returns stop for an explicit sender, when the trigger is evaluated, assert that no ticket and no confirmation effect exist and the reason is persisted", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Script("evaluator", `{"outcome":"stop","reason":"request is ambiguous, waiting for the author"}`)
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		if tickets := f.tickets(t); len(tickets) != 0 {
			t.Fatalf("a stop decision must never create a ticket, got %+v", tickets)
		}
		if effects := f.effects(t); len(effects) != 0 {
			t.Fatalf("no confirmation comment is owed without a ticket, got %+v", effects)
		}
		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionStop {
			t.Fatalf("decisions = %+v, want exactly the stop record", rows)
		}
		if rows[0].Reason != "request is ambiguous, waiting for the author" {
			t.Fatalf("stop reason = %q, want the evaluator's judgment persisted", rows[0].Reason)
		}
		if rows[0].Strength != policy.StrengthExplicit {
			t.Fatalf("stop record strength = %q, want the gate-verified strength", rows[0].Strength)
		}
	})

	t.Run("Given an evaluator that reports its own unavailability, when the trigger is evaluated, assert that an unavailable decision is persisted", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Script("evaluator", `{"outcome":"unavailable","reason":"cannot reach the thread context"}`)
		source, tr := f.seedTrigger(t, addressedComment("acme/api"))

		if _, err := f.engine.EvaluateTrigger(context.Background(), source, tr, f.pol); err != nil {
			t.Fatalf("EvaluateTrigger failed: %v", err)
		}

		rows := f.decisions(t, source)
		if len(rows) != 1 || rows[0].Outcome != store.DecisionUnavailable {
			t.Fatalf("decisions = %+v, want exactly one unavailable row", rows)
		}
		if !rows[0].Retryable() {
			t.Fatalf("an unavailable decision is the retryable record")
		}
	})

	t.Run("Given a trigger whose stored payload is not a CommentPayload, when the trigger is evaluated, assert that the evaluation fails loudly and records no decision", func(t *testing.T) {
		f := newEngineFixture(t)
		f.acceptEcho("should never be reached")
		ctx := context.Background()
		source := github.SourceName("acme/api")
		if _, err := f.store.IngestEvents(ctx,
			store.SourceRef{Name: source, Kind: "github"},
			[]store.Event{{
				ThreadKey:       "issue:42",
				DeterministicID: "issue-comment:666",
				Payload:         []byte(`{"kind":"unknown","nope":1}`),
				OccurredAt:      time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
			}}, intake.DefaultSightline); err != nil {
			t.Fatalf("failed to ingest trigger: %v", err)
		}
		triggers, err := f.store.Triggers(ctx, source)
		if err != nil || len(triggers) != 1 {
			t.Fatalf("failed to read back trigger: %v", err)
		}

		if _, err := f.engine.EvaluateTrigger(ctx, source, triggers[0], f.pol); err == nil {
			t.Fatalf("a malformed payload must fail the evaluation loudly")
		}
		if rows := f.decisions(t, source); len(rows) != 0 {
			t.Fatalf("decisions = %+v, want no record for a payload the contract cannot decode", rows)
		}
	})
}

func TestEngineEvaluatePending(t *testing.T) {
	ctx := context.Background()

	// addressedComment builds the canonical authorized trigger: explicit
	// sender alice, @bot mention, allowlisted repo.
	addressedComment := func(commentID int64, body string) intake.CommentPayload {
		pl := comment("alice", body, "acme/api")
		pl.CommentID = commentID
		return pl
	}
	// ingest ingests one trigger through the real ingest path without the
	// single-row read-back assertion of seedTrigger (these tests seed
	// several triggers on one source).
	ingest := func(t *testing.T, f *engineFixture, pl intake.CommentPayload) string {
		t.Helper()
		raw, err := json.Marshal(pl)
		if err != nil {
			t.Fatalf("failed to encode trigger payload: %v", err)
		}
		source := github.SourceName(pl.Repo)
		if _, err := f.store.IngestEvents(context.Background(),
			store.SourceRef{Name: source, Kind: "github"},
			[]store.Event{{
				ThreadKey:       fmt.Sprintf("issue:%d", pl.Issue),
				DeterministicID: fmt.Sprintf("issue-comment:%d", pl.CommentID),
				Payload:         raw,
				OccurredAt:      pl.CreatedAt,
			}}, intake.DefaultSightline); err != nil {
			t.Fatalf("failed to ingest trigger: %v", err)
		}
		return source
	}

	t.Run("Given two pending triggers on a source, when EvaluatePending runs, assert that both are judged, the accepted one mints a ticket and the cycle reports what it did", func(t *testing.T) {
		f := newEngineFixture(t)
		f.transport.Script("evaluator",
			`{"outcome":"ignore","reason":"not a task"}`,
			fmt.Sprintf(`{"outcome":"accept","strength":"explicit","policy_version":%q,"reason":"a task"}`, f.pol.Version))
		source1 := ingest(t, f, addressedComment(2001, "@acme-bot what is this issue about?"))
		source2 := ingest(t, f, addressedComment(2002, "@acme-bot please fix the flaky login test"))
		if source1 != source2 {
			t.Fatalf("both triggers must belong to one source, got %s and %s", source1, source2)
		}

		stats, err := f.engine.EvaluatePending(ctx, source1, f.pol, 10)
		if err != nil {
			t.Fatalf("EvaluatePending failed: %v", err)
		}
		if stats.Evaluated != 2 {
			t.Fatalf("evaluated = %d, want both triggers judged", stats.Evaluated)
		}
		if stats.Tickets != 1 {
			t.Fatalf("tickets = %d, want exactly the accepted one", stats.Tickets)
		}
		if stats.Outcomes[store.DecisionIgnore] != 1 || stats.Outcomes[store.DecisionAccept] != 1 {
			t.Fatalf("outcomes = %+v, want one ignore and one accept", stats.Outcomes)
		}
		if tickets := f.tickets(t); len(tickets) != 1 {
			t.Fatalf("tickets = %d rows, want exactly the accepted ticket", len(tickets))
		}
		if effects := f.effects(t); len(effects) != 1 {
			t.Fatalf("effects = %d rows, want exactly the confirmation effect", len(effects))
		}
	})

	t.Run("Given a cycle with an already-terminal trigger and a fresh one, when EvaluatePending runs, assert that only the fresh trigger is judged and the terminal one is never re-evaluated", func(t *testing.T) {
		f := newEngineFixture(t)
		f.acceptEcho("first cycle accept")
		source, tr1 := f.seedTrigger(t, addressedComment(3001, "@acme-bot please fix the flaky login test"))
		if _, err := f.engine.EvaluateTrigger(ctx, source, tr1, f.pol); err != nil {
			t.Fatalf("first evaluation failed: %v", err)
		}
		f.acceptEcho("should never be consumed")
		ingest(t, f, addressedComment(3002, "@acme-bot also please add a retry"))

		stats, err := f.engine.EvaluatePending(ctx, source, f.pol, 10)
		if err != nil {
			t.Fatalf("EvaluatePending failed: %v", err)
		}
		if stats.Evaluated != 1 {
			t.Fatalf("evaluated = %d, want only the fresh trigger judged", stats.Evaluated)
		}
		if tickets := f.tickets(t); len(tickets) != 2 {
			t.Fatalf("tickets = %d rows, want the first ticket and the new one", len(tickets))
		}
	})

	t.Run("Given one trigger whose payload violates the decode contract, when EvaluatePending runs, assert that the failure surfaces but the other trigger is still judged", func(t *testing.T) {
		f := newEngineFixture(t)
		f.acceptEcho("authorized coding request")
		ctxLocal := context.Background()
		source := github.SourceName("acme/api")
		if _, err := f.store.IngestEvents(ctxLocal,
			store.SourceRef{Name: source, Kind: "github"},
			[]store.Event{{
				ThreadKey:       "issue:42",
				DeterministicID: "issue-comment:666",
				Payload:         []byte(`{"kind":"unknown","nope":1}`),
				OccurredAt:      time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
			}}, intake.DefaultSightline); err != nil {
			t.Fatalf("failed to ingest the malformed trigger: %v", err)
		}
		// Seed the healthy trigger directly: seedTrigger asserts a
		// single-row read-back, which no longer holds on this source.
		ingest(t, f, addressedComment(4001, "@acme-bot please fix the flaky login test"))

		stats, err := f.engine.EvaluatePending(ctxLocal, source, f.pol, 10)
		if err == nil {
			t.Fatalf("a malformed trigger must fail the cycle loudly")
		}
		if stats.Evaluated != 1 {
			t.Fatalf("evaluated = %d, want the healthy trigger still judged", stats.Evaluated)
		}
		if tickets := f.tickets(t); len(tickets) != 1 {
			t.Fatalf("tickets = %d rows, want the healthy trigger's ticket", len(tickets))
		}
	})
}
