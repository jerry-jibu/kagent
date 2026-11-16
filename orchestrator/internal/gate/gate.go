// Package gate implements the deterministic admission gate of the Eval
// stage (docs/designs/0001 §3: Eval = Gate + Assessment; §7: 门在编排器，
// 不在模型). The Gate is pure code: given the policy snapshot and a decoded
// trigger payload it returns a verdict the Engine acts on verbatim. The
// evaluator agent only judges content afterward — it can never grant an
// authorization the Gate did not already give, because its Decision must
// echo the gate-judged strength and policy version and the Engine validates
// that echo (package eval).
//
// The Gate must judge deterministically for every trigger: there is no
// "unknown" outcome, only allow-with-strength or deny-with-reason.
package gate

import (
	"fmt"
	"strings"

	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
)

// Verdict is the Gate's judgment of one trigger. A denial carries no
// strength, so a denied trigger cannot be confused with an allowed one
// downstream.
type Verdict struct {
	// Allowed reports whether the trigger passes the gate.
	Allowed bool
	// Strength is the admission strength granted to the sender
	// (policy.StrengthExplicit or policy.StrengthInferred) when allowed,
	// empty otherwise.
	Strength string
	// Reason is the deterministic denial reason when not allowed.
	Reason string
}

// Judge applies the policy to one trigger payload:
//
//  1. the repository must be in the policy allowlist;
//  2. the author must not be a bot — bot comments never count as human
//     triggers (policy.IsBot);
//  3. the comment body must address a policy bot — background conversation
//     never reaches the evaluator (docs/designs/0001 §4.1: 在名单+@ = explicit);
//  4. the sender's admission table strength decides allow or deny; a sender
//     missing from the table is denied (deny by default).
func Judge(p policy.Policy, pl intake.CommentPayload) Verdict {
	if !repoAllowlisted(p, pl.Repo) {
		return Verdict{Reason: fmt.Sprintf("repository %q is not in the policy allowlist", pl.Repo)}
	}
	if p.IsBot(pl.Author) {
		return Verdict{Reason: fmt.Sprintf("author %q is a bot; bot comments never count as human triggers", pl.Author)}
	}
	if !addressesBot(p, pl.Body) {
		return Verdict{Reason: "no policy bot is addressed in the comment body"}
	}
	strength := p.StrengthFor(pl.Author)
	if strength == policy.StrengthDenied {
		return Verdict{Reason: fmt.Sprintf("sender %q is not admitted by the policy admission table", pl.Author)}
	}
	return Verdict{Allowed: true, Strength: strength}
}

// repoAllowlisted reports whether the repository full name is in the
// allowlist. Entries are exact "owner/name" matches (policy validates the
// shape).
func repoAllowlisted(p policy.Policy, repo string) bool {
	for _, r := range p.Repositories {
		if r == repo {
			return true
		}
	}
	return false
}

// addressesBot reports whether the body @-mentions any policy bot login.
// Mention matching follows GitHub: the "@login" token is case-insensitive
// and must not be a prefix of a longer login ("@acme-bot2" does not address
// "acme-bot").
func addressesBot(p policy.Policy, body string) bool {
	for _, bot := range p.Bots {
		if mentionsLogin(body, bot) {
			return true
		}
	}
	return false
}

// mentionsLogin scans the body for "@login" occurrences whose following
// character cannot extend the login (GitHub logins are alphanumerics and
// hyphens), so longer logins sharing a prefix do not match.
func mentionsLogin(body, login string) bool {
	if login == "" {
		return false
	}
	lower := strings.ToLower(body)
	token := "@" + strings.ToLower(login)
	for start := 0; ; {
		at := strings.Index(lower[start:], token)
		if at < 0 {
			return false
		}
		end := start + at + len(token)
		if end == len(lower) || !isLoginChar(lower[end]) {
			return true
		}
		start += at + 1
	}
}

// isLoginChar reports whether r can continue a GitHub login token. The body
// is lowercased ASCII-matched, so byte comparison is exact for [a-z0-9-].
func isLoginChar(r byte) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
}
