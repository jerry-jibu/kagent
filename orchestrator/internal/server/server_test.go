package server_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/server"
)

func TestHealthz(t *testing.T) {
	t.Run("Given a server whose readiness holds, when a caller GETs /healthz, assert that it answers 200 ok", func(t *testing.T) {
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "abc123", time.Now(), true },
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

func TestAdminPolicy(t *testing.T) {
	t.Run("Given a loaded policy, when a caller GETs /admin/policy, assert that the response carries the current policy version hash", func(t *testing.T) {
		loadedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "deadbeef", loadedAt, true },
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
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/policy", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got %d", rec.Code)
		}
	})
}
