// Package delivery is the V1 tail of the pipeline (T7, docs/designs/0003
// Alternative E + 0001 §4.1 ⑥⑦): after a worker reports ready, the
// orchestrator reads the workspace back, computes the patch ITSELF against
// the exact materialization baseline (the worker never touches git or
// github.com — the egress whitelist forbids it and no credential ever enters
// the sandbox), pushes the ticket branch, opens the PR, and queues the
// result comment through the transactional outbox (deterministic marker,
// replay-safe like the T4 confirmation comment).
//
// Failure semantics: a patch that is missing/corrupted/empty is permanent —
// the ticket fails with its audit and no PR is pushed. Infrastructure
// failures (backend outage, git failure, GitHub failure) stay pending for
// the next beat; the branch push is replay-safe (deterministic commit) and
// PR creation is observably idempotent (an existing head-branch PR is
// returned, never duplicated).
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// DefaultGitBaseURL is the git remote base the repo name composes with.
const DefaultGitBaseURL = "https://github.com"

// Options configure the deliverer.
type Options struct {
	// GitBaseURL is the git remote base ("https://github.com"): the clone/
	// push URL of mandate-scope repo R becomes GitBaseURL + "/" + R + ".git".
	GitBaseURL string
	// Token is the orchestrator-held credential for git (clone/push) — the
	// same credential monotonicity as the materializer (0002 A5): the token
	// rides the transient credential helper, never a URL, never the sandbox.
	Token string
	// AuthorName/AuthorEmail override the commit identity.
	AuthorName  string
	AuthorEmail string
	// Log receives per-delivery summaries; nil falls back to slog.Default().
	Log *slog.Logger
}

// Deliverer ships pending results: workspace read-back → orchestrator-side
// patch → ticket branch → PR → result-comment effect. It is the loop body
// the orchestrator beats; every candidate is independent and idempotent.
type Deliverer struct {
	store   *store.Store
	backend workspace.Backend
	patcher *workspace.Patcher
	gh      github.Transport
	opts    Options
	log     *slog.Logger
}

// New wires the deliverer. Store, Backend and Transport are required.
func New(st *store.Store, backend workspace.Backend, gh github.Transport, opts Options) *Deliverer {
	base := opts.GitBaseURL
	if base == "" {
		base = DefaultGitBaseURL
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Deliverer{
		store:   st,
		backend: backend,
		patcher: workspace.NewPatcher(workspace.PatcherOptions{}),
		gh:      gh,
		opts:    opts,
		log:     log,
	}
}

// Stats reports one DeliverPending run.
type Stats struct {
	// Delivered counts results this run shipped (branch + PR + queued comment).
	Delivered int
	// Confirmed counts candidates that turned out already delivered
	// (a raced scan won; nothing to do).
	Confirmed int
	// Failed counts permanent failures: the patch is missing, corrupted or
	// empty — the ticket transitions to failed with its audit, no PR.
	Failed int
	// Retried counts transient failures that stay pending for the next beat
	// (backend outage, git failure, GitHub failure).
	Retried int
}

// DeliverPending ships every pending result, oldest first. One candidate's
// failure never blocks the others; the error return is reserved for a broken
// scan (the store itself unreachable).
func (d *Deliverer) DeliverPending(ctx context.Context) (Stats, error) {
	candidates, err := d.store.PendingDeliveries(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("failed to read pending deliveries: %w", err)
	}

	var stats Stats
	for _, c := range candidates {
		delivered, err := d.deliverOne(ctx, c)
		if err != nil {
			// deliverOne classified and recorded the failure; a broken scan
			// is the only condition that errors the beat.
			if isScanError(err) {
				return stats, err
			}
			continue
		}
		switch delivered {
		case outcomeDelivered:
			stats.Delivered++
		case outcomeConfirmed:
			stats.Confirmed++
		case outcomeFailed:
			stats.Failed++
		case outcomeRetried:
			stats.Retried++
		}
	}
	return stats, nil
}

// outcome classes deliverOne maps every candidate to.
const (
	outcomeDelivered = iota
	outcomeConfirmed
	outcomeFailed
	outcomeRetried
)

// errScan marks a broken scan (store unreachable): the beat aborts, other
// candidates stay for the next tick.
var errScan = errors.New("scan error")

func isScanError(err error) bool {
	return errors.Is(err, errScan)
}

// deliverOne ships one candidate. It classifies every outcome:
//
//	permanent — no baseline, empty delta: FailTicketDelivery (ticket failed,
//	            audit, no PR);
//	transient — backend/git/GitHub failures: audited, stays pending;
//	success   — branch pushed + PR resolved + comment effect enqueued in the
//	            delivered transition.
func (d *Deliverer) deliverOne(ctx context.Context, c store.DeliveryCandidate) (int, error) {
	ticketID := c.Ticket.ID

	if c.BaseBranch == "" || c.BaseCommit == "" {
		// A ready result without a materialization baseline is malformed
		// durable state: the workspace can never be delivered, so fail
		// permanently instead of retrying forever.
		return d.failPermanently(ctx, ticketID, "no materialization baseline recorded for the workspace")
	}

	// 1. Read the workspace back (Alternative E: the orchestrator computes
	//    the patch from the workspace state; the worker never produced one).
	state, err := workspace.ReadWorkspaceState(ctx, d.backend, ticketID)
	if err != nil {
		return d.retryTransient(ctx, ticketID, err, "workspace read-back")
	}

	// 2. Ship the ticket branch: apply the state onto the exact baseline
	//    commit, commit deterministically, push refs/heads/<ticketID>.
	outcome, err := d.patcher.Patch(ctx, workspace.PatchRequest{
		RepoURL:     d.repoURL(c.Ticket.Mandate.Scope.Repo),
		Token:       d.opts.Token,
		BaseCommit:  c.BaseCommit,
		Branch:      ticketID,
		Message:     commitMessage(c, state),
		State:       state,
		SkipPaths:   c.BaseSkipped,
		AuthorName:  d.opts.AuthorName,
		AuthorEmail: d.opts.AuthorEmail,
	})
	if err != nil {
		if errors.Is(err, workspace.ErrEmptyChangeSet) {
			return d.failPermanently(ctx, ticketID,
				"workspace state matches the baseline; nothing to deliver (no empty PR)")
		}
		return d.retryTransient(ctx, ticketID, err, "patch production")
	}

	// 3. Open the PR (observably idempotent: an existing head-branch PR is
	//    returned, never duplicated).
	report := d.readReadyReport(ctx, ticketID)
	pr, err := d.gh.CreatePullRequest(ctx, c.Ticket.Mandate.Scope.Repo, pullRequestInput(c, state, report, outcome))
	if err != nil {
		return d.retryTransient(ctx, ticketID, err, "pull request creation")
	}

	// 4. Mark delivered: the state flip, the result-comment effect and the
	//    audit commit together (docs/designs/0001 §6). The outbox channel
	//    posts the comment after the commit, marker-idempotent.
	effect, err := resultCommentEffect(c, pr, report.summary)
	if err != nil {
		return d.retryTransient(ctx, ticketID, err, "result comment encoding")
	}
	applied, err := d.store.MarkTicketDelivered(ctx, store.DeliveredPlan{
		TicketID: ticketID,
		PRNumber: pr.Number,
		PRURL:    pr.URL,
		Branch:   ticketID,
		Commit:   outcome.Commit,
		Effect:   effect,
	})
	if err != nil {
		return d.retryTransient(ctx, ticketID, err, "delivery persistence")
	}
	if !applied {
		// A raced scan already delivered this ticket: its effect stands.
		return outcomeConfirmed, nil
	}

	d.log.Info("ticket result delivered",
		"ticket", ticketID,
		"pr", pr.URL,
		"branch", ticketID,
		"commit", outcome.Commit)
	return outcomeDelivered, nil
}

// failPermanently records a permanent delivery failure: the ticket fails with
// its audit in one transaction and no PR was pushed.
func (d *Deliverer) failPermanently(ctx context.Context, ticketID, reason string) (int, error) {
	applied, err := d.store.FailTicketDelivery(ctx, ticketID, reason)
	if err != nil {
		return 0, fmt.Errorf("%w: failed to fail the delivery of %s: %w", errScan, ticketID, err)
	}
	if applied {
		d.log.Warn("ticket delivery failed permanently",
			"ticket", ticketID, "reason", reason)
	}
	return outcomeFailed, nil
}

// retryTransient records a transient failure (audit observation) and keeps
// the candidate pending for the next beat.
func (d *Deliverer) retryTransient(ctx context.Context, ticketID string, cause error, stage string) (int, error) {
	if err := d.store.RecordAudit(ctx, ticketID, "delivery.failed", map[string]any{
		"stage":     stage,
		"error":     cause.Error(),
		"permanent": false,
	}); err != nil {
		return 0, fmt.Errorf("%w: failed to record the transient failure of %s: %w", errScan, ticketID, err)
	}
	d.log.Warn("ticket delivery retried on the next beat",
		"ticket", ticketID, "stage", stage, "error", cause)
	return outcomeRetried, nil
}

// repoURL composes the git remote of a mandate-scope repo.
func (d *Deliverer) repoURL(repo string) string {
	return workspace.ComposeRepoURL(d.gitBase(), repo)
}

func (d *Deliverer) gitBase() string {
	if d.opts.GitBaseURL == "" {
		return DefaultGitBaseURL
	}
	return d.opts.GitBaseURL
}

// readyReport is the report.ready audit detail the result comment and the PR
// description quote back to the requester.
type readyReport struct {
	summary   string
	artifacts []string
}

// readReadyReport reads the latest ready report back from the audit trail —
// the durable record the dispatcher wrote at the ready transition.
func (d *Deliverer) readReadyReport(ctx context.Context, ticketID string) readyReport {
	events, err := d.store.AuditEvents(ctx, ticketID)
	if err != nil {
		return readyReport{}
	}
	var report readyReport
	for _, ev := range events {
		if ev.Event != "report.ready" {
			continue
		}
		if s, ok := ev.Detail["summary"].(string); ok {
			report.summary = s
		}
		if raw, ok := ev.Detail["artifacts"].([]any); ok {
			report.artifacts = make([]string, 0, len(raw))
			for _, a := range raw {
				if s, ok := a.(string); ok {
					report.artifacts = append(report.artifacts, s)
				}
			}
		}
	}
	return report
}

// pullRequestInput builds the PR the requester can review: head = ticket
// branch, base = the issue-pointed branch, body referencing the ticket and
// the originating issue (T7 AC: PR 描述引用 issue/ticket).
func pullRequestInput(c store.DeliveryCandidate, state map[string][]byte, report readyReport, outcome workspace.PatchOutcome) github.PullRequestInput {
	scope := c.Ticket.Mandate.Scope
	var body strings.Builder
	fmt.Fprintf(&body, "Delivers ticket `%s` — the sandbox result for the request in #%d.\n\n",
		c.Ticket.ID, scope.Issue)
	if report.summary != "" {
		fmt.Fprintf(&body, "**Report:** %s\n\n", report.summary)
	}
	if len(report.artifacts) > 0 {
		fmt.Fprintf(&body, "**Reported artifacts:** %s\n\n", strings.Join(report.artifacts, ", "))
	}
	fmt.Fprintf(&body, "**Files in the delivery:** %d\n", len(state))
	fmt.Fprintf(&body, "**Base:** %s @ `%s` (the commit the workspace was materialized from)\n\n",
		c.BaseBranch, shortSHA(c.BaseCommit))
	fmt.Fprintf(&body, "The patch was computed orchestrator-side from the workspace read-back state (docs/designs/0003 Alternative E); commit `%s`.\n", shortSHA(outcome.Commit))

	title := c.Ticket.ID
	if line := firstLine(report.summary); line != "" {
		title = c.Ticket.ID + ": " + truncateTitle(line)
	}
	return github.PullRequestInput{
		Title: title,
		Head:  c.Ticket.ID,
		Base:  c.BaseBranch,
		Body:  body.String(),
	}
}

// commitMessage pins the delivery's provenance into the branch history. It
// must be deterministic per ticket (replays re-derive it byte for byte — the
// commit SHA and the push no-op depend on it).
func commitMessage(c store.DeliveryCandidate, state map[string][]byte) string {
	scope := c.Ticket.Mandate.Scope
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: deliver workspace result\n\n", c.Ticket.ID)
	fmt.Fprintf(&sb, "Applies the worker sandbox state for %s#%d onto the materialization baseline.\n", scope.Repo, scope.Issue)
	fmt.Fprintf(&sb, "Files: %d. Base: %s@%s.\n", len(state), c.BaseBranch, shortSHA(c.BaseCommit))
	fmt.Fprintf(&sb, "Patch computed orchestrator-side (docs/designs/0003 Alternative E).")
	return sb.String()
}

// resultCommentEffect builds the queued GitHub result-comment effect (T7 AC:
// 结果评论含 PR 链接与 Report 摘要，重放不重复): the deterministic marker rides
// the outbox idempotency key, the same discipline as the confirmation
// comment (T4).
func resultCommentEffect(c store.DeliveryCandidate, pr github.PullRequest, summary string) (store.OutboxEffect, error) {
	scope := c.Ticket.Mandate.Scope
	key := "result:" + c.Ticket.ID
	var body strings.Builder
	fmt.Fprintf(&body, "Ticket `%s` finished — opened PR #%d: %s\n\n", c.Ticket.ID, pr.Number, pr.URL)
	if summary != "" {
		fmt.Fprintf(&body, "%s\n\n", summary)
	}
	fmt.Fprintf(&body, "Branch `%s` was delivered for #%d; review and merge there.\n\n%s",
		c.Ticket.ID, scope.Issue, github.EffectMarker(key))

	payload, err := json.Marshal(github.IssueCommentEffect{
		Kind:  github.EffectKindIssueComment,
		Repo:  scope.Repo,
		Issue: scope.Issue,
		Body:  body.String(),
	})
	if err != nil {
		return store.OutboxEffect{}, fmt.Errorf("failed to encode the result comment: %w", err)
	}
	return store.OutboxEffect{
		Channel:        github.ChannelGitHub,
		Address:        fmt.Sprintf("%s#%d", scope.Repo, scope.Issue),
		IdempotencyKey: key,
		Payload:        payload,
	}, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func firstLine(s string) string {
	if at := strings.IndexByte(s, '\n'); at >= 0 {
		return strings.TrimSpace(s[:at])
	}
	return strings.TrimSpace(s)
}

func truncateTitle(s string) string {
	const max = 72
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
