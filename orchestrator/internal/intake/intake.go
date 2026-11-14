// Package intake turns external transport events into durable, deduplicated
// triggers with thread attribution (docs/designs/0001 §2: Intake 去重·Thread
// 归属). The dedup and sightline rules themselves live in the store; intake
// maps transport shapes onto them and owns the trigger payload contract that
// later evaluation stages decode.
package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// DefaultSightline is how long a thread without a live ticket stays
// ingestible after its last ingested activity (docs/designs/0001: 无活
// Ticket 的 Thread 保留 14 天视线).
const DefaultSightline = 14 * 24 * time.Hour

// CommentPayload is the audit payload persisted with an issue-comment
// trigger. Evaluation stages (Gate, evaluator) decode trigger payloads into
// this type, so its JSON shape is a contract.
type CommentPayload struct {
	Kind      string    `json:"kind"`
	Repo      string    `json:"repo"`
	Issue     int       `json:"issue"`
	CommentID int64     `json:"comment_id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

// DecodeCommentPayload decodes a stored trigger payload into the intake
// contract. Evaluation stages (Gate, evaluator) decode trigger payloads
// through this function: trigger rows are immutable audit data, so a
// payload that no longer decodes into the contract is malformed durable
// data and is rejected — never silently decoded into a zero value.
func DecodeCommentPayload(payload []byte) (CommentPayload, error) {
	var pl CommentPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		return CommentPayload{}, fmt.Errorf("payload is not a CommentPayload: %w", err)
	}
	switch {
	case pl.Kind == "":
		return CommentPayload{}, fmt.Errorf("payload kind is empty: not an intake payload")
	case pl.Repo == "":
		return CommentPayload{}, fmt.Errorf("payload repo is empty: not a CommentPayload")
	case pl.Author == "":
		return CommentPayload{}, fmt.Errorf("payload author is empty: not a CommentPayload")
	}
	return pl, nil
}

// Intake ingests transport events into the store.
type Intake struct {
	store     *store.Store
	sightline time.Duration
}

// New returns an Intake applying the DefaultSightline.
func New(st *store.Store) *Intake {
	return &Intake{store: st, sightline: DefaultSightline}
}

// IngestIssueComments maps a poll batch into trigger events and ingests it
// atomically. The cursor high-water mark covers the whole batch, so the
// caller may treat the batch as fully consumed regardless of the stats mix.
func (i *Intake) IngestIssueComments(ctx context.Context, repo string, comments []github.IssueComment) (store.IngestStats, error) {
	if len(comments) == 0 {
		return store.IngestStats{}, nil
	}

	events := make([]store.Event, 0, len(comments))
	for _, c := range comments {
		payload, err := json.Marshal(CommentPayload{
			Kind:      "issue-comment",
			Repo:      repo,
			Issue:     c.IssueNumber,
			CommentID: c.ID,
			Author:    c.Author,
			Body:      c.Body,
			URL:       c.URL,
			CreatedAt: c.CreatedAt,
		})
		if err != nil {
			return store.IngestStats{}, fmt.Errorf("failed to encode comment %d: %w", c.ID, err)
		}
		events = append(events, store.Event{
			ThreadKey:       c.ThreadKey(),
			DeterministicID: c.DeterministicTriggerID(),
			Payload:         payload,
			OccurredAt:      c.CreatedAt,
		})
	}

	stats, err := i.store.IngestEvents(ctx,
		store.SourceRef{Name: github.SourceName(repo), Kind: "github"}, events, i.sightline)
	if err != nil {
		return store.IngestStats{}, fmt.Errorf("failed to ingest %d comments for %s: %w", len(comments), repo, err)
	}
	return stats, nil
}

// SourceCursor returns the persisted read position of a source. The poller
// reads it once per cycle so a restart resumes from the store's checkpoint
// instead of re-reading from the beginning.
func (i *Intake) SourceCursor(ctx context.Context, repo string) (time.Time, bool, error) {
	return i.store.SourceCursor(ctx, github.SourceName(repo))
}
