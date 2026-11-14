// Package a2a is the orchestrator's A2A seam (spec: A2ATransport): every
// interaction with kagent agents crosses this interface. The evaluator
// assessment invoke landed in T3; the T6 dispatch chain (Dispatch with
// ReturnImmediately + PushConfig, and the task status query the Pass beat
// reconciles against) extends the same interface. Production implementations
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

	// Dispatch sends ticket work as a task and returns immediately with the
	// task identity (docs/designs/0001 D2: SendMessage + ReturnImmediately +
	// PushConfig). The task continues executing after the call returns;
	// terminal states arrive at the registered push receiver, and TaskState
	// queries remain the correctness backstop.
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)

	// TaskState fetches one task's current snapshot. A task the gateway does
	// not know fails with ErrTaskNotFound.
	TaskState(ctx context.Context, agent, taskID string) (Task, error)
}
