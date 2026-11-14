package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// PushReceiverPath is the HTTP path the push receiver serves; dispatches
// register PushBaseURL + this path with the gateway.
const PushReceiverPath = "/a2a/push"

// Defaults for zero-valued Options.
const (
	// DefaultMaxFollowups caps the continuations a single ticket may spend.
	// An unbounded progress→follow-up loop would burn worker turns forever.
	DefaultMaxFollowups = 5
	// DefaultStartDeadline is the G7 stuck window: a dispatched task that
	// has produced no outcome (terminal or pause) within it trips the Pass's
	// stuck branch — needs-attention, never an automatic replay.
	DefaultStartDeadline = 10 * time.Minute
)

// Options configures the dispatch chain.
type Options struct {
	// WorkerAgent is the A2A route of the worker agent (name or
	// namespace/name).
	WorkerAgent string
	// PushBaseURL is the orchestrator origin the gateway can reach for push
	// callbacks, e.g. "http://orchestrator.orchestrator.svc:8080".
	PushBaseURL string
	// MaxFollowups caps progress continuations per ticket (0 → default).
	MaxFollowups int
	// StartDeadline bounds how long a dispatched task may run without an
	// outcome before the Pass marks it stuck (G7); 0 → default, negative
	// stores no deadline.
	StartDeadline time.Duration
	// FollowUpAfter delays a progress continuation after the last
	// assignment settled; 0 continues on the next Pass tick.
	FollowUpAfter time.Duration
	// RecoveryCutoff is the instant the current process started (T8 startup
	// recovery, docs/designs 0001 §4.3). Assignments minted before it were
	// left behind by a dead process: their first dispatch after the restart
	// carries the restart recovery annotation. Zero disables the restart
	// arm — dispatch-failure retries keep annotating via the ledger.
	RecoveryCutoff time.Time
	// RequireWorkspace gates the dispatch scan on the materialization
	// baseline (docs/designs 0003 路线 A: accept 后、派工前物化): set it when
	// the orchestrator runs the workspace pipeline so a pending assignment
	// waits until its ticket's workspace has been materialized.
	RequireWorkspace bool
}

func (o Options) maxFollowups() int {
	if o.MaxFollowups <= 0 {
		return DefaultMaxFollowups
	}
	return o.MaxFollowups
}

// startDeadlineAt returns the absolute stuck deadline for a dispatch at t.
func (o Options) startDeadlineAt(t time.Time) time.Time {
	switch {
	case o.StartDeadline > 0:
		return t.Add(o.StartDeadline)
	case o.StartDeadline < 0:
		return time.Time{} // explicit "no deadline"
	default:
		return t.Add(DefaultStartDeadline)
	}
}

// Dispatcher owns the push side of the advancement chain (docs/designs/0001
// D2): it dispatches pending assignments as A2A tasks (SendMessage +
// ReturnImmediately + PushConfig), registers terminal outcomes the push
// receiver observes, and — through ApplyTerminal — drives the ticket state
// machine from the worker's Report. The Pass beat calls DispatchPending and
// reconciles with TaskState; both funnel terminal knowledge into
// ApplyTerminal, making push delivery a pure optimization.
type Dispatcher struct {
	store     *store.Store
	transport a2a.Transport
	opts      Options
}

// New returns a Dispatcher.
func New(st *store.Store, tr a2a.Transport, opts Options) *Dispatcher {
	return &Dispatcher{store: st, transport: tr, opts: opts}
}

// DispatchStats reports one DispatchPending run.
type DispatchStats struct {
	// Dispatched counts assignments that landed a task this run.
	Dispatched int
	// Failed counts dispatch attempts that failed and were recorded for the
	// next tick (docs/designs/0001 §4.3: 到期重派入口).
	Failed int
}

// DispatchPending dispatches every pending assignment of a live ticket. One
// failure does not block the others: it is recorded on the assignment
// (dispatch ledger + audit) and the next Pass tick retries it with the
// recovery annotation — a dispatch that never landed a task is safe to
// replay, unlike a task with an uncertain outcome (docs/designs/0001 §8).
func (d *Dispatcher) DispatchPending(ctx context.Context) (DispatchStats, error) {
	candidates, err := d.store.PendingDispatches(ctx, d.opts.RequireWorkspace)
	if err != nil {
		return DispatchStats{}, fmt.Errorf("failed to read pending dispatches: %w", err)
	}

	var stats DispatchStats
	for _, c := range candidates {
		if err := d.dispatchOne(ctx, c); err != nil {
			stats.Failed++
			continue
		}
		stats.Dispatched++
	}
	return stats, nil
}

func (d *Dispatcher) dispatchOne(ctx context.Context, c store.DispatchCandidate) error {
	request, err := d.buildRequest(ctx, c)
	if err != nil {
		// A malformed trigger payload is malformed durable data: loud
		// failure, recorded on the assignment like any dispatch failure.
		return d.recordFailure(ctx, c, err)
	}

	result, err := d.transport.Dispatch(ctx, *request)
	if err != nil {
		return d.recordFailure(ctx, c, err)
	}

	if err := d.store.MarkAssignmentDispatched(ctx, c.Assignment.ID, c.Ticket.ID,
		result.TaskID, result.ContextID, d.opts.startDeadlineAt(time.Now())); err != nil {
		if errors.Is(err, store.ErrAssignmentNotPending) {
			// A concurrent dispatch won; the task id it bound stands.
			return nil
		}
		return fmt.Errorf("failed to register the landed dispatch of %s: %w", c.Assignment.ID, err)
	}
	return nil
}

// isRestartRedispatch reports whether the assignment was minted before the
// current process started: its first dispatch here is a restart recovery
// redispatch (docs/designs 0001 §4.3: 重启中断的 Assignment 重派).
func (d *Dispatcher) isRestartRedispatch(c store.DispatchCandidate) bool {
	return !d.opts.RecoveryCutoff.IsZero() && c.Assignment.CreatedAt.Before(d.opts.RecoveryCutoff)
}

// recordFailure records one failed dispatch attempt. The assignment stays
// pending; the Pass's next tick retries it with the recovery annotation.
func (d *Dispatcher) recordFailure(ctx context.Context, c store.DispatchCandidate, cause error) error {
	if err := d.store.RecordDispatchFailure(ctx, c.Assignment.ID, cause.Error()); err != nil {
		return fmt.Errorf("failed to record the dispatch failure of %s: %w", c.Assignment.ID, err)
	}
	return fmt.Errorf("dispatch of assignment %s failed: %w", c.Assignment.ID, cause)
}

// buildRequest builds the A2A DispatchRequest for one candidate: the
// dispatch document as the message, the ticket session's contextId for
// continuity, and the push receiver registration for terminal states.
func (d *Dispatcher) buildRequest(ctx context.Context, c store.DispatchCandidate) (*a2a.DispatchRequest, error) {
	message, err := d.buildDocument(ctx, c)
	if err != nil {
		return nil, err
	}
	return &a2a.DispatchRequest{
		Agent:     d.opts.WorkerAgent,
		Message:   message,
		ContextID: c.Ticket.SessionID,
		PushURL:   PushReceiverURL(d.opts.PushBaseURL),
		PushID:    PushIDFor(c.Assignment.ID),
	}, nil
}

// buildDocument builds the dispatch document the worker executes: ticket
// identity, workspace name (D3: 目录名 = ticket ID), the mandate summary, the
// original request, and — past a failed attempt — the recovery annotation
// (docs/designs/0001 §4.1: 任务文本 + workspace 名 + Mandate 摘要; §4.3:
// 恢复注记).
func (d *Dispatcher) buildDocument(ctx context.Context, c store.DispatchCandidate) (string, error) {
	doc := map[string]any{
		"kind":      "dispatch",
		"ticket":    c.Ticket.ID,
		"thread":    map[string]any{"source": c.Ticket.ThreadSource, "key": c.Ticket.ThreadKey},
		"mandate":   c.Ticket.Mandate,
		"workspace": c.Ticket.ID,
	}

	pl, err := intake.DecodeCommentPayload(d.triggerPayload(ctx, c))
	if err != nil {
		return "", fmt.Errorf("ticket %s carries a trigger payload violating the decode contract: %w", c.Ticket.ID, err)
	}
	doc["request"] = map[string]any{
		"repo":       pl.Repo,
		"issue":      pl.Issue,
		"comment_id": pl.CommentID,
		"author":     pl.Author,
		"body":       pl.Body,
		"url":        pl.URL,
	}

	if c.Assignment.DispatchAttempts > 0 || d.isRestartRedispatch(c) {
		recovery := map[string]any{}
		if c.Assignment.DispatchAttempts > 0 {
			recovery["attempt"] = c.Assignment.DispatchAttempts + 1
			recovery["previous_error"] = c.Assignment.LastError
		}
		if d.isRestartRedispatch(c) {
			recovery["reason"] = "orchestrator restart"
		}
		doc["recovery"] = recovery

		// 未读 Inputs 随行 (docs/designs 0001 §4.3): person messages recorded
		// on the thread while the ticket was interrupted ride along. There is
		// no worker ACK in P0, so the inputs ride on every recovery
		// redispatch — the document carries history, the store keeps truth.
		inputs, err := d.store.FollowupInputs(ctx, c.Ticket.ID)
		if err != nil {
			return "", fmt.Errorf("ticket %s carries unreadable followup inputs: %w", c.Ticket.ID, err)
		}
		if len(inputs) > 0 {
			doc["unread_inputs"] = inputs
		}
	}

	message, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to encode the dispatch document: %w", err)
	}
	return string(message), nil
}

func (d *Dispatcher) triggerPayload(ctx context.Context, c store.DispatchCandidate) []byte {
	if c.Ticket.TriggerSource == "" {
		return nil
	}
	triggers, err := d.store.Triggers(ctx, c.Ticket.TriggerSource)
	if err != nil {
		return nil
	}
	for _, tr := range triggers {
		if tr.DeterministicID == c.Ticket.TriggerDeterministicID {
			return tr.Payload
		}
	}
	return nil
}

// PushReceiverURL joins the configured base with the receiver path.
func PushReceiverURL(base string) string {
	return strings.TrimSuffix(base, "/") + PushReceiverPath
}

// PushIDFor derives the deterministic push registration id of an assignment:
// re-dispatches of the same assignment re-register the same config instead of
// accumulating registrations.
func PushIDFor(assignmentID string) string {
	return "push-" + assignmentID
}

// ErrUnknownTask reports that a pushed/queried task id does not belong to any
// assignment the orchestrator dispatched — malformed or foreign data.
var ErrUnknownTask = errors.New("task does not belong to any assignment")

// Applied reports what ApplyTerminal did.
type Applied struct {
	// AssignmentSettled is true when this call settled the assignment.
	AssignmentSettled bool
	// TicketChanged is true when the ticket status moved in this call.
	TicketChanged bool
}

// ApplyTerminal applies one task observation to the state machine. It is the
// single funnel for terminal knowledge — the push receiver and the Pass
// reconcile both call it, so a push and its reconcile backfill cannot double
// count (the store's settled guard makes the replay a no-op).
//
// Semantics per observation (docs/designs/0001 §3, docs/designs/0002 §3):
//   - terminal task + Report document → Report-driven ticket transition
//     (six-state coverage; unparseable text falls back to the bare task
//     state mapping);
//   - input-required task → the reserved detection point: the ticket moves
//     to needs-attention (P0), the assignment keeps running;
//   - any other non-terminal state with a Report → audit-only observation.
func (d *Dispatcher) ApplyTerminal(ctx context.Context, task a2a.Task) (Applied, error) {
	assignment, ticket, found, err := d.store.AssignmentByTaskID(ctx, task.ID)
	if err != nil {
		return Applied{}, fmt.Errorf("failed to resolve the assignment of task %s: %w", task.ID, err)
	}
	if !found {
		return Applied{}, fmt.Errorf("%w: %s", ErrUnknownTask, task.ID)
	}

	switch {
	case a2a.IsTerminalTaskState(task.State):
		return d.applyReportPlan(ctx, assignment, ticket, task)
	case task.State == a2a.StateInputRequired || task.State == a2a.StateAuthRequired:
		// Reserved detection point (docs/designs/0002 §3: Pass 对账检测
		// input-required): P0 maps it to needs-attention. The Attention
		// bridge and the answer channel land later.
		changed, err := d.store.MarkTicketNeedsAttention(ctx, ticket.ID,
			fmt.Sprintf("task %s paused in %s awaiting a person", task.ID, task.State))
		return Applied{TicketChanged: changed}, err
	default:
		// A non-terminal progress observation: record it, change nothing.
		if report, perr := ParseReport(task.StatusText); perr == nil {
			if err := d.store.RecordAudit(ctx, ticket.ID, "report."+report.State, reportDetail(report, task.ID)); err != nil {
				return Applied{}, fmt.Errorf("failed to record the progress observation of %s: %w", task.ID, err)
			}
		}
		return Applied{}, nil
	}
}

// applyReportPlan settles a terminal task under its Report plan.
func (d *Dispatcher) applyReportPlan(ctx context.Context, assignment store.Assignment, ticket store.Ticket, task a2a.Task) (Applied, error) {
	report, err := ParseReport(task.StatusText)
	if err != nil {
		// The worker emitted no readable report: fall back to the bare task
		// state (docs/designs/0001 §3: P0 可从 task 终态+文本提取).
		state, ok := ReportFromTaskState(task.State)
		if !ok {
			return Applied{}, fmt.Errorf("terminal task %s carries no report and state %s maps to none", task.ID, task.State)
		}
		report = Report{State: state, Summary: "(no report document; derived from task state " + task.State + ")"}
	}

	plan := PlanForReport(report)
	outcome, err := d.store.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
		AssignmentID:     assignment.ID,
		AssignmentStatus: plan.AssignmentStatus,
		ReportState:      report.State,
		TicketID:         ticket.ID,
		TicketStatus:     plan.TicketStatus,
		DeliveryPending:  plan.Deliverable,
		Event:            plan.Event,
		Detail:           reportDetail(report, task.ID),
	})
	if err != nil {
		return Applied{}, fmt.Errorf("failed to apply the %s outcome of task %s: %w", report.State, task.ID, err)
	}
	return Applied{AssignmentSettled: outcome.Applied, TicketChanged: outcome.TicketChanged}, nil
}

func reportDetail(report Report, taskID string) map[string]any {
	detail := map[string]any{
		"state":   report.State,
		"task_id": taskID,
	}
	if report.Summary != "" {
		detail["summary"] = report.Summary
	}
	if len(report.Artifacts) > 0 {
		detail["artifacts"] = report.Artifacts
	}
	return detail
}
