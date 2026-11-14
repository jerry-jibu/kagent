package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/gate"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// DefaultInvokeTimeout bounds one evaluator invoke. The evaluator is an
// agent turn — bounded, but not instant.
const DefaultInvokeTimeout = 60 * time.Second

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
	// (docs/designs/0001 §7: 门在编排器，不在模型).
	verdict := gate.Judge(pol, pl)
	if !verdict.Allowed {
		return e.record(ctx, source, tr.DeterministicID, store.DecisionInput{
			Outcome:       store.DecisionDenied,
			PolicyVersion: pol.Version,
			Reason:        verdict.Reason,
		})
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

// Input is everything the evaluator needs to judge one trigger: the trigger
// with its decoded payload, plus the Gate verdict the Decision must echo
// (docs/designs/0001 §4.1: Assessment invoke = Trigger 批次 + Thread 快照 +
// Ticket 历史 + Policy; P0 carries the single trigger — a thread holds no
// live ticket before the accept, and the policy version pins the judgment).
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
		{Name: "policy_version", Type: "string", Required: false, Description: "echo of the policy version above; required for accept"},
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
