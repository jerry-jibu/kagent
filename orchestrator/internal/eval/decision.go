// Package eval is the Assessment half of the Eval stage (docs/designs/0001
// §3: Eval = Gate + Assessment): it invokes the evaluator agent through the
// A2A transport, parses the structured Decision out of the response, and
// validates that Decision against the Gate's inputs before the Engine
// persists it.
//
// The security boundary lives here: the evaluator only judges content, and
// its Decision must echo the gate-judged strength and policy version
// (docs/designs/0001 §7: evaluator 只做研判，无权凭空 accept). Anything the
// evaluator claims that disagrees with the Gate is a forgery attempt and is
// rejected by code — never persisted as a decision the pipeline would act on.
package eval

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// ErrReject marks evaluator output the orchestrator refuses: a response that
// is not a Decision, or a Decision whose claims fail code validation. The
// Engine persists these as store.DecisionRejected audit records.
var ErrReject = errors.New("evaluator decision rejected")

// Decision is the structured assessment output the evaluator agent must
// return (docs/designs/0001 §3: accept/stop/ignore/unavailable + strength +
// policyVersion + reason). The evaluator is a kagent harness agent with an
// outputSchema (docs/designs/0002 A2: structured output is only available on
// the kagent harness — that is why the evaluator must be one).
type Decision struct {
	Outcome  string `json:"outcome"`
	Strength string `json:"strength,omitempty"`
	// PolicyVersion is camelCase on the wire: the evaluator's enforced
	// outputSchema (deploy/kagent/agent-evaluator.yaml) is the contract
	// source, and JSON keys never match snake_case tags.
	PolicyVersion string `json:"policyVersion,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// Validate checks the Decision against the Gate's inputs (docs/designs/0001
// §7: accept 必须携带 gate 已判定的 strength + policy 版本，代码校验).
//
//   - the outcome must be in the evaluator vocabulary;
//   - any strength or policy version the Decision carries must equal the
//     gate's — a mismatch is a forged claim and is rejected;
//   - an accept must carry both, so no ticket can ever be created from an
//     authorization the Gate did not state.
func (d Decision) Validate(gateStrength, gatePolicyVersion string) error {
	switch d.Outcome {
	case store.DecisionAccept, store.DecisionStop, store.DecisionIgnore, store.DecisionUnavailable:
	default:
		return fmt.Errorf("%w: outcome %q is not in the decision vocabulary", ErrReject, d.Outcome)
	}
	if d.Strength != "" && d.Strength != gateStrength {
		return fmt.Errorf("%w: decision strength %q does not match the gate strength %q",
			ErrReject, d.Strength, gateStrength)
	}
	if d.PolicyVersion != "" && d.PolicyVersion != gatePolicyVersion {
		return fmt.Errorf("%w: decision policy version %q does not match the gate policy version %q",
			ErrReject, d.PolicyVersion, gatePolicyVersion)
	}
	if d.Outcome == store.DecisionAccept {
		if d.Strength == "" {
			return fmt.Errorf("%w: accept decision does not carry the gate strength", ErrReject)
		}
		if d.PolicyVersion == "" {
			return fmt.Errorf("%w: accept decision does not carry the policy version", ErrReject)
		}
	}
	return nil
}

// ParseDecision decodes the evaluator's final response text into a Decision.
// The evaluator answers with exactly one Decision JSON object (outputSchema
// contract), so a response that is not one — prose, an empty string, or an
// object without an outcome — is rejected rather than mined for keywords;
// prompt injection in comment text must not be able to shape the verdict.
func ParseDecision(raw string) (Decision, error) {
	var d Decision
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return Decision{}, fmt.Errorf("%w: response is not a Decision JSON object: %v", ErrReject, err)
	}
	if d.Outcome == "" {
		return Decision{}, fmt.Errorf("%w: response carries no outcome field", ErrReject)
	}
	return d, nil
}
