package dispatch_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
)

// mintGatewayKeypair builds the gateway side of the push JWT contract.
func mintGatewayKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

// signPushToken mirrors the gateway's PushJWTSigner: Ed25519 EdDSA, kid =
// base64url(sha256(public)), claims taskId/iss/aud/exp(5min).
func signPushToken(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey, taskID, issuer, audience string) string {
	t.Helper()
	digest := sha256.Sum256(pub)
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"taskId": taskID,
		"iss":    issuer,
		"aud":    audience,
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
		"nbf":    time.Now().Unix(),
		"iat":    time.Now().Unix(),
	})
	token.Header["kid"] = base64.RawURLEncoding.EncodeToString(digest[:])
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("sign push token: %v", err)
	}
	return signed
}

// pemFor renders the Ed25519 public key as the PEM form the flag accepts.
func pemFor(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal PKIX public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// Given the gateway's pinned Ed25519 public key, when pushes arrive with the
// signed Bearer credential, assert: a correctly signed token for the event's
// task applies (200); a token bound to a DIFFERENT task is 403; garbage is
// 401; a foreign-key signature is 401 (kid pins the gateway key); a missing
// credential is 401. The verifier is what keeps the unauthenticated P0
// listener from terminating tickets or injecting Report text (PR #11
// Required #12).
func TestPushReceiverJWTVerification(t *testing.T) {
	ctx := context.Background()
	st, _, assign := acceptFixture(t)

	// Bind the fixture assignment so a valid push resolves via ApplyTerminal.
	if _, err := st.BeginDispatchAttempt(ctx, assign.ID); err != nil {
		t.Fatalf("BeginDispatchAttempt: %v", err)
	}

	pub, priv := mintGatewayKeypair(t)
	const issuer = "kagent-gateway"
	const audience = "http://orchestrator:8080/a2a/push"
	verifier, err := dispatch.NewPushJWTVerifier(pemFor(t, pub), issuer, audience)
	if err != nil {
		t.Fatalf("NewPushJWTVerifier: %v", err)
	}
	receiver := dispatch.NewReceiver(dispatch.New(st, a2a.NewFake(), dispatch.Options{WorkerAgent: "kagent/worker"}), verifier)
	handler := http.HandlerFunc(receiver.ServeHTTP)

	const boundTask = "task-bound"
	goodToken := signPushToken(t, priv, pub, boundTask, issuer, audience)
	otherToken := signPushToken(t, priv, pub, "task-other", issuer, audience)

	body := func(taskID string) string {
		return fmt.Sprintf(`{"task":{"id":"%s","contextId":"ctx-jwt","status":{"state":"TASK_STATE_WORKING"}}}`, taskID)
	}
	post := func(token, taskID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, dispatch.PushReceiverPath, strings.NewReader(body(taskID)))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	t.Run("Given a correctly signed token for the event's task, when the push arrives, assert 200 and the observation applies", func(t *testing.T) {
		rec := post(goodToken, boundTask)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		// The verified observation flowed into the adoption ledger keyed by
		// the event's context id — a push alone does not bind (adoption
		// happens on the next dispatch tick, after gateway verification).
		obs, found, err := st.TakeOrphanTask(ctx, "ctx-jwt")
		if err != nil || !found {
			t.Fatalf("TakeOrphanTask = %+v, %v, %v; want the verified push recorded for adoption", obs, found, err)
		}
		if obs.TaskID != boundTask {
			t.Fatalf("orphan observation task = %q, want task-bound", obs.TaskID)
		}
	})

	t.Run("Given a valid token bound to a different task, when the push arrives for task-bound, assert 403 (claim/event mismatch)", func(t *testing.T) {
		rec := post(otherToken, boundTask)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("Given a garbage credential, when the push arrives, assert 401", func(t *testing.T) {
		if rec := post("not-a-jwt", boundTask); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("Given a token signed by a foreign key, when the push arrives, assert 401 (kid pins the gateway key)", func(t *testing.T) {
		foreignPub, foreignPriv := mintGatewayKeypair(t)
		foreign := signPushToken(t, foreignPriv, foreignPub, boundTask, issuer, audience)
		if rec := post(foreign, boundTask); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (foreign key must fail)", rec.Code)
		}
	})

	t.Run("Given verification enabled and no credential, when the push arrives, assert 401", func(t *testing.T) {
		if rec := post("", boundTask); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("Given an expired token, when the push arrives, assert 401 (5-minute gateway validity is enforced)", func(t *testing.T) {
		expired := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
			"taskId": boundTask,
			"iss":    issuer,
			"aud":    audience,
			"exp":    time.Now().Add(-time.Minute).Unix(),
			"nbf":    time.Now().Add(-2 * time.Minute).Unix(),
			"iat":    time.Now().Add(-2 * time.Minute).Unix(),
		})
		digest := sha256.Sum256(pub)
		expired.Header["kid"] = base64.RawURLEncoding.EncodeToString(digest[:])
		signed, err := expired.SignedString(priv)
		if err != nil {
			t.Fatalf("sign expired token: %v", err)
		}
		if rec := post(signed, boundTask); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (expired)", rec.Code)
		}
	})
}
