package a2a_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
)

// gatewayScript is what one recorded JSON-RPC request looked like on the
// wire.
type gatewayScript struct {
	Handler func(w http.ResponseWriter, r *http.Request) bool
}

func TestHTTPTransportInvokeAgent(t *testing.T) {
	ctx := context.Background()

	// recordedRequest captures the request shape assertions need.
	type recordedRequest struct {
		Path       string
		Method     string
		A2AVersion string
		Body       struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			ID      string          `json:"id"`
			Params  json.RawMessage `json:"params"`
		}
	}

	newGateway := func(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*recordedRequest, *a2a.HTTPTransport) {
		t.Helper()
		rec := &recordedRequest{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				JSONRPC string          `json:"jsonrpc"`
				Method  string          `json:"method"`
				ID      string          `json:"id"`
				Params  json.RawMessage `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.Path = r.URL.Path
			rec.Method = r.Method
			rec.A2AVersion = r.Header.Get("A2A-Version")
			rec.Body.JSONRPC = body.JSONRPC
			rec.Body.Method = body.Method
			rec.Body.ID = body.ID
			rec.Body.Params = body.Params
			respond(w, r)
		}))
		t.Cleanup(srv.Close)
		return rec, a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: srv.URL})
	}

	t.Run("Given a gateway answering with a final Message, when an agent is invoked, assert that the response text comes back and the request speaks the gateway's wire contract", func(t *testing.T) {
		rec, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"message":{"role":"ROLE_AGENT","parts":[{"text":"{\"outcome\":\"accept\",\"strength\":\"explicit\"}"}],"messageId":"m1"}}}`))
		})

		got, err := tr.InvokeAgent(ctx, "kagent/evaluator", "the assessment document")
		if err != nil {
			t.Fatalf("InvokeAgent failed: %v", err)
		}
		if !strings.Contains(got, "accept") {
			t.Fatalf("response text = %q, want the evaluator's Decision JSON", got)
		}

		if rec.Path != "/agents/kagent/evaluator" {
			t.Fatalf("request path = %q, want the agent route", rec.Path)
		}
		if rec.Body.Method != "SendMessage" {
			t.Fatalf("rpc method = %q, want SendMessage", rec.Body.Method)
		}
		if rec.A2AVersion != "1.0" {
			t.Fatalf("A2A-Version header = %q, want the served protocol version 1.0 (the gateway rejects absent/0.3 with VERSION_NOT_SUPPORTED)", rec.A2AVersion)
		}
		if rec.Body.JSONRPC != "2.0" || rec.Body.ID == "" {
			t.Fatalf("request envelope = %+v, want jsonrpc 2.0 with an id", rec.Body)
		}
		var params struct {
			Message struct {
				Role      string `json:"role"`
				MessageID string `json:"messageId"`
				Parts     []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Params, &params); err != nil {
			t.Fatalf("params are not JSON: %v", err)
		}
		if params.Message.Role != "ROLE_USER" {
			t.Fatalf("message role = %q, want ROLE_USER", params.Message.Role)
		}
		if params.Message.MessageID == "" {
			t.Fatal("message must carry a messageId")
		}
		if len(params.Message.Parts) != 1 || params.Message.Parts[0].Text != "the assessment document" {
			t.Fatalf("parts = %+v, want the assessment text", params.Message.Parts)
		}
	})

	t.Run("Given a bare agent name, when an agent is invoked, assert that it routes under the default kagent namespace", func(t *testing.T) {
		rec, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"message":{"role":"ROLE_AGENT","parts":[{"text":"ok"}],"messageId":"m1"}}}`))
		})
		if _, err := tr.InvokeAgent(ctx, "evaluator", "hi"); err != nil {
			t.Fatalf("InvokeAgent failed: %v", err)
		}
		if rec.Path != "/agents/kagent/evaluator" {
			t.Fatalf("request path = %q, want the default-namespace route", rec.Path)
		}
	})

	t.Run("Given a gateway answering with a completed Task carrying the agent's final turn in history, when an agent is invoked, assert that the last agent message's text comes back", func(t *testing.T) {
		_, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"task":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"},"history":[{"role":"ROLE_USER","parts":[{"text":"the assessment document"}],"messageId":"m0"},{"role":"ROLE_AGENT","parts":[{"text":"{\"outcome\":\"ignore\"}"}],"messageId":"m1"}]}}}`))
		})

		got, err := tr.InvokeAgent(ctx, "kagent/evaluator", "the assessment document")
		if err != nil {
			t.Fatalf("InvokeAgent failed: %v", err)
		}
		if !strings.Contains(got, "ignore") {
			t.Fatalf("response text = %q, want the agent's final turn", got)
		}
	})

	t.Run("Given a gateway answering with a Task whose answer lives in an artifact, when an agent is invoked, assert that the artifact text comes back", func(t *testing.T) {
		_, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"task":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"},"artifacts":[{"name":"decision","parts":[{"text":"{\"outcome\":\"stop\"}"}]}]}}}`))
		})

		got, err := tr.InvokeAgent(ctx, "kagent/evaluator", "the assessment document")
		if err != nil {
			t.Fatalf("InvokeAgent failed: %v", err)
		}
		if !strings.Contains(got, "stop") {
			t.Fatalf("response text = %q, want the artifact's decision", got)
		}
	})

	t.Run("Given a gateway answering with a JSON-RPC error, when an agent is invoked, assert that the error surfaces naming the message", func(t *testing.T) {
		_, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","error":{"code":-32603,"message":"agent queue full"}}`))
		})

		_, err := tr.InvokeAgent(ctx, "kagent/evaluator", "hi")
		if err == nil {
			t.Fatal("a JSON-RPC error must surface")
		}
		if !strings.Contains(err.Error(), "agent queue full") {
			t.Fatalf("error = %v, want the gateway's message", err)
		}
	})

	t.Run("Given an unreachable gateway, when an agent is invoked, assert that the failure surfaces as an error", func(t *testing.T) {
		tr := a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: "http://127.0.0.1:1"})
		if _, err := tr.InvokeAgent(ctx, "kagent/evaluator", "hi"); err == nil {
			t.Fatal("an unreachable gateway must fail the invoke")
		}
	})

	t.Run("Given an agent route with an empty segment, when the transport resolves the route, assert that the invoke is refused", func(t *testing.T) {
		_, tr := newGateway(t, func(w http.ResponseWriter, r *http.Request) {})
		if _, err := tr.InvokeAgent(ctx, "/evaluator", "hi"); err == nil {
			t.Fatal("an empty namespace must be refused")
		}
	})
}
