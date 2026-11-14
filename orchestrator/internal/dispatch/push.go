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

// ServeHTTP accepts one pushed event and registers the observation.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var event pushEvent
	if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
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
		writePushJSON(w, http.StatusNotFound, false, err.Error())
	default:
		writePushJSON(w, http.StatusInternalServerError, false, err.Error())
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
