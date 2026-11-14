package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/gate"
	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// DefaultInvokeTimeout bounds one evaluator invoke. The evaluator is an
// agent turn — bounded, but not instant.
const DefaultInvokeTimeout = 60 * time.Second

// DefaultEvaluateBatch caps how many pending triggers one EvaluatePending
// cycle judges. A poison trigger (a payload the decode contract rejects)
// stays pending and is retried every cycle; the bound keeps one bad trigger
// from turning a cycle into an unbounded retry storm.
const DefaultEvaluateBatch = 50

// Engine is the eval slice of the control-plane writer (docs/designs/0001
// §3: Eval = Gate + Assessment; spec: Engine 唯一写者). For one ingested
// trigger it runs the deterministic Gate, invokes the evaluator agent only
// when the Gate allows, validates the Decision against the Gate's inputs,
// and persists the outcome as a decision record. Later tickets extend the
// same writer: an accept decision is what Ticket creation keys off.
type Engine struct {
	store     *store.Store
	evaluator *Evaluator
}

// NewEngine wires the Engine to its store and evaluator.
func NewEngine(st *store.Store, evaluator *Evaluator) *Engine {
	return &Engine{store: st, evaluator: evaluator}
}

// EvalStats reports what one EvaluatePending cycle judged.
type EvalStats struct {
	// Evaluated counts triggers driven to a terminal decision this cycle.
	Evaluated int
	// Tickets counts accepted tickets created this cycle.
	Tickets int
	// Outcomes counts persisted decisions by outcome.
	Outcomes map[string]int
}

// EvaluatePending judges every trigger of a source that still owes a
// decision (docs/designs/0001 §8 failure semantics: unavailable records are
// retryable; terminal decisions are never re-evaluated). One failing trigger
// does not block the others — its error is collected and the cycle returns a
// joined error, so a poison payload stays loud without starving the queue.
func (e *Engine) EvaluatePending(ctx context.Context, source string, pol policy.Policy, limit int) (EvalStats, error) {
	triggers, err := e.store.PendingTriggers(ctx, source, limit)
	if err != nil {
		return EvalStats{}, fmt.Errorf("failed to read pending triggers for %s: %w", source, err)
	}

	var stats EvalStats
	stats.Outcomes = map[string]int{}
	var errs []error
	for _, tr := range triggers {
		dec, err := e.EvaluateTrigger(ctx, source, tr, pol)
		if err != nil {
			errs = append(errs, fmt.Errorf("trigger %s/%s: %w", source, tr.DeterministicID, err))
			continue
		}
		stats.Evaluated++
		stats.Outcomes[dec.Outcome]++
		if dec.Outcome == store.DecisionAccept {
			stats.Tickets++
		}
	}
	return stats, errors.Join(errs...)
}

// EvaluateTrigger runs the full eval chain for one ingested trigger and
// returns the persisted decision.
//
// Failure semantics (docs/designs/0001 §8, spec AC):
//   - a trigger with a terminal decision is never re-evaluated (idempotent
//     under poll overlap and crash-retry replays);
//   - an unavailable record (evaluator unreachable/timeout) is retryable:
//     re-evaluation continues the trail in the same decisions table;
//   - a payload the decode contract cannot read fails loudly and records
//     nothing — malformed durable data is never silently omitted.
func (e *Engine) EvaluateTrigger(ctx context.Context, source string, tr store.Trigger, pol policy.Policy) (store.Decision, error) {
	if latest, found, err := e.store.TriggerDecision(ctx, source, tr.DeterministicID); err != nil {
		return store.Decision{}, fmt.Errorf("failed to read prior decisions for trigger %s/%s: %w",
			source, tr.DeterministicID, err)
	} else if found && !latest.Retryable() {
		// Already judged terminally: the trigger is a replay (overlapping
		// poll, crash retry). The existing record stands.
		return latest, nil
	}

	pl, err := intake.DecodeCommentPayload(tr.Payload)
	if err != nil {
		return store.Decision{}, fmt.Errorf("trigger %s/%s payload violates the decode contract: %w",
			source, tr.DeterministicID, err)
	}

	// Gate first: deterministic code owns admission; the evaluator never
	// sees a trigger the Gate denied, so a denial costs no invoke
	// (docs/designs 0001 §7: 门在编排器，不在模型).
	verdict := gate.Judge(pol, pl)
	if !verdict.Allowed {
		return e.record(ctx, source, tr.DeterministicID, store.DecisionInput{
			Outcome:       store.DecisionDenied,
			PolicyVersion: pol.Version,
			Reason:        verdict.Reason,
		})
	}

	// followupOwners guard (T8, docs/designs 0001 §3: 还欠人回复的 Ticket
	// 持有 Thread 上的 follow-up，Engine 只许 stop; §4.2: 该 Thread 有活
	// Ticket → 订阅扇出免评估). A thread with a live ticket has an owner:
	// judging this trigger into a second ticket would fork the thread's
	// work. P0 has no fan-out channel, so the gate-allowed message is
	// recorded as the owner's follow-up input (it rides along on the
	// owner's next recovery dispatch) and judged ignore — no evaluator
	// invoke. A denied message never reaches this guard: input only
	// matters if the Gate would have admitted it.
	owner, owned, err := e.store.FollowupOwnerOfThread(ctx, source, tr.ThreadKey)
	if err != nil {
		return store.Decision{}, fmt.Errorf("failed to resolve the followup owner of %s/%s: %w",
			source, tr.ThreadKey, err)
	}
	if owned {
		dec, err := e.store.RecordFollowupInput(ctx, owner.ID, source, tr.DeterministicID, store.DecisionInput{
			Outcome:       store.DecisionIgnore,
			Strength:      verdict.Strength,
			PolicyVersion: pol.Version,
			Reason: fmt.Sprintf("thread %s/%s is owned by live ticket %s (followupOwners); recorded as its follow-up input",
				source, tr.ThreadKey, owner.ID),
		})
		if err != nil {
			return store.Decision{}, fmt.Errorf("failed to record the followup input for trigger %s/%s: %w",
				source, tr.DeterministicID, err)
		}
		return dec, nil
	}

	in := Input{
		Source:          source,
		DeterministicID: tr.DeterministicID,
		ThreadKey:       tr.ThreadKey,
		Payload:         pl,
		Strength:        verdict.Strength,
		PolicyVersion:   pol.Version,
	}
	dec, err := e.evaluator.Evaluate(ctx, in)
	switch {
	case err == nil && dec.Outcome == store.DecisionAccept:
		// The accept transition: decision, ticket, assignment and the queued
		// confirmation comment all commit in one transaction
		// (docs/designs/0001 §6 — this is the Engine's sole-writer duty T4
		// delivers).
		return e.accept(ctx, source, tr, pl, verdict, dec, pol)
	case err == nil:
		// Persist the GATE-verified strength and policy version — the
		// Decision's echoed values were validated to be identical, and for
		// empty echoes (stop/ignore) the gate's values are the record.
		return e.record(ctx, source, tr.DeterministicID, store.DecisionInput{
			Outcome:       dec.Outcome,
			Strength:      verdict.Strength,
			PolicyVersion: pol.Version,
			Reason:        dec.Reason,
		})
	case errors.Is(err, ErrReject):
		// Forged or unparseable evaluator output: the claim never lands as
		// a decision the pipeline would act on; the rejection record keeps
		// the forgery attempt auditable.
		return e.record(ctx, source, tr.DeterministicID, store.DecisionInput{
			Outcome:       store.DecisionRejected,
			Strength:      verdict.Strength,
			PolicyVersion: pol.Version,
			Reason:        err.Error(),
		})
	default:
		// Evaluator unreachable or timed out: the retryable record.
		return e.record(ctx, source, tr.DeterministicID, store.DecisionInput{
			Outcome:       store.DecisionUnavailable,
			Strength:      verdict.Strength,
			PolicyVersion: pol.Version,
			Reason:        err.Error(),
		})
	}
}

// record persists one decision row; the decisions table is the evaluation
// audit trail, so a record failure is an error, never a log line.
func (e *Engine) record(ctx context.Context, source, deterministicID string, in store.DecisionInput) (store.Decision, error) {
	dec, err := e.store.RecordDecision(ctx, source, deterministicID, in)
	if err != nil {
		return store.Decision{}, fmt.Errorf("failed to persist %s decision for trigger %s/%s: %w",
			in.Outcome, source, deterministicID, err)
	}
	return dec, nil
}

// accept performs the full accept transition (docs/designs/0001 §6): the
// accept decision, the ticket with its mandate (policy hash + sender +
// scope + strength), the pending assignment, and the confirmation-comment
// effect commit together or not at all. The effect is delivered by the
// outbox channel only after the commit, so any failure here leaves neither a
// half-built ticket nor an outward comment.
func (e *Engine) accept(ctx context.Context, source string, tr store.Trigger, pl intake.CommentPayload,
	verdict gate.Verdict, dec Decision, pol policy.Policy) (store.Decision, error) {

	mandate := store.Mandate{
		PolicyVersion: pol.Version,
		Strength:      verdict.Strength,
		Sender:        pl.Author,
		Scope:         store.MandateScope{Repo: pl.Repo, Issue: pl.Issue},
	}
	decisionIn := store.DecisionInput{
		Outcome:       store.DecisionAccept,
		Strength:      verdict.Strength,
		PolicyVersion: pol.Version,
		Reason:        dec.Reason,
	}
	ticketID := store.TicketIDForTrigger(source, tr.DeterministicID)
	effect, err := confirmationEffect(ticketID, mandate)
	if err != nil {
		return store.Decision{}, fmt.Errorf("failed to build the confirmation effect for ticket %s: %w", ticketID, err)
	}

	created, err := e.store.CreateTicket(ctx, store.TicketInput{
		Source:          source,
		DeterministicID: tr.DeterministicID,
		ThreadKey:       tr.ThreadKey,
		Decision:        decisionIn,
		Mandate:         mandate,
		Effects:         []store.OutboxEffect{effect},
	})
	if err != nil {
		// A racing replay (accept already recorded) rolled back whole: the
		// committed ticket stands, this call is a no-op observer.
		if errors.Is(err, store.ErrDuplicateAccept) {
			latest, found, err := e.store.TriggerDecision(ctx, source, tr.DeterministicID)
			if err == nil && found {
				return latest, nil
			}
		}
		return store.Decision{}, fmt.Errorf("failed to create the ticket for trigger %s/%s: %w",
			source, tr.DeterministicID, err)
	}
	return created.Decision, nil
}

// confirmEffectKey is the outbox idempotency key of a ticket's confirmation
// comment: one ticket owes exactly one acknowledgment, whatever replays.
func confirmEffectKey(ticketID string) string {
	return "confirm:" + ticketID
}

// confirmationEffect builds the queued GitHub confirmation-comment effect for
// a freshly accepted ticket: it addresses the original issue, carries the
// ticket id, and embeds the deterministic marker the channel's receipt
// idempotency relies on (docs/designs/0001 §6).
func confirmationEffect(ticketID string, mandate store.Mandate) (store.OutboxEffect, error) {
	body := fmt.Sprintf(
		"Acknowledged — opened ticket `%s` to work on this request (requested by @%s, policy %s).\n"+
			"Progress and results will be reported in this thread.\n\n%s",
		ticketID, mandate.Sender, shortHash(mandate.PolicyVersion),
		github.EffectMarker(confirmEffectKey(ticketID)))

	payload, err := json.Marshal(github.IssueCommentEffect{
		Kind:  github.EffectKindIssueComment,
		Repo:  mandate.Scope.Repo,
		Issue: mandate.Scope.Issue,
		Body:  body,
	})
	if err != nil {
		return store.OutboxEffect{}, err
	}
	return store.OutboxEffect{
		Channel:        github.ChannelGitHub,
		Address:        fmt.Sprintf("%s#%d", mandate.Scope.Repo, mandate.Scope.Issue),
		IdempotencyKey: confirmEffectKey(ticketID),
		Payload:        payload,
	}, nil
}

// shortHash truncates a policy version hash for display. The full hash lives
// in the mandate and the policy table; the comment only shows a fingerprint.
func shortHash(version string) string {
	if len(version) > 12 {
		return version[:12]
	}
	return version
}

// Input is everything the evaluator needs to judge one trigger: the trigger
// with its decoded payload, plus the Gate verdict the Decision must echo
// (docs/designs/0001 §4.1: Assessment invoke = Trigger 批次 + Thread 快照 +
// Ticket 历史 + Policy; P0 carries the single trigger — the followupOwners
// guard keeps a thread that holds a live ticket from ever reaching the
// evaluator, so a judged trigger always runs against an unowned thread).
type Input struct {
	Source          string
	DeterministicID string
	ThreadKey       string
	Payload         intake.CommentPayload
	// Strength is the gate-judged admission strength.
	Strength string
	// PolicyVersion is the policy version hash the evaluation runs under.
	PolicyVersion string
}

// assessmentWire is the JSON document sent to the evaluator agent.
type assessmentWire struct {
	Kind          string            `json:"kind"` // "assessment"
	PolicyVersion string            `json:"policy_version"`
	Strength      string            `json:"strength"`
	Trigger       assessmentTrigger `json:"trigger"`
	// DecisionSchema tells the agent the exact shape of the required
	// structured answer (outputSchema contract).
	DecisionSchema []decisionField `json:"decision_schema"`
}

type assessmentTrigger struct {
	Source          string                `json:"source"`
	DeterministicID string                `json:"deterministic_id"`
	ThreadKey       string                `json:"thread_key"`
	Payload         intake.CommentPayload `json:"payload"`
}

type decisionField struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// decisionSchema is the outputSchema contract the evaluator must answer
// with (docs/designs/0002 A2: the Decision's structured-output shape).
func decisionSchema() []decisionField {
	return []decisionField{
		{Name: "outcome", Type: "string", Required: true, Description: "the judgment", Enum: []string{"accept", "stop", "ignore", "unavailable"}},
		{Name: "strength", Type: "string", Required: false, Description: "echo of the gate strength above; required for accept"},
		{Name: "policyVersion", Type: "string", Required: false, Description: "echo of the policy version above; required for accept"},
		{Name: "reason", Type: "string", Required: false, Description: "one-sentence justification"},
	}
}

// Evaluator invokes the evaluator agent through the A2A transport and
// returns its validated Decision. It is the only place evaluator output
// enters the control plane.
type Evaluator struct {
	transport a2a.Transport
	agent     string
	// Timeout bounds one invoke. Zero uses DefaultInvokeTimeout.
	Timeout time.Duration
}

// NewEvaluator returns an Evaluator invoking the named agent through the
// transport. The agent name is the A2A agent (kagent Agent CR) that carries
// the kagent harness + outputSchema (docs/designs/0002 §3.3).
func NewEvaluator(transport a2a.Transport, agent string) *Evaluator {
	return &Evaluator{transport: transport, agent: agent}
}

// Evaluate sends the assessment invoke, parses the structured Decision and
// validates it against the gate inputs in in. Errors split three ways:
//   - ErrReject: the response is not a Decision or fails code validation →
//     the Engine records a rejection;
//   - any other error: the invoke itself failed (unreachable/timeout) →
//     the Engine records an unavailable (retryable) outcome.
func (ev *Evaluator) Evaluate(ctx context.Context, in Input) (Decision, error) {
	timeout := ev.Timeout
	if timeout <= 0 {
		timeout = DefaultInvokeTimeout
	}
	// WithTimeout honors an earlier parent deadline, so a caller-imposed
	// budget always wins.
	invokeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	message, err := assessmentMessage(in)
	if err != nil {
		return Decision{}, fmt.Errorf("failed to build the assessment message: %w", err)
	}

	raw, err := ev.transport.InvokeAgent(invokeCtx, ev.agent, message)
	if err != nil {
		return Decision{}, fmt.Errorf("evaluator %q invoke failed: %w", ev.agent, err)
	}

	dec, err := ParseDecision(raw)
	if err != nil {
		return Decision{}, err
	}
	if err := dec.Validate(in.Strength, in.PolicyVersion); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

// assessmentMessage builds the Assessment document for one trigger.
func assessmentMessage(in Input) (string, error) {
	wire := assessmentWire{
		Kind:          "assessment",
		PolicyVersion: in.PolicyVersion,
		Strength:      in.Strength,
		Trigger: assessmentTrigger{
			Source:          in.Source,
			DeterministicID: in.DeterministicID,
			ThreadKey:       in.ThreadKey,
			Payload:         in.Payload,
		},
		DecisionSchema: decisionSchema(),
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
