package a2a

import (
	"errors"
	"strconv"
)

// Dispatch-failure classification (PR #11 review, Required): a failed
// Dispatch is either OBSERVABLE — the gateway demonstrably received and
// refused the send, so no task can have landed and an immediate resend is
// safe — or AMBIGUOUS — the transport died without an answer (client
// timeout, connection reset, canceled context), so the gateway may have
// accepted the SendMessage and a task may be live. Resending an ambiguous
// failure with a fresh message id forks a second worker onto the same ticket
// workspace; the dispatcher must instead route it through the write-ahead
// adopt/quarantine recovery path. The classification is typed at this seam
// because only the transport knows whether an answer arrived.

// StatusDispatchError marks an observably failed send: the gateway answered
// the request (a non-2xx HTTP status, a JSON-RPC error, an unusable answer
// body), or the request was never sendable (an invalid agent route). In
// every case this send cannot have landed a task.
type StatusDispatchError struct {
	// StatusCode is the HTTP status the gateway answered with; 0 when the
	// failure is not a status response (JSON-RPC error, unusable body, bad
	// route).
	StatusCode int
	// Detail is the human-readable failure context.
	Detail string
}

func (e *StatusDispatchError) Error() string {
	if e.StatusCode != 0 {
		return e.Detail + ": HTTP " + strconv.Itoa(e.StatusCode)
	}
	return e.Detail
}

// AmbiguousDispatchError marks a send whose outcome at the gateway is
// unknown: no answer arrived, so the gateway may still be executing (or may
// have landed) the task.
type AmbiguousDispatchError struct {
	// Err is the underlying transport failure.
	Err error
}

func (e *AmbiguousDispatchError) Error() string {
	return "dispatch outcome at the gateway is unknown: " + e.Err.Error()
}

func (e *AmbiguousDispatchError) Unwrap() error { return e.Err }

// IsAmbiguousDispatchFailure reports whether a Dispatch failure left the
// outcome at the gateway unobservable. A StatusDispatchError anywhere in the
// chain is observable (the gateway answered and refused — safe to resend
// immediately); every other failure class — timeouts, connection resets,
// canceled contexts, and any transport error carrying no answer evidence —
// is ambiguous, because "the send may have landed" is the only safe reading
// of an absent answer.
func IsAmbiguousDispatchFailure(err error) bool {
	if err == nil {
		return false
	}
	var status *StatusDispatchError
	if errors.As(err, &status) {
		return false
	}
	return true
}
