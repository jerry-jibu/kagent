package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The two consistency predicates of docs/designs 0001 §3, transplanted from
// superagent's verified state-consistency rules onto the P0 store shape:
//
//   - waitsForPerson: the ticket's latest assignment settled without
//     completing (blocked/failed in the design gloss; the six-state report
//     vocabulary in dispatch/report.go refines these into blocked, failed,
//     waiting and dropped — all four leave the ticket needing a person, plus
//     the unresolved red-line state of 0001 §8) and nobody has spoken on the
//     thread since. The ticket must hold for a person's input.
//   - followupOwners: a thread with a live ticket has an owner that holds the
//     thread's follow-ups — the Engine may only stop it, never fork the
//     thread into a second ticket (docs/designs 0001 §4.2: 订阅扇出免评估).
//
// The report vocabulary below is SQL by necessity (the store cannot import
// the dispatch package); dispatch/report.go's Report constants are the source
// of truth these literals mirror.

// latestAssignmentCTE resolves each ticket's latest assignment by mint order
// (follow-ups mint later than their predecessors). "Latest" — not "latest
// settled" — is deliberate: a fresh pending/running assignment means the
// ticket is in active play (e.g. after an admin retry), whatever an earlier
// assignment settled under.
const latestAssignmentCTE = `
WITH latest AS (
    SELECT DISTINCT ON (a.ticket_id)
           a.ticket_id, a.id AS assignment_id, a.status, a.report, a.updated_at AS settled_at
    FROM assignments a
    ORDER BY a.ticket_id, a.created_at DESC, a.id DESC
)`

// waitsForPersonArms is the "settled without completing" arm shared by the
// predicate scans: the latest assignment is the red-line unresolved state, or
// ended under a person-needed report (dispatch/report.go vocabulary).
const waitsForPersonArms = `
      (l.status = $1
          OR (l.status = $2 AND l.report IN ($3, $4, $5, $6)))`

// WaitsForPersonTicket is one ticket the waitsForPerson predicate holds:
// its latest assignment settled without completing and nobody has spoken on
// the thread since (docs/designs 0001 §3). TicketStatus lets the enforcer
// skip tickets already sitting in needs-attention without a store write.
type WaitsForPersonTicket struct {
	TicketID         string
	ThreadSource     string
	ThreadKey        string
	TicketStatus     string // pending | running | needs-attention
	AssignmentID     string // the latest assignment
	AssignmentStatus string // ended | unresolved
	Report           string // blocked | failed | waiting | dropped | "" (unresolved)
	SettledAt        time.Time
}

// WaitsForPersonTickets returns every live ticket (pending/running/
// needs-attention) the waitsForPerson predicate holds. Held tickets already
// sitting in needs-attention are included so callers can show the flag; a
// terminal ticket (failed/ended) is never a candidate.
//
// "Nobody has spoken" compares against ingested triggers — every P0 trigger
// is a person comment (the bot's own artifacts travel the outbox, not the
// trigger table). The followupOwners guard records such speech on the owner
// ticket as well, so an answered hold is visible here.
func (s *Store) WaitsForPersonTickets(ctx context.Context) ([]WaitsForPersonTicket, error) {
	rows, err := s.pool.Query(ctx, latestAssignmentCTE+`
	SELECT tk.id, tk.thread_source, tk.thread_key, tk.status,
	       l.assignment_id, l.status, l.report, l.settled_at
	FROM tickets tk
	JOIN latest l ON l.ticket_id = tk.id
	WHERE tk.status IN ($7, $8, $9)
	  AND `+waitsForPersonArms+`
	  AND NOT EXISTS (
	      SELECT 1 FROM triggers t
	      WHERE t.source = tk.thread_source
	        AND t.thread_key = tk.thread_key
	        AND t.created_at > l.settled_at)
	ORDER BY tk.created_at, tk.id`,
		AssignmentStatusUnresolved, AssignmentStatusEnded,
		"blocked", "failed", "waiting", "dropped",
		TicketStatusPending, TicketStatusRunning, TicketStatusNeedsAttention)
	if err != nil {
		return nil, fmt.Errorf("failed to scan waitsForPerson candidates: %w", err)
	}
	return collectWaitsForPerson(rows)
}

func collectWaitsForPerson(rows pgx.Rows) ([]WaitsForPersonTicket, error) {
	defer rows.Close()
	var out []WaitsForPersonTicket
	for rows.Next() {
		var c WaitsForPersonTicket
		if err := rows.Scan(&c.TicketID, &c.ThreadSource, &c.ThreadKey, &c.TicketStatus,
			&c.AssignmentID, &c.AssignmentStatus, &c.Report, &c.SettledAt); err != nil {
			return nil, fmt.Errorf("failed to scan waitsForPerson candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FollowupOwnerOfThread resolves the ticket that owns a thread's follow-ups:
// the newest live ticket on (source, threadKey). A thread whose tickets are
// all terminal has no owner — fresh triggers evaluate freely again. The eval
// engine consults this before minting a second ticket (0001 §3: Engine 只许
// stop; §4.2: the person's message is the owner's follow-up input).
func (s *Store) FollowupOwnerOfThread(ctx context.Context, source, threadKey string) (Ticket, bool, error) {
	row := s.pool.QueryRow(ctx, `
	SELECT id, thread_source, thread_key, mandate, status, session_id,
	       trigger_source, trigger_deterministic_id, created_at
	FROM tickets
	WHERE thread_source = $1 AND thread_key = $2
	  AND status IN ($3, $4, $5)
	ORDER BY created_at DESC, id DESC
	LIMIT 1`,
		source, threadKey,
		TicketStatusPending, TicketStatusRunning, TicketStatusNeedsAttention)
	tk, err := scanTicket(row)
	if err == ErrTicketNotFound {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, err
	}
	return tk, true, nil
}
