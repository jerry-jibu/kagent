package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Ticket and assignment statuses. The Engine owns the full status
// vocabularies; see migrations/0004_tickets.sql and 0001_init.sql for the
// CHECK-enforced lists.
const (
	TicketStatusPending        = "pending"
	TicketStatusRunning        = "running"
	TicketStatusNeedsAttention = "needs-attention"
	TicketStatusFailed         = "failed"
	TicketStatusEnded          = "ended"

	AssignmentStatusPending    = "pending"
	AssignmentStatusRunning    = "running"
	AssignmentStatusEnded      = "ended"
	AssignmentStatusUnresolved = "unresolved"

	AssignmentKindInitial  = "initial"
	AssignmentKindFollowup = "followup"
)

// ErrInvalidTicket rejects ticket creations that would create
// unrepresentable authorization state.
var ErrInvalidTicket = errors.New("invalid ticket input")

// ErrDuplicateAccept reports that the trigger already carries an accept
// decision (decisions_accept_once_idx): the creation was a racing replay and
// the whole transaction rolled back.
var ErrDuplicateAccept = errors.New("accept decision already recorded for trigger")

// MandateScope is the range a mandate authorizes work in
// (docs/designs/0001 §3: Mandate = policy version + scope + strength + sender).
type MandateScope struct {
	Repo  string `json:"repo"`
	Issue int    `json:"issue"`
}

// Mandate is the durable authorization a ticket was created under. It
// references the policy version hash and the gate-judged strength and never
// any claim found in message content (docs/designs/0001 §7: 消息内容是输入
// 不是权限).
type Mandate struct {
	PolicyVersion string       `json:"policy_version"`
	Strength      string       `json:"strength"`
	Sender        string       `json:"sender"`
	Scope         MandateScope `json:"scope"`
}

// OutboxEffect is one external side effect enqueued in the same transaction
// as the state it belongs to (docs/designs/0001 §6: 外部副作用以 effect 在
// 事务内入队、提交后由适配器执行). The channel adapter owns the payload's
// wire shape; the store persists it opaquely.
type OutboxEffect struct {
	Channel        string
	Address        string
	IdempotencyKey string
	Payload        []byte
}

// Ticket is one authorized work unit. SessionID is the A2A contextId of the
// ticket's kagent session — set on the first successful dispatch and reused
// verbatim by every continuation (同一 ticket 续发消息 contextId 不变). The
// trigger identity is recorded at creation so the dispatch document can carry
// the original request.
type Ticket struct {
	ID                     string
	ThreadSource           string
	ThreadKey              string
	Mandate                Mandate
	Status                 string
	SessionID              string
	TriggerSource          string
	TriggerDeterministicID string
	CreatedAt              time.Time
}

// Assignment is one ticket↔executor binding. Dispatch (T6) sets TaskID once,
// when the A2A task is sent. Report carries the state of the report the
// assignment settled under; DispatchAttempts/LastError are the dispatch
// ledger a recovery redispatch annotates. StartedDeadline is the G7 stuck
// window: a dispatched task past it with no outcome trips the Pass's stuck
// branch (nil = no deadline).
type Assignment struct {
	ID               string
	TicketID         string
	TaskID           *string
	Status           string
	Report           string
	DispatchAttempts int
	LastError        string
	Kind             string
	StartedDeadline  *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TicketInput is one accept-to-ticket creation. Decision and Mandate must
// agree: they are two views of one gate verdict, and the store refuses the
// input when they disagree so a forged mandate cannot diverge from the
// recorded decision.
type TicketInput struct {
	Source          string
	DeterministicID string
	ThreadKey       string
	Decision        DecisionInput
	Mandate         Mandate
	Effects         []OutboxEffect
}

func (in TicketInput) validate() error {
	switch {
	case in.Source == "":
		return fmt.Errorf("%w: source is empty", ErrInvalidTicket)
	case in.DeterministicID == "":
		return fmt.Errorf("%w: deterministic id is empty", ErrInvalidTicket)
	case in.ThreadKey == "":
		return fmt.Errorf("%w: thread key is empty", ErrInvalidTicket)
	}
	if in.Decision.Outcome != DecisionAccept {
		return fmt.Errorf("%w: ticket creation requires an accept decision, got %q",
			ErrInvalidTicket, in.Decision.Outcome)
	}
	if err := in.Decision.validate(); err != nil {
		return err
	}
	m := in.Mandate
	switch {
	case m.PolicyVersion != in.Decision.PolicyVersion:
		return fmt.Errorf("%w: mandate policy version %q disagrees with the decision's %q",
			ErrInvalidTicket, m.PolicyVersion, in.Decision.PolicyVersion)
	case m.Strength != in.Decision.Strength:
		return fmt.Errorf("%w: mandate strength %q disagrees with the decision strength %q",
			ErrInvalidTicket, m.Strength, in.Decision.Strength)
	case m.Sender == "":
		return fmt.Errorf("%w: mandate sender is empty", ErrInvalidTicket)
	case m.Scope.Repo == "":
		return fmt.Errorf("%w: mandate scope repo is empty", ErrInvalidTicket)
	case m.Scope.Issue <= 0:
		return fmt.Errorf("%w: mandate scope issue %d is not a GitHub conversation number",
			ErrInvalidTicket, m.Scope.Issue)
	}
	for i, e := range in.Effects {
		switch {
		case e.Channel == "":
			return fmt.Errorf("%w: effect %d names no channel", ErrInvalidTicket, i)
		case e.Address == "":
			return fmt.Errorf("%w: effect %d names no address", ErrInvalidTicket, i)
		case e.IdempotencyKey == "":
			return fmt.Errorf("%w: effect %d names no idempotency key", ErrInvalidTicket, i)
		case len(e.Payload) == 0 || !json.Valid(e.Payload):
			return fmt.Errorf("%w: effect %d payload is empty or not JSON", ErrInvalidTicket, i)
		}
	}
	return nil
}

// CreatedTicket is the outcome of CreateTicket: the ticket and the accept
// decision that committed together in one transaction.
type CreatedTicket struct {
	Ticket   Ticket
	Decision Decision
}

// CreateTicket performs the accept transition (docs/designs/0001 §6: Engine
// 唯一写者, 全部状态迁移在单事务内完成): the accept decision row, the ticket
// with its mandate, the pending assignment, and the queued external effects
// all commit together or not at all. A crash before the commit leaves the
// trigger unjudged (re-evaluated on the next cycle); a crash after it leaves
// a fully-committed ticket whose effects the deliverer replays. The
// accept-once partial index on decisions makes a racing replay roll back as
// ErrDuplicateAccept, so a trigger can never mint two tickets.
func (s *Store) CreateTicket(ctx context.Context, in TicketInput) (CreatedTicket, error) {
	if err := in.validate(); err != nil {
		return CreatedTicket{}, err
	}

	mandate, err := json.Marshal(in.Mandate)
	if err != nil {
		return CreatedTicket{}, fmt.Errorf("%w: mandate is not encodable: %v", ErrInvalidTicket, err)
	}

	ticketID := TicketIDForTrigger(in.Source, in.DeterministicID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreatedTicket{}, fmt.Errorf("failed to begin create-ticket transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var d Decision
	err = tx.QueryRow(ctx, `
		INSERT INTO decisions (source, deterministic_id, outcome, strength, policy_version, reason)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		in.Source, in.DeterministicID, in.Decision.Outcome, in.Decision.Strength,
		in.Decision.PolicyVersion, in.Decision.Reason,
	).Scan(&d.ID, &d.CreatedAt)
	if err != nil {
		if duplicateAccept(err) {
			return CreatedTicket{}, fmt.Errorf("trigger %s/%s: %w", in.Source, in.DeterministicID, ErrDuplicateAccept)
		}
		return CreatedTicket{}, fmt.Errorf("failed to record accept decision for trigger %s/%s: %w",
			in.Source, in.DeterministicID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO tickets (id, thread_source, thread_key, mandate, status, trigger_source, trigger_deterministic_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ticketID, in.Source, in.ThreadKey, mandate, TicketStatusPending,
		in.Source, in.DeterministicID); err != nil {
		return CreatedTicket{}, fmt.Errorf("failed to insert ticket %s: %w", ticketID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO assignments (id, ticket_id, status)
		VALUES ($1, $2, $3)`,
		AssignmentIDForTrigger(in.Source, in.DeterministicID), ticketID, AssignmentStatusPending); err != nil {
		return CreatedTicket{}, fmt.Errorf("failed to insert assignment for ticket %s: %w", ticketID, err)
	}

	for _, e := range in.Effects {
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox (channel, address, idempotency_key, payload)
			VALUES ($1, $2, $3, $4)`,
			e.Channel, e.Address, e.IdempotencyKey, e.Payload); err != nil {
			return CreatedTicket{}, fmt.Errorf("failed to enqueue effect %q for ticket %s: %w",
				e.IdempotencyKey, ticketID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return CreatedTicket{}, fmt.Errorf("failed to commit ticket %s: %w", ticketID, err)
	}

	d.Source = in.Source
	d.DeterministicID = in.DeterministicID
	d.Outcome = in.Decision.Outcome
	d.Strength = in.Decision.Strength
	d.PolicyVersion = in.Decision.PolicyVersion
	d.Reason = in.Decision.Reason

	return CreatedTicket{
		Ticket: Ticket{
			ID:           ticketID,
			ThreadSource: in.Source,
			ThreadKey:    in.ThreadKey,
			Mandate:      in.Mandate,
			Status:       TicketStatusPending,
		},
		Decision: d,
	}, nil
}

// duplicateAccept reports whether err is the decisions_accept_once_idx
// unique violation — a racing accept for a trigger already judged.
func duplicateAccept(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == "23505" &&
		pgErr.ConstraintName == "decisions_accept_once_idx"
}

// Ticket returns one ticket row.
func (s *Store) Ticket(ctx context.Context, id string) (Ticket, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, thread_source, thread_key, mandate, status, session_id,
		       trigger_source, trigger_deterministic_id, created_at
		FROM tickets WHERE id = $1`, id)
	return scanTicket(row)
}

// Tickets returns all ticket rows, creation order first.
func (s *Store) Tickets(ctx context.Context) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, thread_source, thread_key, mandate, status, session_id,
		       trigger_source, trigger_deterministic_id, created_at
		FROM tickets
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("failed to list tickets: %w", err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		tk, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

// scanTicket decodes one tickets row. A mandate that no longer decodes is
// malformed durable data and is rejected, never silently decoded into a
// zero-value authorization.
func scanTicket(row rowScanner) (Ticket, error) {
	var tk Ticket
	var mandate []byte
	var sessionID *string
	if err := row.Scan(&tk.ID, &tk.ThreadSource, &tk.ThreadKey, &mandate, &tk.Status, &sessionID,
		&tk.TriggerSource, &tk.TriggerDeterministicID, &tk.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Ticket{}, ErrTicketNotFound
		}
		return Ticket{}, fmt.Errorf("failed to scan ticket: %w", err)
	}
	if sessionID != nil {
		tk.SessionID = *sessionID
	}
	if err := json.Unmarshal(mandate, &tk.Mandate); err != nil {
		return Ticket{}, fmt.Errorf("ticket %s carries a mandate that does not decode: %w", tk.ID, err)
	}
	return tk, nil
}

// assignmentColumns is the shared SELECT list of the single-table assignment
// scans; assignmentJoinColumns qualifies it for the candidate JOIN queries.
const (
	assignmentColumns = `
	id, ticket_id, task_id, status, report, dispatch_attempts, last_error,
	kind, started_deadline, created_at, updated_at`

	assignmentJoinColumns = `
	a.id, a.ticket_id, a.task_id, a.status, a.report, a.dispatch_attempts,
	a.last_error, a.kind, a.started_deadline, a.created_at, a.updated_at`
)

func scanAssignment(row rowScanner) (Assignment, error) {
	var a Assignment
	if err := row.Scan(&a.ID, &a.TicketID, &a.TaskID, &a.Status, &a.Report,
		&a.DispatchAttempts, &a.LastError, &a.Kind, &a.StartedDeadline, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return Assignment{}, fmt.Errorf("failed to scan assignment: %w", err)
	}
	return a, nil
}

// Assignments returns the assignments bound to a ticket, oldest first.
func (s *Store) Assignments(ctx context.Context, ticketID string) ([]Assignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+assignmentColumns+`
		FROM assignments
		WHERE ticket_id = $1
		ORDER BY created_at, id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("failed to list assignments for ticket %s: %w", ticketID, err)
	}
	defer rows.Close()

	var out []Assignment
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// triggerIdentityHash derives the shared id-suffix of a trigger's ticket and
// assignment: the trigger identity is immutable (audit and idempotency unit),
// so the derived ids are deterministic and re-deriving them after a crash
// yields the same rows.
func triggerIdentityHash(source, deterministicID string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + deterministicID))
	return hex.EncodeToString(sum[:])[:12]
}

// TicketIDForTrigger derives the deterministic ticket id of a trigger.
func TicketIDForTrigger(source, deterministicID string) string {
	return "ticket-" + triggerIdentityHash(source, deterministicID)
}

// AssignmentIDForTrigger derives the deterministic id of the assignment a
// ticket creation mints for a trigger.
func AssignmentIDForTrigger(source, deterministicID string) string {
	return "assign-" + triggerIdentityHash(source, deterministicID)
}
