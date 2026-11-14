package a2a_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
)

// TestHTTPTransportDispatch pins the dispatch wire contract: SendMessage with
// ReturnImmediately + PushConfig registration, immediate task identity back,
// and the session-continuity contextId.
func TestHTTPTransportDispatch(t *testing.T) {
	ctx := context.Background()

	type recordedParams struct {
		Message struct {
			Role      string `json:"role"`
			MessageID string `json:"messageId"`
			ContextID string `json:"contextId"`
			Parts     []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"message"`
		Config struct {
			ReturnImmediately bool `json:"returnImmediately"`
			PushConfig        struct {
				URL    string `json:"url"`
				ID     string `json:"id"`
				Tenant string `json:"tenant"`
			} `json:"taskPushNotificationConfig"`
		} `json:"configuration"`
	}

	newGateway := func(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, params recordedParams)) (*recordedParams, *a2a.HTTPTransport) {
		t.Helper()
		rec := &recordedParams{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				JSONRPC string         `json:"jsonrpc"`
				Method  string         `json:"method"`
				ID      string         `json:"id"`
				Params  recordedParams `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			*rec = body.Params
			respond(w, r, *rec)
		}))
		t.Cleanup(srv.Close)
		return rec, a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: srv.URL})
	}

	t.Run("Given a gateway answering ReturnImmediately with a submitted task, when ticket work is dispatched, assert that the task identity comes back and the request registers the push receiver", func(t *testing.T) {
		rec, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request, params recordedParams) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"id":"task-9","contextId":"ctx-9","status":{"state":"TASK_STATE_SUBMITTED"}}}`))
		})

		result, err := tr.Dispatch(ctx, a2a.DispatchRequest{
			Agent:   "kagent/worker",
			Message: "the dispatch document",
			PushURL: "http://orchestrator:8080/a2a/push",
			PushID:  "push-ticket-9-1",
		})
		if err != nil {
			t.Fatalf("Dispatch failed: %v", err)
		}
		if result.TaskID != "task-9" || result.ContextID != "ctx-9" || result.State != a2a.StateSubmitted {
			t.Fatalf("result = %+v, want the gateway's task identity", result)
		}

		if rec.Message.Role != "ROLE_USER" || rec.Message.MessageID == "" {
			t.Fatalf("message = %+v, want a ROLE_USER message with an id", rec.Message)
		}
		if len(rec.Message.Parts) != 1 || rec.Message.Parts[0].Text != "the dispatch document" {
			t.Fatalf("parts = %+v, want the dispatch document", rec.Message.Parts)
		}
		if rec.Message.ContextID != "" {
			t.Fatalf("contextId = %q, want it absent on a first dispatch", rec.Message.ContextID)
		}
		if !rec.Config.ReturnImmediately {
			t.Fatal("configuration.returnImmediately must be set (docs/designs/0001 D2)")
		}
		if rec.Config.PushConfig.URL != "http://orchestrator:8080/a2a/push" || rec.Config.PushConfig.ID != "push-ticket-9-1" {
			t.Fatalf("push config = %+v, want the receiver URL and registration id", rec.Config.PushConfig)
		}
		if rec.Config.PushConfig.Tenant != "kagent/worker" {
			t.Fatalf("push tenant = %q, want the agent route", rec.Config.PushConfig.Tenant)
		}
	})

	t.Run("Given a continuation dispatch carrying the ticket session's contextId, when it is sent, assert that the contextId travels on the message", func(t *testing.T) {
		rec, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request, params recordedParams) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"id":"task-10","contextId":"ctx-9","status":{"state":"TASK_STATE_SUBMITTED"}}}`))
		})

		result, err := tr.Dispatch(ctx, a2a.DispatchRequest{Agent: "kagent/worker", Message: "continue", ContextID: "ctx-9"})
		if err != nil {
			t.Fatalf("Dispatch failed: %v", err)
		}
		if result.ContextID != "ctx-9" {
			t.Fatalf("result contextId = %q, want the ticket session's contextId", result.ContextID)
		}
		if rec.Message.ContextID != "ctx-9" {
			t.Fatalf("message contextId = %q, want the same session (同一 ticket 续发 contextId 不变)", rec.Message.ContextID)
		}
	})

	t.Run("Given a gateway answering a dispatch with an error, when ticket work is dispatched, assert that the failure surfaces (the unresponsive-worker path)", func(t *testing.T) {
		_, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request, params recordedParams) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","error":{"code":-32603,"message":"runtime unreachable"}}`))
		})
		if _, err := tr.Dispatch(ctx, a2a.DispatchRequest{Agent: "kagent/worker", Message: "x"}); err == nil {
			t.Fatal("a JSON-RPC dispatch error must surface")
		}
	})

	t.Run("Given a gateway answering a dispatch with a bare Message instead of a task, when ticket work is dispatched, assert that the dispatch is refused for lack of a task identity", func(t *testing.T) {
		_, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request, params recordedParams) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"message":{"role":"ROLE_AGENT","parts":[{"text":"done"}],"messageId":"m1"}}}`))
		})
		if _, err := tr.Dispatch(ctx, a2a.DispatchRequest{Agent: "kagent/worker", Message: "x"}); err == nil {
			t.Fatal("a dispatch without a task identity must be refused")
		}
	})
}

// TestHTTPTransportTaskState pins the reconcile wire contract: GetTask by id,
// the task snapshot back, and the task-not-found sentinel the Pass beat's
// red line keys off.
func TestHTTPTransportTaskState(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a gateway holding a terminal task, when the task is queried, assert that the snapshot including the status text comes back", func(t *testing.T) {
		var gotParams struct {
			ID string `json:"id"`
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body.Params)
			_ = json.Unmarshal(raw, &gotParams)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"id":"task-9","contextId":"ctx-9","status":{"state":"TASK_STATE_COMPLETED","message":{"role":"ROLE_AGENT","parts":[{"text":"{\"kind\":\"report\",\"state\":\"ready\"}"}],"messageId":"m9"}}}}`))
		}))
		defer srv.Close()
		tr := a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: srv.URL})

		task, err := tr.TaskState(ctx, "kagent/worker", "task-9")
		if err != nil {
			t.Fatalf("TaskState failed: %v", err)
		}
		if task.ID != "task-9" || task.State != a2a.StateCompleted {
			t.Fatalf("task = %+v, want the terminal snapshot", task)
		}
		if !strings.Contains(task.StatusText, "ready") {
			t.Fatalf("status text = %q, want the worker's report document", task.StatusText)
		}
		if gotParams.ID != "task-9" {
			t.Fatalf("GetTask params id = %q, want the queried task", gotParams.ID)
		}
	})

	t.Run("Given a task the gateway does not know, when it is queried, assert that ErrTaskNotFound surfaces (the red-line trigger)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","error":{"code":-32001,"message":"task not found"}}`))
		}))
		defer srv.Close()
		tr := a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: srv.URL})

		if _, err := tr.TaskState(ctx, "kagent/worker", "task-gone"); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Fatalf("err = %v, want ErrTaskNotFound", err)
		}
	})
}
