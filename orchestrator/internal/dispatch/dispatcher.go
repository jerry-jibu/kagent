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

// dispatchDocument is the JSON document sent as the dispatch message — the
// worker's execution contract (docs/designs/0001 §4.1: 任务文本 + workspace
// 名 + Mandate 摘要). Its JSON shape is a contract; typed so the compiler,
// not a map literal, keeps the fields honest.
type dispatchDocument struct {
	Kind         string                `json:"kind"`
	Ticket       string                `json:"ticket"`
	Thread       dispatchThread        `json:"thread"`
	Mandate      store.Mandate         `json:"mandate"`
	Workspace    string                `json:"workspace"`
	Request      dispatchRequest       `json:"request"`
	Recovery     *dispatchRecovery     `json:"recovery,omitempty"`
	UnreadInputs []store.FollowupInput `json:"unread_inputs,omitempty"`
}

// dispatchThread identifies the external conversation the ticket serves.
type dispatchThread struct {
	Source string `json:"source"`
	Key    string `json:"key"`
}

// dispatchRequest embeds the original request verbatim — the trigger payload
// the ticket was created from, so the worker sees the request, not a summary.
type dispatchRequest struct {
	Repo      string `json:"repo"`
	Issue     int    `json:"issue"`
	CommentID int64  `json:"comment_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	URL       string `json:"url"`
}

// dispatchRecovery annotates a redispatch: which attempt this is and why it
// is happening again (docs/designs/0001 §4.3: 恢复注记). A fresh first
// dispatch carries no recovery section at all.
type dispatchRecovery struct {
	Attempt       int    `json:"attempt,omitempty"`
	PreviousError string `json:"previous_error,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// buildDocument builds the dispatch document the worker executes: ticket
// identity, workspace name (D3: 目录名 = ticket ID), the mandate summary, the
// original request, and — past a failed attempt — the recovery annotation
// (docs/designs/0001 §4.1; §4.3: 恢复注记).
func (d *Dispatcher) buildDocument(ctx context.Context, c store.DispatchCandidate) (string, error) {
	doc := dispatchDocument{
		Kind:      "dispatch",
		Ticket:    c.Ticket.ID,
		Thread:    dispatchThread{Source: c.Ticket.ThreadSource, Key: c.Ticket.ThreadKey},
		Mandate:   c.Ticket.Mandate,
		Workspace: c.Ticket.ID,
	}

	payload, err := d.triggerPayload(ctx, c)
	if err != nil {
		return "", fmt.Errorf("ticket %s cannot carry its original request: %w", c.Ticket.ID, err)
	}
	pl, err := intake.DecodeCommentPayload(payload)
	if err != nil {
		return "", fmt.Errorf("ticket %s carries a trigger payload violating the decode contract: %w", c.Ticket.ID, err)
	}
	doc.Request = dispatchRequest{
		Repo:      pl.Repo,
		Issue:     pl.Issue,
		CommentID: pl.CommentID,
		Author:    pl.Author,
		Body:      pl.Body,
		URL:       pl.URL,
	}

	if c.Assignment.DispatchAttempts > 0 || d.isRestartRedispatch(c) {
		doc.Recovery = &dispatchRecovery{}
		if c.Assignment.DispatchAttempts > 0 {
			doc.Recovery.Attempt = c.Assignment.DispatchAttempts + 1
			doc.Recovery.PreviousError = c.Assignment.LastError
		}
		if d.isRestartRedispatch(c) {
			doc.Recovery.Reason = "orchestrator restart"
		}
	}

	// 未读 Inputs 随行 (docs/designs 0001 §4.3): person messages recorded on
	// the ticket's thread ride along on EVERY dispatch while they exist —
	// first dispatches included. The inputs are unacknowledged context: a
	// dispatch that omits them would send the worker back to work on stale
	// knowledge (直送上下文语义). There is no worker ACK in P0, so the document
	// carries history and the store keeps truth; progress follow-ups carry
	// them the same way.
	inputs, err := d.store.FollowupInputs(ctx, c.Ticket.ID)
	if err != nil {
		return "", fmt.Errorf("ticket %s carries unreadable followup inputs: %w", c.Ticket.ID, err)
	}
	doc.UnreadInputs = inputs

	message, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to encode the dispatch document: %w", err)
	}
	return string(message), nil
}

// triggerPayload reads back the trigger payload the ticket was created from.
// The trigger rows are immutable audit data: a read failure or a missing
// matching row is malformed durable state and is refused — the dispatch is
// never built without its original request (AGENTS.md: reject malformed
// durable data rather than silently omitting it).
func (d *Dispatcher) triggerPayload(ctx context.Context, c store.DispatchCandidate) ([]byte, error) {
	if c.Ticket.TriggerSource == "" {
		return nil, fmt.Errorf("ticket records no trigger source")
	}
	triggers, err := d.store.Triggers(ctx, c.Ticket.TriggerSource)
	if err != nil {
		return nil, fmt.Errorf("failed to read triggers of source %s: %w", c.Ticket.TriggerSource, err)
	}
	for _, tr := range triggers {
		if tr.DeterministicID == c.Ticket.TriggerDeterministicID {
			return tr.Payload, nil
		}
	}
	return nil, fmt.Errorf("trigger %s/%s is not in the audit rows of its source", c.Ticket.TriggerSource, c.Ticket.TriggerDeterministicID)
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

// ReportDetail is the audit detail of a report observation — the durable
// record later stages read back: the deliverer quotes the ready report's
// summary and artifacts into the PR body (T7), the admin rows surface the
// last observation. Typed, not a map: the JSON shape is a contract.
type ReportDetail struct {
	State     string   `json:"state"`
	TaskID    string   `json:"task_id"`
	Summary   string   `json:"summary,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

func reportDetail(report Report, taskID string) ReportDetail {
	return ReportDetail{
		State:     report.State,
		TaskID:    taskID,
		Summary:   report.Summary,
		Artifacts: report.Artifacts,
	}
}
