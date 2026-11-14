package server_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/server"
)

func TestHealthz(t *testing.T) {
	t.Run("Given a server whose readiness holds, when a caller GETs /healthz, assert that it answers 200 ok", func(t *testing.T) {
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "abc123", time.Now(), true },
			nil,
			nil,
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Given a server whose readiness fails, when a caller GETs /healthz, assert that it answers 503 with the reason", func(t *testing.T) {
		h := server.NewHandler(
			func() error { return errors.New("migrations not applied") },
			func() (string, time.Time, bool) { return "abc123", time.Now(), true },
			nil,
			nil,
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got %d", rec.Code)
		}
		if body := rec.Body.String(); body == "" {
			t.Fatal("503 body must carry the failure reason")
		}
	})
}

func TestPushEndpoint(t *testing.T) {
	t.Run("Given a mounted push receiver, when the gateway POSTs an event, assert that the receiver sees it on the push path", func(t *testing.T) {
		received := make(chan string, 1)
		push := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- string(body)
			w.WriteHeader(http.StatusOK)
		})
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "abc123", time.Now(), true },
			push,
			nil,
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/a2a/push", strings.NewReader(`{"task":{}}`)))

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
		select {
		case body := <-received:
			if body != `{"task":{}}` {
				t.Fatalf("receiver body = %q, want the verbatim event", body)
			}
		default:
			t.Fatal("the mounted receiver must see the POST")
		}
	})

	t.Run("Given no push receiver configured, when a caller probes the push path, assert that it answers 404 rather than crashing", func(t *testing.T) {
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "abc123", time.Now(), true },
			nil,
			nil,
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/a2a/push", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", rec.Code)
		}
	})
}
func TestAdminPolicy(t *testing.T) {
	t.Run("Given a loaded policy, when a caller GETs /admin/policy, assert that the response carries the current policy version hash", func(t *testing.T) {
		loadedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "deadbeef", loadedAt, true },
			nil,
			nil,
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/policy", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
		var body struct {
			Version  string    `json:"version"`
			LoadedAt time.Time `json:"loadedAt"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response must be JSON: %v (body: %s)", err, rec.Body.String())
		}
		if body.Version != "deadbeef" {
			t.Fatalf("want version deadbeef, got %q", body.Version)
		}
		if !body.LoadedAt.Equal(loadedAt) {
			t.Fatalf("want loadedAt %v, got %v", loadedAt, body.LoadedAt)
		}
	})

	t.Run("Given no policy loaded yet, when a caller GETs /admin/policy, assert that it answers 503", func(t *testing.T) {
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "", time.Time{}, false },
			nil,
			nil,
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/policy", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got %d", rec.Code)
		}
	})
}
