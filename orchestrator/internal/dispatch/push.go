package dispatch

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
)

// Receiver is the A2A push receiver endpoint (docs/designs/0001 D2): the
// gateway POSTs task status events here; each event funnels into
// Dispatcher.ApplyTerminal, which is idempotent, so gateway retries and the
// Pass reconcile backfill cannot double count. Push is an optimization — a
// lost push is repaired by the Pass beat within two ticks.
//
// P0 mounts the endpoint inside the cluster boundary without request
// authentication; the gateway's signed-JWT verification is future work
// (go/core/internal/service/session PushJWTSigner), tracked for the P0
// hardening pass.
type Receiver struct {
	dispatcher *Dispatcher
}

// NewReceiver returns the push receiver backed by the dispatcher.
func NewReceiver(d *Dispatcher) *Receiver {
	return &Receiver{dispatcher: d}
}

// pushEvent mirrors a2a-go's StreamResponse event wrapper: exactly one of the
// event fields carries the payload. Message/ArtifactUpdate are tracked only
// to recognize well-formed events that carry no task status.
type pushEvent struct {
	Task           *pushTaskEnvelope         `json:"task"`
	StatusUpdate   *pushStatusUpdateEnvelope `json:"statusUpdate"`
	Message        *json.RawMessage          `json:"message"`
	ArtifactUpdate *json.RawMessage          `json:"artifactUpdate"`
}

type pushTaskEnvelope struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    pushStatus `json:"status"`
}

type pushStatusUpdateEnvelope struct {
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId"`
	Status    pushStatus `json:"status"`
}

type pushStatus struct {
	State   string       `json:"state"`
	Message *pushMessage `json:"message,omitempty"`
}

type pushMessage struct {
	Parts []struct {
		Text string `json:"text"`
	} `json:"parts"`
}

func (m *pushMessage) text() string {
	if m == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range m.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// maxPushBodyBytes bounds one pushed event. A2A task events are small; a
// larger body is malformed for this endpoint, and the receiver is
// unauthenticated in P0 — an unbounded decode would let one oversized POST
// OOM the process holding the instance lock (PR #11 review, Required).
const maxPushBodyBytes = 4 << 20 // 4 MiB

// ServeHTTP accepts one pushed event and registers the observation.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var event pushEvent
	req.Body = http.MaxBytesReader(w, req.Body, maxPushBodyBytes)
	if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writePushJSON(w, http.StatusRequestEntityTooLarge, false,
				"push event exceeds the body bound")
			return
		}
		writePushJSON(w, http.StatusBadRequest, false, "event is not an A2A push event: "+err.Error())
		return
	}

	if !event.knownEnvelope() {
		// Malformed durable data is rejected, never silently omitted: a
		// body matching no A2A event envelope must not read as accepted.
		writePushJSON(w, http.StatusBadRequest, false, "event is not an A2A push event")
		return
	}

	task, ok := event.task()
	if !ok {
		// A well-formed event that carries no task status (e.g. a bare
		// message or artifact update): nothing to register — accept it so
		// the gateway does not retry a permanently non-bindable payload.
		writePushJSON(w, http.StatusOK, false, "event carries no task status")
		return
	}

	_, err := r.dispatcher.ApplyTerminal(req.Context(), task)
	switch {
	case err == nil:
		writePushJSON(w, http.StatusOK, true, "")
	case errors.Is(err, ErrUnknownTask):
		// The task belongs to no assignment — most likely a dispatch whose
		// binding write was lost to a crash (write-ahead intent, PR #11 C3).
		// Record it for adoption instead of discarding: the restart
		// recovery binds and applies it rather than re-sending. Adoption
		// verifies the task at the gateway before trusting it, so an
		// unverified push cannot spoof a binding.
		if recErr := r.dispatcher.RecordUnclaimedTask(req.Context(), task); recErr != nil {
			// A generic reason in 5xx bodies: store internals stay off the
			// wire (PR #11 review, Required).
			writePushJSON(w, http.StatusInternalServerError, false, "failed to record the unclaimed task for adoption")
			return
		}
		writePushJSON(w, http.StatusOK, true, "task recorded for adoption")
	default:
		writePushJSON(w, http.StatusInternalServerError, false, "failed to apply the pushed task observation")
	}
}

// knownEnvelope reports whether the decoded body matches any A2A event shape.
func (e pushEvent) knownEnvelope() bool {
	return e.Task != nil || e.StatusUpdate != nil || e.Message != nil || e.ArtifactUpdate != nil
}

// task extracts the observable task from whichever envelope carried it.
func (e pushEvent) task() (a2a.Task, bool) {
	switch {
	case e.Task != nil:
		if e.Task.ID == "" {
			return a2a.Task{}, false
		}
		return a2a.Task{
			ID:         e.Task.ID,
			ContextID:  e.Task.ContextID,
			State:      e.Task.Status.State,
			StatusText: e.Task.Status.Message.text(),
		}, true
	case e.StatusUpdate != nil:
		if e.StatusUpdate.TaskID == "" {
			return a2a.Task{}, false
		}
		return a2a.Task{
			ID:         e.StatusUpdate.TaskID,
			ContextID:  e.StatusUpdate.ContextID,
			State:      e.StatusUpdate.Status.State,
			StatusText: e.StatusUpdate.Status.Message.text(),
		}, true
	default:
		return a2a.Task{}, false
	}
}

func writePushJSON(w http.ResponseWriter, status int, accepted bool, reason string) {
	body := map[string]any{"accepted": accepted}
	if reason != "" {
		body["reason"] = reason
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
