// Package source runs the polling side of the ingest chain (docs/designs/
// 0001 §2: Sources, 确定性 Trigger ID 由 transport 事件承载): each Source owns
// one repository inbox, a persisted cursor, and a rewind window that makes
// consecutive polls overlap so boundary events are always observed twice and
// deduplicated downstream. Polling never mutates state; the store's atomic
// ingest is the only write path.
package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
)

// Poll defaults (docs/designs/0001 §2.2: GitHub 轮询 60s).
const (
	DefaultPollInterval = 60 * time.Second
	DefaultBatchSize    = 500
	// DefaultOverlap rewinds the persisted cursor 5s per poll so consecutive
	// poll windows overlap; the trigger dedup absorbs the overlap
	// (docs/designs/0001 §6).
	DefaultOverlap = 5 * time.Second
)

// Source configures one polling source instance.
type Source struct {
	// Repo is the full name ("owner/name") this source polls.
	Repo string
	// PollInterval is the tick cadence. Zero uses DefaultPollInterval.
	PollInterval time.Duration
	// BatchSize caps how many comments one poll fetches and ingests.
	// Zero uses DefaultBatchSize.
	BatchSize int
	// Overlap is the cursor rewind per poll. Zero uses DefaultOverlap.
	Overlap time.Duration
	// StartCursor is where a source with no persisted cursor begins reading.
	// Zero reads from the origin of the repository's history; use it to skip
	// backlogs on busy repositories.
	StartCursor time.Time
}

func (s Source) normalize() (Source, error) {
	if s.Repo == "" {
		return Source{}, fmt.Errorf("%w: repo must be owner/name", ErrMisconfigured)
	}
	if s.PollInterval <= 0 {
		s.PollInterval = DefaultPollInterval
	}
	if s.BatchSize <= 0 {
		s.BatchSize = DefaultBatchSize
	}
	if s.Overlap <= 0 {
		s.Overlap = DefaultOverlap
	}
	return s, nil
}

// ErrMisconfigured wraps source configuration errors; construction refuses a
// source the poll loop could not run.
var ErrMisconfigured = errors.New("source is misconfigured")

// PollResult reports what one poll cycle observed.
type PollResult struct {
	// Source is the source name the cycle ran for.
	Source string
	// Since is the read position the poll used (persisted cursor rewound by
	// the overlap, or the start cursor on a fresh source).
	Since time.Time
	// Fetched, Ingested, Duplicates, Dropped mirror the ingest stats for the
	// comments fetched in this cycle.
	Fetched    int
	Ingested   int
	Duplicates int
	Dropped    int
}

// Poller drives one source: cursor read, transport poll, atomic ingest.
type Poller struct {
	transport github.Transport
	ingest    *intake.Intake
	src       Source
}

// New builds a Poller for a source, applying poll defaults. Misconfigured
// sources (no repository) are refused so the run loop cannot start in a
// state it could never serve.
func New(transport github.Transport, ingest *intake.Intake, src Source) (*Poller, error) {
	src, err := src.normalize()
	if err != nil {
		return nil, err
	}
	return &Poller{transport: transport, ingest: ingest, src: src}, nil
}

// PollOnce runs one poll cycle: read the persisted cursor (or the start
// cursor), rewind it by the overlap window, fetch one batch and ingest it
// atomically. The cursor only advances inside the store's ingest
// transaction, so a crash mid-cycle loses nothing and the next poll re-reads
// the window.
func (p *Poller) PollOnce(ctx context.Context) (PollResult, error) {
	srcName := github.SourceName(p.src.Repo)
	res := PollResult{Source: srcName}

	cursor, ok, err := p.ingest.SourceCursor(ctx, p.src.Repo)
	if err != nil {
		return res, fmt.Errorf("failed to read cursor for %s: %w", srcName, err)
	}
	since := p.src.StartCursor
	if ok {
		// The persisted read position wins; the rewind creates the overlap
		// window that dedup absorbs.
		since = cursor.Add(-p.src.Overlap)
	}
	res.Since = since

	comments, err := p.transport.PollIssueComments(ctx, p.src.Repo, since, p.src.BatchSize)
	if err != nil {
		return res, fmt.Errorf("failed to poll %s since %s: %w", p.src.Repo, since.Format(time.RFC3339), err)
	}
	res.Fetched = len(comments)

	stats, err := p.ingest.IngestIssueComments(ctx, p.src.Repo, comments)
	if err != nil {
		return res, fmt.Errorf("failed to ingest batch for %s: %w", srcName, err)
	}
	res.Ingested = stats.Ingested
	res.Duplicates = stats.Duplicates
	res.Dropped = stats.Dropped
	return res, nil
}

// Run polls on the configured cadence until ctx ends: one immediate cycle
// (catch-up on start) then one per tick. Errors are logged per cycle and
// never stop the loop — a failing GitHub or a claimed inbox (single active
// worker) is retried on the next tick; the overlap window keeps events safe.
func (p *Poller) Run(ctx context.Context, log *slog.Logger) {
	interval := p.src.PollInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.cycle(ctx, log)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.cycle(ctx, log)
		}
	}
}

func (p *Poller) cycle(ctx context.Context, log *slog.Logger) {
	res, err := p.PollOnce(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error("github poll cycle failed", "source", res.Source, "error", err)
		return
	}
	if res.Fetched == 0 {
		log.Debug("github poll empty", "source", res.Source, "since", res.Since.Format(time.RFC3339))
		return
	}
	log.Info("github poll ingested", "source", res.Source,
		"since", res.Since.Format(time.RFC3339),
		"fetched", res.Fetched, "ingested", res.Ingested,
		"duplicates", res.Duplicates, "dropped", res.Dropped)
}
