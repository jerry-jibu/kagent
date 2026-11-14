package github

import (
	"encoding/json"
	"fmt"
)

// ChannelGitHub is the outbox channel name of the GitHub channel adapter.
const ChannelGitHub = "github"

// EffectKindIssueComment identifies an effect payload that posts one comment
// on an issue conversation.
const EffectKindIssueComment = "github-issue-comment"

// IssueCommentEffect is the wire payload of a GitHub issue-comment effect
// (docs/designs/0001 §3: Channel = Outbox 的 Channel 适配器). The Engine
// encodes it when enqueuing the effect; the channel decodes it when
// delivering, so the JSON shape is a contract between the two.
type IssueCommentEffect struct {
	Kind  string `json:"kind"`
	Repo  string `json:"repo"`
	Issue int    `json:"issue"`
	Body  string `json:"body"`
}

// ParseIssueCommentEffect decodes an effect payload into the contract.
// Malformed durable data is rejected, never silently decoded into a zero
// value.
func ParseIssueCommentEffect(payload []byte) (IssueCommentEffect, error) {
	var e IssueCommentEffect
	if err := json.Unmarshal(payload, &e); err != nil {
		return IssueCommentEffect{}, fmt.Errorf("effect payload is not an IssueCommentEffect: %w", err)
	}
	switch {
	case e.Kind != EffectKindIssueComment:
		return IssueCommentEffect{}, fmt.Errorf("effect payload kind %q is not %q", e.Kind, EffectKindIssueComment)
	case e.Repo == "":
		return IssueCommentEffect{}, fmt.Errorf("effect payload names no repo")
	case e.Issue <= 0:
		return IssueCommentEffect{}, fmt.Errorf("effect payload issue %d is not a GitHub conversation number", e.Issue)
	case e.Body == "":
		return IssueCommentEffect{}, fmt.Errorf("effect payload body is empty")
	}
	return e, nil
}

// EffectMarker returns the deterministic invisible marker the orchestrator
// embeds in every comment it posts for an effect idempotency key
// (docs/designs/0001 §6: GitHub 评论用确定性 marker、重复投递可检出). The
// sender embeds it in the body; a retrying deliverer searches the
// conversation for it before posting again, so a lost receipt can never
// produce a second comment.
func EffectMarker(idempotencyKey string) string {
	return fmt.Sprintf("<!-- superagent:effect:%s -->", idempotencyKey)
}
