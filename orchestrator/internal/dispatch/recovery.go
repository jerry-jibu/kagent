package dispatch

import (
	"context"
	"fmt"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// Recover is the startup step of the advancement chain (docs/designs 0001
// §4.3: 装载未终结 Assignment; §8 orchestrator-crash row): after a kill -9 the
// new process opens the same store and runs Recover once, before the first
// Pass tick. Committed state is already durable, so recovery only needs to
// LOOK: every pending assignment is re-dispatched by the Pass under the
// recovery annotation (a dispatch that never landed is safe to replay),
// every running assignment is reconciled against the gateway task store, and
// committed-but-undelivered effects are replayed by the outbox deliverer
// with receipt dedup (已发未确认消息靠 receipt 去重). Recovery therefore
// records the manifest per holding ticket — the auditable answer to "what
// was in flight when the process died" — and returns it.
func Recover(ctx context.Context, st *store.Store) (RecoveryStats, error) {
	candidates, err := st.NonTerminalAssignments(ctx)
	if err != nil {
		return RecoveryStats{}, fmt.Errorf("failed to load non-terminal assignments: %w", err)
	}

	stats := RecoveryStats{}
	byTicket := map[string][]store.Assignment{}
	var order []string
	for _, c := range candidates {
		stats.assign(c.Assignment.Status)
		if _, seen := byTicket[c.Ticket.ID]; !seen {
			order = append(order, c.Ticket.ID)
		}
		byTicket[c.Ticket.ID] = append(byTicket[c.Ticket.ID], c.Assignment)
	}

	for _, ticketID := range order {
		manifest := make([]map[string]any, 0, len(byTicket[ticketID]))
		for _, a := range byTicket[ticketID] {
			entry := map[string]any{
				"id":     a.ID,
				"status": a.Status,
			}
			if a.TaskID != nil {
				entry["task_id"] = *a.TaskID
			}
			manifest = append(manifest, entry)
		}
		if err := st.RecordAudit(ctx, ticketID, "recovery.startup", map[string]any{
			"assignments": manifest,
		}); err != nil {
			return RecoveryStats{}, fmt.Errorf("failed to audit the recovery manifest of %s: %w", ticketID, err)
		}
	}
	stats.Tickets = len(byTicket)
	return stats, nil
}

// RecoveryStats reports the startup manifest: how many assignments were
// loaded per status, across how many tickets.
type RecoveryStats struct {
	Tickets int
	Pending int
	Running int
}

func (r *RecoveryStats) assign(status string) {
	switch status {
	case store.AssignmentStatusPending:
		r.Pending++
	case store.AssignmentStatusRunning:
		r.Running++
	}
}
