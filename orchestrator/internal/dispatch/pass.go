package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// DefaultPassInterval is the Pass beat (docs/designs/0002 §3: 2s 节拍): a
// swallowed push is repaired within two beats, and the input-required
// detection reserved here observes a paused task within one beat of it
// reaching the gateway.
const DefaultPassInterval = 2 * time.Second

// Deliverer is the outbox drain the Pass owns from T6 on (the T4 eval loop
// ran it while dispatch did not exist).
type Deliverer interface {
	DeliverPending(ctx context.Context) (outbox.DeliverStats, error)
}

// PassOptions configures the Pass beat.
type PassOptions struct {
	// MaxFollowups caps progress continuations per ticket; 0 defers to the
	// Dispatcher's Options default.
	MaxFollowups int
	// FollowUpAfter delays a continuation after the last assignment settled;
	// 0 continues on the next tick.
	FollowUpAfter time.Duration
}

// Pass is the reconciling heartbeat of the advancement chain (docs/designs
// 0001 §4.3, D2): every beat it dispatches pending assignments, reconciles
// running assignments against the gateway's task store (the push-loss
// backstop — push is an optimization, the Pass is the correctness
// dependency), enforces the red line on tasks that vanished, schedules
// follow-ups for progress reports, and drains the outbox.
type Pass struct {
	store      *store.Store
	dispatcher *Dispatcher
	transport  a2a.Transport
	deliverer  Deliverer
	opts       PassOptions
}

// NewPass wires the beat. deliverer may be nil (no outbox drain).
func NewPass(st *store.Store, d *Dispatcher, deliverer Deliverer, opts PassOptions) *Pass {
	return &Pass{
		store:      st,
		dispatcher: d,
		transport:  d.transport,
		deliverer:  deliverer,
		opts:       opts,
	}
}

// PassStats reports one Tick.
type PassStats struct {
	// Dispatched counts assignments that landed a task this tick.
	Dispatched int
	// DispatchFailed counts dispatch attempts that failed this tick.
	DispatchFailed int
	// Reconciled counts running assignments whose task observation moved the
	// state machine this tick (terminal backfill or input-required pause).
	Reconciled int
	// Redlined counts assignments marked unresolved this tick (task missing
	// at the gateway — no automatic replay, docs/designs/0001 §8, 0009 §4 内联).
	Redlined int
	// ReconcileFailed counts tasks whose status query failed this tick
	// (gateway error — retried next beat; the outcome stays unknown).
	ReconcileFailed int
	// Stuck counts dispatched assignments past their start deadline whose
	// task produced no outcome (G7): needs-attention, no replay.
	Stuck int
	// Held counts tickets the waitsForPerson scan moved to needs-attention
	// this tick (0001 §3: 最近 Assignment blocked/failed 且此后无人发言 →
	// 自动挂起输入). Tickets already held are not re-audited or counted.
	Held int
	// Followups counts continuation assignments minted this tick.
	Followups int
	// Delivered counts outbox effects delivered this tick.
	Delivered int
	// ItemFailures counts per-item persistence failures this tick (a
	// poisoned row whose red-line/hold/follow-up write failed): each is
	// retried next tick, and one bad row never blocks the other items or
	// the outbox drain (PR #11 review, Required — the tick's comment
	// promised this; the stages now honor it).
	ItemFailures int
}

// Tick runs one beat. Per-item failures never fail the beat: each is counted
// and retried on the next tick — the store's transactional guards make every
// retry safe. A stage that cannot even scan (the store itself unreachable)
// records its error and the beat CONTINUES with the remaining stages — the
// outbox drain runs every tick regardless of any earlier stage's error, so a
// poisoned row can never starve user-visible deliveries (PR #11 review,
// Required). All stage errors are joined and returned after the drain.
func (p *Pass) Tick(ctx context.Context) (PassStats, error) {
	var stats PassStats
	var errs []error

	dispatched, err := p.dispatcher.DispatchPending(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to dispatch pending assignments: %w", err))
	} else {
		stats.Dispatched = dispatched.Dispatched
		stats.DispatchFailed = dispatched.Failed
	}

	if err := p.reconcile(ctx, &stats); err != nil {
		errs = append(errs, err)
	}
	if err := p.enforceWaitsForPerson(ctx, &stats); err != nil {
		errs = append(errs, err)
	}
	if err := p.scheduleFollowups(ctx, &stats); err != nil {
		errs = append(errs, err)
	}
	if p.deliverer != nil {
		delivered, err := p.deliverer.DeliverPending(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to drain the outbox: %w", err))
		} else {
			stats.Delivered = delivered.Delivered + delivered.Confirmed
		}
	}
	return stats, errors.Join(errs...)
}

// reconcile drives every running assignment to its task's real state
// (docs/designs/0001 §4.3: 对账 running Assignment 与 task 真实终态). Terminal
// and paused observations funnel through ApplyTerminal — the same idempotent
// path the push receiver uses — so a push that DID arrive makes the
// reconcile a no-op, and a swallowed push is backfilled here. A task the
// gateway no longer knows trips the red line: unresolved assignment,
// needs-attention ticket, no replay (docs/designs/0001 §8 (0009 §4 内联) — full recovery
// semantics land in T8). A gateway error keeps the assignment running: an
// unknown outcome is not the red line.
func (p *Pass) reconcile(ctx context.Context, stats *PassStats) error {
	candidates, err := p.store.RunningAssignmentCandidates(ctx)
	if err != nil {
		return fmt.Errorf("failed to read reconcile candidates: %w", err)
	}

	for _, c := range candidates {
		taskID := ""
		if c.Assignment.TaskID != nil {
			taskID = *c.Assignment.TaskID
		}
		task, err := p.transport.TaskState(ctx, p.dispatcher.opts.WorkerAgent, taskID)
		switch {
		case errors.Is(err, a2a.ErrTaskNotFound):
			if _, aerr := p.store.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
				AssignmentID:     c.Assignment.ID,
				AssignmentStatus: store.AssignmentStatusUnresolved,
				TicketID:         c.Ticket.ID,
				TicketStatus:     store.TicketStatusNeedsAttention,
				Event:            "assignment.unresolved",
				Detail: map[string]any{
					"task_id": taskID,
					"reason":  "task not found at the gateway; outcome indeterminate — no automatic replay (docs/designs/0001 §8, 0009 §4 内联)",
				},
			}); aerr != nil {
				// Per-item failure: count and continue — one poisoned row
				// never blocks the rest of the scan or the drain.
				stats.ItemFailures++
				continue
			}
			stats.Redlined++
		case err != nil:
			// Gateway error: the outcome stays unknown — retried next beat.
			stats.ReconcileFailed++
		default:
			if isStuck(c.Assignment, task, time.Now()) {
				// G7: the task was accepted but never produced an outcome
				// within its start window — the poll&ack lesson of a forever
				// unacknowledged item. Needs-attention with an audit row;
				// the red line forbids replay (docs/designs/0001 §8, 0009 §4 内联). The
				// assignment keeps running: the task may still be alive, a
				// person decides.
				changed, merr := p.store.MarkTicketNeedsAttention(ctx, c.Ticket.ID,
					fmt.Sprintf("task %s produced no outcome before its start deadline", taskID))
				if merr != nil {
					stats.ItemFailures++
					continue
				}
				if changed {
					if aerr := p.store.RecordAudit(ctx, c.Ticket.ID, "assignment.stuck", map[string]any{
						"assignment_id":    c.Assignment.ID,
						"task_id":          taskID,
						"started_deadline": c.Assignment.StartedDeadline,
						"observed_state":   task.State,
					}); aerr != nil {
						stats.ItemFailures++
						continue
					}
				}
				stats.Stuck++
				continue
			}
			applied, aerr := p.dispatcher.ApplyTerminal(ctx, task)
			if aerr != nil {
				// Loud per-item failure (ErrUnknownTask = the binding
				// vanished mid-flight): counted, the scan moves on — one
				// poisoned candidate never blocks the others or the drain.
				stats.ItemFailures++
				continue
			}
			if applied.AssignmentSettled || applied.TicketChanged {
				stats.Reconciled++
			}
		}
	}
	return nil
}

// isStuck reports the G7 condition: the task is past its start deadline and
// still holds no outcome — not terminal, and not paused for a person (a
// paused task HAS an outcome: the input-required detection point owns it).
func isStuck(a store.Assignment, task a2a.Task, now time.Time) bool {
	if a.StartedDeadline == nil || !now.After(*a.StartedDeadline) {
		return false
	}
	switch task.State {
	case a2a.StateInputRequired, a2a.StateAuthRequired:
		return false
	default:
		return !a2a.IsTerminalTaskState(task.State)
	}
}

// enforceWaitsForPerson holds every ticket the waitsForPerson predicate
// flags that is not already held (docs/designs 0001 §3: 最近 Assignment
// blocked/failed 且此后无人发言 → 自动挂起输入). In a healthy store the
// report-driven transitions hold these tickets atomically with the settle,
// so this scan is the consistency backstop: it catches drifted rows and
// makes the hold a standing invariant, not an accident of the write path.
// Tickets already in needs-attention are skipped without a write — the hold
// absorbs automation through the dispatch scan's needs-attention exclusion.
// A person answers through the admin retry (or a fresh trigger); the Pass
// never resumes held work on its own.
func (p *Pass) enforceWaitsForPerson(ctx context.Context, stats *PassStats) error {
	candidates, err := p.store.WaitsForPersonTickets(ctx)
	if err != nil {
		return fmt.Errorf("failed to read waitsForPerson candidates: %w", err)
	}
	for _, c := range candidates {
		if c.TicketStatus == store.TicketStatusNeedsAttention {
			continue
		}
		settled := c.Report
		if settled == "" {
			settled = c.AssignmentStatus
		}
		changed, err := p.store.MarkTicketNeedsAttention(ctx, c.TicketID,
			fmt.Sprintf("waitsForPerson: assignment %s settled %s and nobody has spoken on the thread since", c.AssignmentID, settled))
		if err != nil {
			stats.ItemFailures++
			continue
		}
		if changed {
			stats.Held++
		}
	}
	return nil
}

// scheduleFollowups mints continuation assignments for running tickets
// whose work ended under a progress report (docs/designs/0001 §4.3: 到期/
// 重派入口), within the per-ticket cap. The minted assignment is pending;
// the NEXT tick's dispatch (or this tick's, ordering aside — dispatch runs
// before the follow-up scan within one Tick, so the dispatch actually lands
// on the following beat) sends it under the ticket's unchanged contextId.
func (p *Pass) scheduleFollowups(ctx context.Context, stats *PassStats) error {
	candidates, err := p.store.FollowupCandidates(ctx)
	if err != nil {
		return fmt.Errorf("failed to read followup candidates: %w", err)
	}

	maxFollowups := p.opts.MaxFollowups
	if maxFollowups <= 0 {
		maxFollowups = p.dispatcher.opts.maxFollowups()
	}
	for _, c := range candidates {
		if c.Followups >= maxFollowups {
			continue
		}
		if p.opts.FollowUpAfter > 0 && time.Since(c.LastEndedAt) < p.opts.FollowUpAfter {
			continue
		}
		if _, err := p.store.CreateFollowupAssignment(ctx, c.TicketID); err != nil {
			stats.ItemFailures++
			continue
		}
		stats.Followups++
	}

	// Follow-ups minted above are pending; dispatch them on this same beat
	// so a progress continuation costs one tick, not two.
	if stats.Followups > 0 {
		dispatched, err := p.dispatcher.DispatchPending(ctx)
		if err != nil {
			return fmt.Errorf("failed to dispatch minted follow-ups: %w", err)
		}
		stats.Dispatched += dispatched.Dispatched
		stats.DispatchFailed += dispatched.Failed
	}
	return nil
}
