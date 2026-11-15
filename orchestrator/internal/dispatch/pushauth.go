package dispatch

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Push JWT verification (PR #11 review Required #12 / design-R3-2): the
// kagent gateway signs one short-lived Ed25519 credential per delivery
// attempt (go/core/internal/service/session PushJWTSigner: claims taskId,
// iss = gateway issuer, aud = the registered push URL, exp 5min) and sends
// it as the push's Bearer credential. The receiver verifies it before
// trusting any event — the endpoint is otherwise unauthenticated inside
// the cluster boundary, and a forged POST could terminate tickets and
// inject Report text into PR bodies.
//
// The deployment pins the GATEWAY's public half: --push-jwt-public-key
// (Ed25519 PEM) + --push-jwt-issuer; the audience is derived from
// --push-base-url + PushReceiverPath. Unset keys keep the P0 dev behavior
// (unauthenticated, loud log) so local e2e and fake-gateway setups run
// unchanged; non-dev deployments MUST set them.

// ErrPushUnauthorized marks a rejected push (bad/missing token, wrong
// claims) — logged at warn for the operator, answered 401/403.
var ErrPushUnauthorized = errors.New("push event rejected: unauthorized")

// pushJWTClaims mirrors the gateway's pushJWTClaims (session package).
type pushJWTClaims struct {
	TaskID string `json:"taskId"`
	jwt.RegisteredClaims
}

// PushJWTVerifier validates pushed events against the gateway's pinned
// Ed25519 public key. nil-safe: a zero verifier verifies nothing and the
// receiver falls back to the documented P0 unauthenticated mode.
type PushJWTVerifier struct {
	public   ed25519.PublicKey
	issuer   string
	audience string
}

// NewPushJWTVerifier parses the gateway's Ed25519 public key (PEM "PUBLIC
// KEY" form) with the expected issuer. audience is the orchestrator's own
// push receiver URL — the gateway registers it verbatim in PushConfig.
func NewPushJWTVerifier(publicKeyPEM, issuer, audience string) (*PushJWTVerifier, error) {
	if publicKeyPEM == "" || issuer == "" {
		return nil, fmt.Errorf("push JWT verifier requires both the gateway public key PEM and the issuer")
	}
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("push JWT public key must be a PEM PUBLIC KEY block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the push JWT public key: %w", err)
	}
	edKey, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("push JWT public key must be Ed25519, got %T", key)
	}
	return &PushJWTVerifier{public: edKey, issuer: issuer, audience: audience}, nil
}

// keyFunc pins EdDSA + the key-derived kid: the gateway's kid is
// base64url(sha256(public)) — recomputable here, so a mismatched key
// (wrong JWKS pinned) fails loudly instead of verifying nothing.
func (v *PushJWTVerifier) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodEd25519); !ok {
		return nil, fmt.Errorf("%w: unexpected signing method %v (want EdDSA)", ErrPushUnauthorized, token.Header["alg"])
	}
	want := base64.RawURLEncoding.EncodeToString(func() []byte {
		d := sha256.Sum256(v.public)
		return d[:]
	}())
	if kid, _ := token.Header["kid"].(string); kid != "" && kid != want {
		return nil, fmt.Errorf("%w: token kid %q does not match the pinned gateway key", ErrPushUnauthorized, kid)
	}
	return v.public, nil
}

// VerifyRequest checks the request's Bearer token and binds the verified
// task id. The event's task id must equal the claim — a valid token for one
// task cannot carry another task's events.
func (v *PushJWTVerifier) VerifyRequest(r *http.Request) (taskID string, err error) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) || len(auth) <= len(prefix) {
		return "", fmt.Errorf("%w: missing Bearer credential", ErrPushUnauthorized)
	}
	claims := &pushJWTClaims{}
	_, err = jwt.ParseWithClaims(strings.TrimPrefix(auth, prefix), claims, v.keyFunc,
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(time.Now))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPushUnauthorized, err)
	}
	if claims.TaskID == "" {
		return "", fmt.Errorf("%w: token carries no taskId claim", ErrPushUnauthorized)
	}
	return claims.TaskID, nil
}
