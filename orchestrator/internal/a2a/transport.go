// Package a2a is the orchestrator's A2A seam (spec: A2ATransport): every
// interaction with kagent agents crosses this interface. The evaluator
// assessment invoke lands in T3; worker dispatch and task reconciliation
// extend the same interface in later tickets. Production implementations
// speak A2A to the kagent Gateway — the orchestrator is kagent's caller,
// with the same standing as the CLI and UI (docs/designs/0001 §5).
package a2a

import "context"

// Transport is the orchestrator's A2A seam. Tests inject a Fake; production
// dials the kagent Gateway.
type Transport interface {
	// InvokeAgent sends one non-streaming message to the named agent and
	// returns the agent's final response text. It blocks until the task
	// reaches a terminal state or ctx is done — an unreachable gateway, a
	// hung task, and a canceled context all surface as errors, which the
	// caller turns into retryable unavailable outcomes (docs/designs/0001
	// §8 failure semantics).
	InvokeAgent(ctx context.Context, agent, message string) (string, error)
}
