package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
)

// Persisted decision outcomes (docs/designs/0001 §3 Decision row). The first
// four are the evaluator's structured Decision vocabulary; denied and
// rejected are orchestrator-side conclusions the Engine records for the audit
// trail: denied is the Gate's deterministic refusal (no evaluator invoke),
// rejected is the code-validation refusal of a forged evaluator output.
const (
	DecisionAccept      = "accept"
	DecisionStop        = "stop"
	DecisionIgnore      = "ignore"
	DecisionUnavailable = "unavailable"
	DecisionDenied      = "denied"
	DecisionRejected    = "rejected"
)

// ErrInvalidDecision rejects decision records that would create
// unrepresentable state.
var ErrInvalidDecision = errors.New("invalid decision")

// Decision is one persisted evaluation record for a trigger. Strength and
// PolicyVersion always carry the GATE-verified values — never an agent's
// claim (docs/designs/0001 §7).
type Decision struct {
	ID              int64
	Source          string
	DeterministicID string
	Outcome         string
	Strength        string
	PolicyVersion   string
	Reason          string
	CreatedAt       time.Time
}

// Retryable reports whether the decision leaves the trigger open for
// re-evaluation. Only unavailable does: it means evaluation could not
// complete (evaluator unreachable or timed out), not that it concluded.
func (d Decision) Retryable() bool {
	return d.Outcome == DecisionUnavailable
}

// DecisionInput is the record the Engine persists for one evaluation. The
// store validates it against the persisted vocabulary so a caller cannot
// record a decision the columns' invariants forbid.
type DecisionInput struct {
	Outcome       string
	Strength      string
	PolicyVersion string
	Reason        string
}

func (in DecisionInput) validate() error {
	switch in.Outcome {
	case DecisionAccept, DecisionStop, DecisionIgnore, DecisionUnavailable, DecisionDenied, DecisionRejected:
	default:
		return fmt.Errorf("%w: outcome %q is not in the decision vocabulary", ErrInvalidDecision, in.Outcome)
	}
	if in.PolicyVersion == "" {
		return fmt.Errorf("%w: policy version is empty", ErrInvalidDecision)
	}
	switch {
	case in.Outcome == DecisionDenied && in.Strength != "":
		return fmt.Errorf("%w: a gate denial carries no strength, got %q", ErrInvalidDecision, in.Strength)
	case in.Outcome != DecisionDenied &&
		in.Strength != policy.StrengthExplicit && in.Strength != policy.StrengthInferred:
		return fmt.Errorf("%w: outcome %q needs a gate strength of explicit or inferred, got %q",
			ErrInvalidDecision, in.Outcome, in.Strength)
	}
	return nil
}

// RecordDecision appends one evaluation record for a trigger. A second
// accept for the same trigger is refused by a partial unique index: Ticket
// creation (T4) keys off accept decisions, so the store itself makes
// double-accept unrepresentable even under racing retries.
func (s *Store) RecordDecision(ctx context.Context, source, deterministicID string, in DecisionInput) (Decision, error) {
	if err := in.validate(); err != nil {
		return Decision{}, err
	}

	var d Decision
	err := s.pool.QueryRow(ctx, `
		INSERT INTO decisions (source, deterministic_id, outcome, strength, policy_version, reason)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		source, deterministicID, in.Outcome, in.Strength, in.PolicyVersion, in.Reason,
	).Scan(&d.ID, &d.CreatedAt)
	if err != nil {
		return Decision{}, fmt.Errorf("failed to record %s decision for trigger %s/%s: %w",
			in.Outcome, source, deterministicID, err)
	}
	d.Source = source
	d.DeterministicID = deterministicID
	d.Outcome = in.Outcome
	d.Strength = in.Strength
	d.PolicyVersion = in.PolicyVersion
	d.Reason = in.Reason
	return d, nil
}

// Decisions returns the evaluation records of a source, oldest first.
// The decisions table is the evaluation audit trail (docs/designs/0001 §3),
// so replaying "who judged what and why" starts here.
func (s *Store) Decisions(ctx context.Context, source string) ([]Decision, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, source, deterministic_id, outcome, strength, policy_version, reason, created_at
		FROM decisions
		WHERE source = $1
		ORDER BY id`, source)
	if err != nil {
		return nil, fmt.Errorf("failed to list decisions for %s: %w", source, err)
	}
	defer rows.Close()

	var out []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.Source, &d.DeterministicID, &d.Outcome,
			&d.Strength, &d.PolicyVersion, &d.Reason, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan decision: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TriggerDecision returns the newest decision recorded for a trigger, if any.
// The Engine consults it before evaluating: a terminal outcome (anything but
// unavailable) means the trigger was already judged and must not be
// re-evaluated; an unavailable one is the retryable record.
func (s *Store) TriggerDecision(ctx context.Context, source, deterministicID string) (Decision, bool, error) {
	var d Decision
	err := s.pool.QueryRow(ctx, `
		SELECT id, source, deterministic_id, outcome, strength, policy_version, reason, created_at
		FROM decisions
		WHERE source = $1 AND deterministic_id = $2
		ORDER BY id DESC
		LIMIT 1`, source, deterministicID).
		Scan(&d.ID, &d.Source, &d.DeterministicID, &d.Outcome,
			&d.Strength, &d.PolicyVersion, &d.Reason, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, false, nil
	}
	if err != nil {
		return Decision{}, false, fmt.Errorf("failed to read latest decision for trigger %s/%s: %w",
			source, deterministicID, err)
	}
	return d, true, nil
}
