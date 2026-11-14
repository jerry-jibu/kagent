package a2a

import "errors"

// A2A task state wire values (a2a-go v2 TaskState). The transport speaks the
// gateway's wire contract, so states cross this seam as their wire strings.
const (
	StateSubmitted     = "TASK_STATE_SUBMITTED"
	StateWorking       = "TASK_STATE_WORKING"
	StateCompleted     = "TASK_STATE_COMPLETED"
	StateFailed        = "TASK_STATE_FAILED"
	StateCanceled      = "TASK_STATE_CANCELED"
	StateRejected      = "TASK_STATE_REJECTED"
	StateInputRequired = "TASK_STATE_INPUT_REQUIRED"
	StateAuthRequired  = "TASK_STATE_AUTH_REQUIRED"
	StateUnspecified   = "TASK_STATE_UNSPECIFIED"
)

// ErrTaskNotFound reports that the gateway has no task under the queried id.
// The Pass beat treats it as the red-line trigger: an assignment whose task
// cannot be found marks unresolved and never auto-replays
// (docs/designs/0001 §8, docs/designs/0009 §4).
var ErrTaskNotFound = errors.New("task not found")

// IsTerminalTaskState reports whether the wire state is immutable: a task in
// a terminal state can never leave it, so reconciling against it is final.
func IsTerminalTaskState(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	default:
		return false
	}
}

// DispatchRequest is one dispatch of ticket work to a worker agent
// (docs/designs/0001 §4.1 step 5). ContextID is the ticket session's A2A
// contextId: empty starts a new session, set it to continue the same one.
// PushURL/PushID register the orchestrator's push receiver so task terminal
// states arrive without polling (docs/designs/0001 D2); the reconcile beat
// remains the correctness backstop, push is an optimization.
type DispatchRequest struct {
	Agent     string
	Message   string
	ContextID string
	PushURL   string
	PushID    string
}

// DispatchResult is the immediate answer of a ReturnImmediately dispatch:
// the task identity to register. State carries the task's state at answer
// time (typically submitted or working; the gateway may also answer with an
// already-quiescent task).
type DispatchResult struct {
	TaskID    string
	ContextID string
	State     string
}

// Task is the task snapshot the gateway answers a status query with.
// StatusText is the status message's text — the carrier of the worker's
// Report document on terminal states.
type Task struct {
	ID         string
	ContextID  string
	State      string
	StatusText string
}
