// Package outbox is the delivery half of the transactional outbox
// (docs/designs/0001 §6): effects are enqueued inside the state transaction
// and delivered to their channels only after that transaction committed. A
// crash anywhere leaves a committed-but-pending effect that the next
// DeliverPending replays; the GitHub channel's deterministic marker search
// makes the replay observable-idempotent — a lost receipt can never produce a
// second comment.
//
// P0 runs a single deliverer (single-instance orchestrator, one drain loop);
// the store's pending→delivered one-way guard keeps the state transition safe
// even if a second deliverer ever races one.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// Store is the store surface the drain needs — narrow on purpose: the
// concrete *store.Store satisfies it, and the seam keeps the drain loop
// unit-testable on fakes without a PostgreSQL fixture.
type Store interface {
	PendingEffects(ctx context.Context) ([]store.Effect, error)
	MarkEffectDelivered(ctx context.Context, id int64, receipt []byte) error
}

// Deliverer drains committed-but-pending effects to their channels.
type Deliverer struct {
	store  Store
	github github.Transport
	// deferred holds effects held out of the drain behind a rate-limit
	// backoff, keyed by effect id until the recorded instant. IN-MEMORY ON
	// PURPOSE (PR #11 review, Required #4 — scope decision): the deferral
	// bounds hammering within one process lifetime; a restart resets it and
	// the first drain re-learns the deadline from the API's Retry-After
	// headers, so no durable state needs a migration. deferredMu guards it —
	// the drain loop is single-beat today, but the map must not silently
	// depend on that.
	deferredMu sync.Mutex
	deferred   map[int64]time.Time
}

// New returns a Deliverer reading the store's outbox and delivering GitHub
// effects through the transport.
func New(st Store, gh github.Transport) *Deliverer {
	return &Deliverer{store: st, github: gh, deferred: map[int64]time.Time{}}
}

// deferEffect holds the effect out of the drain until until.
func (d *Deliverer) deferEffect(id int64, until time.Time) {
	d.deferredMu.Lock()
	defer d.deferredMu.Unlock()
	d.deferred[id] = until
}

// deferredUntil reports the effect's deferral deadline, dropping expired
// entries as the scan walks.
func (d *Deliverer) deferredUntil(id int64, now time.Time) (time.Time, bool) {
	d.deferredMu.Lock()
	defer d.deferredMu.Unlock()
	until, ok := d.deferred[id]
	if !ok {
		return time.Time{}, false
	}
	if !now.Before(until) {
		delete(d.deferred, id) // the window expired: the drain may retry
		return time.Time{}, false
	}
	return until, true
}

// DeliverStats reports what one drain did.
type DeliverStats struct {
	// Delivered counts effects this drain posted to the channel.
	Delivered int
	// Confirmed counts effects already observable on the channel (marker
	// found) that were marked delivered without posting again — the replay
	// path.
	Confirmed int
	// Deferred counts effects held out of the drain behind a rate-limit
	// backoff (PR #11 review, Required #4): they stay pending; the drain
	// retries them once their Retry-After window closes.
	Deferred int
}

// DeliverPending delivers every committed-but-pending effect, oldest first.
// One effect's failure does not block the others: the error is collected and
// returned joined, and a failed effect stays pending for the next cycle. A
// GitHub rate limit is not an error of this kind: the effect is held out of
// the drain until its Retry-After window closes instead of being re-hit
// every 2s beat (the hammering pattern GitHub's abuse detection escalates),
// and the hold is reported in the stats rather than surfacing as an error.
func (d *Deliverer) DeliverPending(ctx context.Context) (DeliverStats, error) {
	effects, err := d.store.PendingEffects(ctx)
	if err != nil {
		return DeliverStats{}, fmt.Errorf("failed to read pending effects: %w", err)
	}

	var stats DeliverStats
	var errs []error
	now := time.Now()
	for _, e := range effects {
		if _, held := d.deferredUntil(e.ID, now); held {
			stats.Deferred++
			continue
		}
		switch e.Channel {
		case github.ChannelGitHub:
			delivered, err := d.deliverGitHubComment(ctx, e)
			if err != nil {
				var rl *github.RateLimitError
				if errors.As(err, &rl) && !rl.RetryAfter.IsZero() {
					// Rate-limited: hold the effect out of the drain until
					// the block lifts — never re-hit it every beat.
					d.deferEffect(e.ID, rl.RetryAfter)
					stats.Deferred++
					continue
				}
				errs = append(errs, fmt.Errorf("effect %d (%s): %w", e.ID, e.IdempotencyKey, err))
				continue
			}
			if delivered {
				stats.Delivered++
			} else {
				stats.Confirmed++
			}
		default:
			// A channel with no adapter is malformed durable data: loud
			// failure, the effect stays pending.
			errs = append(errs, fmt.Errorf("effect %d (%s): unknown channel %q", e.ID, e.IdempotencyKey, e.Channel))
		}
	}
	return stats, errors.Join(errs...)
}

// deliverGitHubComment delivers one GitHub issue-comment effect. It returns
// true when this drain posted the comment, false when the conversation
// already carried the marker (replay after a lost receipt). The effect is
// marked delivered only when the side effect is actually observable.
func (d *Deliverer) deliverGitHubComment(ctx context.Context, e store.Effect) (bool, error) {
	eff, err := github.ParseIssueCommentEffect(e.Payload)
	if err != nil {
		return false, err
	}

	needle := github.EffectMarker(e.IdempotencyKey)
	comments, err := d.github.IssueComments(ctx, eff.Repo, eff.Issue)
	if err != nil {
		return false, fmt.Errorf("failed to read conversation %s#%d for the marker search: %w", eff.Repo, eff.Issue, err)
	}
	for _, c := range comments {
		if !strings.Contains(c.Body, needle) {
			continue
		}
		// The effect is already observable on GitHub — this drain is a
		// replay after a crash between post and receipt. Record the receipt
		// and do not post again.
		receipt, _ := json.Marshal(map[string]any{
			"matched_marker":      needle,
			"existing_comment_id": c.ID,
		})
		if err := d.store.MarkEffectDelivered(ctx, e.ID, receipt); err != nil &&
			!errors.Is(err, store.ErrEffectNotPending) {
			return false, fmt.Errorf("failed to record the marker-match receipt: %w", err)
		}
		return false, nil
	}

	commentID, err := d.github.PostIssueComment(ctx, eff.Repo, eff.Issue, eff.Body)
	if err != nil {
		return false, fmt.Errorf("failed to post the confirmation comment on %s#%d: %w", eff.Repo, eff.Issue, err)
	}

	receipt, err := json.Marshal(map[string]any{"comment_id": commentID})
	if err != nil {
		return false, fmt.Errorf("failed to encode the delivery receipt: %w", err)
	}
	if err := d.store.MarkEffectDelivered(ctx, e.ID, receipt); err != nil &&
		!errors.Is(err, store.ErrEffectNotPending) {
		return false, fmt.Errorf("failed to record the delivery receipt: %w", err)
	}
	return true, nil
}
