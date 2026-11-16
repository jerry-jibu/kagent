package a2a_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
)

// The dispatch-failure classification boundary (PR #11 review, Required):
// a send the gateway answered and refused is observable — an immediate
// resend cannot fork a second task; a send that died without an answer is
// ambiguous — the gateway may have landed it, so the dispatcher must
// quarantine instead of resending.
func TestIsAmbiguousDispatchFailure(t *testing.T) {
	wrappedStatus := fmt.Errorf("dispatch of assignment a-1 failed: %w",
		&a2a.StatusDispatchError{StatusCode: 503, Detail: "SendMessage of agent kagent/worker was refused"})
	wrappedAmbiguous := fmt.Errorf("SendMessage of agent kagent/worker failed: %w",
		&url.Error{Op: "Post", URL: "http://gateway.invalid", Err: errors.New("connection reset by peer")})

	scenarios := []struct {
		name string
		err  error
		want bool
	}{
		{"a nil error", nil, false},
		{"an HTTP status the gateway answered with", &a2a.StatusDispatchError{StatusCode: 503, Detail: "refused"}, false},
		{"a wrapped status error", wrappedStatus, false},
		{"a JSON-RPC refusal (status carries the answer)", &a2a.StatusDispatchError{Detail: "JSON-RPC error -32603"}, false},
		{"an unsendable route", &a2a.StatusDispatchError{Detail: "agent route must be name or namespace/name"}, false},
		{"a typed ambiguous failure", &a2a.AmbiguousDispatchError{Err: errors.New("client timeout")}, true},
		{"a typed ambiguous failure in a chain", wrappedAmbiguous, true},
		{"a context deadline", context.DeadlineExceeded, true},
		{"a plain transport error with no answer evidence", errors.New("connection reset by peer"), true},
	}
	for _, sc := range scenarios {
		t.Run("Given "+sc.name+", when the ambiguity predicate classifies it, assert that the classification is "+boolWord(sc.want), func(t *testing.T) {
			if got := a2a.IsAmbiguousDispatchFailure(sc.err); got != sc.want {
				t.Fatalf("IsAmbiguousDispatchFailure(%v) = %v, want %v", sc.err, got, sc.want)
			}
		})
	}
}

func boolWord(b bool) string {
	if b {
		return "ambiguous"
	}
	return "observable"
}

// The production transport types its failures at the seam: an unreachable or
// hanging gateway is ambiguous; a status/JSON-RPC answer is observable.
func TestHTTPTransportDispatchFailureClassification(t *testing.T) {
	t.Run("Given a gateway that hangs past the client timeout, when ticket work is dispatched, assert that the failure is classified ambiguous — the send may have landed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"task":{"id":"task-late","contextId":"ctx-late","status":{"state":"TASK_STATE_SUBMITTED"}}}}`))
		}))
		t.Cleanup(srv.Close)
		tr := a2a.NewHTTPTransport(a2a.HTTPConfig{
			BaseURL: srv.URL,
			Client:  &http.Client{Timeout: 20 * time.Millisecond},
		})

		_, err := tr.Dispatch(context.Background(), a2a.DispatchRequest{Agent: "kagent/worker", Message: "x"})
		if err == nil {
			t.Fatal("a hung gateway must fail the dispatch")
		}
		if !a2a.IsAmbiguousDispatchFailure(err) {
			t.Fatalf("err = %v, want the timeout classified ambiguous", err)
		}
		var amb *a2a.AmbiguousDispatchError
		if !errors.As(err, &amb) {
			t.Fatalf("err = %v, want the typed AmbiguousDispatchError in the chain", err)
		}
	})

	t.Run("Given a gateway that refuses the send with an HTTP status, when ticket work is dispatched, assert that the failure is classified observable — the gateway answered", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		tr := a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: srv.URL})

		_, err := tr.Dispatch(context.Background(), a2a.DispatchRequest{Agent: "kagent/worker", Message: "x"})
		if err == nil {
			t.Fatal("a refusing gateway must fail the dispatch")
		}
		if a2a.IsAmbiguousDispatchFailure(err) {
			t.Fatalf("err = %v, want the status answer classified observable", err)
		}
		var status *a2a.StatusDispatchError
		if !errors.As(err, &status) || status.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want the typed StatusDispatchError with HTTP 503", err)
		}
	})

	t.Run("Given a gateway answering a dispatch with a JSON-RPC error, when ticket work is dispatched, assert that the failure is classified observable — the gateway received and answered the send", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","error":{"code":-32603,"message":"runtime unreachable"}}`))
		}))
		t.Cleanup(srv.Close)
		tr := a2a.NewHTTPTransport(a2a.HTTPConfig{BaseURL: srv.URL})

		_, err := tr.Dispatch(context.Background(), a2a.DispatchRequest{Agent: "kagent/worker", Message: "x"})
		if err == nil {
			t.Fatal("a JSON-RPC dispatch error must surface")
		}
		if a2a.IsAmbiguousDispatchFailure(err) {
			t.Fatalf("err = %v, want the JSON-RPC answer classified observable", err)
		}
	})
}
