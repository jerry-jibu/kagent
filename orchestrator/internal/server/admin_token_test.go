package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Given an admin token is configured, when a mutating admin call arrives
// without (or with a wrong) Bearer credential, assert 401 — a forged
// in-cluster retry/stop drives the ticket machine (PR #11 Required #12).
func TestAdminTokenGuard(t *testing.T) {
	const token = "secret-operator-token"
	h := newAdminHandler(&fakeAdmin{}, token)

	t.Run("Given no credential, when POST retry arrives, assert 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/tickets/t1/retry", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("Given a wrong credential, when POST stop arrives, assert 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/tickets/t1/stop", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("Given the right credential, when POST retry arrives, assert the handler runs (not 401)", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/tickets/nonexistent/retry", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("correct token must pass the guard, got 401")
		}
	})

	t.Run("Given a token configured, when an admin READ arrives without credential, assert it stays open (reads are the human's view)", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/needs-attention", nil))
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("reads must not be guarded")
		}
	})
}
