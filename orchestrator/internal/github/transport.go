package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Transport is the orchestrator's GitHub seam (spec: GitHubTransport): the
// only place external GitHub state enters the control plane. Production
// implementations dial the GitHub API; tests inject a Fake-backed transport.
// Read side (Sources) and write side (Outbox Channels) share the interface:
// the orchestrator holds the only GitHub credentials (docs/designs/0001 §7).
type Transport interface {
	// PollIssueComments returns issue comments of repo updated at or after
	// since, oldest first, at most limit. A zero since reads from the
	// beginning of the repository's history.
	PollIssueComments(ctx context.Context, repo string, since time.Time, limit int) ([]IssueComment, error)
	// IssueComments returns the current comments of one issue conversation,
	// oldest first. The Outbox GitHub channel reads a conversation before
	// posting to detect its own earlier deliveries (deterministic marker).
	IssueComments(ctx context.Context, repo string, issue int) ([]IssueComment, error)
	// PostIssueComment posts one comment on the issue conversation and
	// returns the created comment's server-assigned id.
	PostIssueComment(ctx context.Context, repo string, issue int, body string) (int64, error)
	// CreatePullRequest opens the PR from the delivered ticket branch to its
	// base branch (T7). Observably idempotent: a head branch that already
	// carries an open PR returns the EXISTING PR, never a duplicate — a
	// delivery replay after a lost receipt is safe.
	CreatePullRequest(ctx context.Context, repo string, in PullRequestInput) (PullRequest, error)
}

// TransportConfig configures the production HTTP transport.
type TransportConfig struct {
	// BaseURL of the GitHub REST API. Empty defaults to https://api.github.com.
	BaseURL string
	// Token is the bearer token; empty means unauthenticated.
	Token string
	// Client overrides the HTTP client (timeouts, transports). Nil defaults
	// to a client with a 30s timeout.
	Client *http.Client
}

// IssueComment is one GitHub conversation comment (issue or PR conversation),
// the trigger source of the P0 pipeline.
type IssueComment struct {
	ID          int64
	IssueNumber int
	Author      string
	Body        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	URL         string
}

// DeterministicTriggerID returns the trigger's stable identity: the comment's
// immutable GitHub id under its event kind. The same comment observed any
// number of times — replays, overlapping poll windows — always yields the
// same id, which is what makes (source, deterministic_id) dedup work
// (docs/designs/0001 §6).
func (c IssueComment) DeterministicTriggerID() string {
	return fmt.Sprintf("issue-comment:%d", c.ID)
}

// ThreadKey returns the external conversation key the comment belongs to.
// PR conversation comments are issue comments in GitHub's model, so issues
// and PRs share the same key shape.
func (c IssueComment) ThreadKey() string {
	return fmt.Sprintf("issue:%d", c.IssueNumber)
}

// SourceName returns the orchestrator source id for a repository full name.
func SourceName(repo string) string {
	return "github:" + repo
}

// githubMaxPerPage is the GitHub API's per_page ceiling.
const githubMaxPerPage = 100

// NewTransport returns the production HTTP transport.
func NewTransport(cfg TransportConfig) Transport {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &httpTransport{
		base:   strings.TrimSuffix(base, "/"),
		token:  cfg.Token,
		client: client,
	}
}

type httpTransport struct {
	base   string
	token  string
	client *http.Client
}

// PollIssueComments walks result pages (per_page at the API ceiling) until
// the limit is filled or the pages are exhausted.
func (t *httpTransport) PollIssueComments(ctx context.Context, repo string, since time.Time, limit int) ([]IssueComment, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("poll limit must be positive, got %d", limit)
	}

	var out []IssueComment
	for page := 1; len(out) < limit; page++ {
		comments, lastPage, err := t.pollPage(ctx, repo, since, page, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, comments...)
		if !lastPage {
			break
		}
		if page > limit/githubMaxPerPage+1 {
			// Defensive bound: pages needed can never exceed ceil(limit/100)+1.
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// pollPage fetches one page; lastPage reports whether this page was full
// (i.e. a next page may exist).
func (t *httpTransport) pollPage(ctx context.Context, repo string, since time.Time, page, limit int) ([]IssueComment, bool, error) {
	perPage := limit
	if perPage > githubMaxPerPage {
		perPage = githubMaxPerPage
	}

	q := url.Values{}
	q.Set("sort", "created")
	q.Set("direction", "asc")
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("page", strconv.Itoa(page))
	if !since.IsZero() {
		// GitHub's `since` filter is on the comment's updated time: edited
		// comments re-appear, and their already-existing trigger ids make the
		// re-appearance a no-op downstream.
		q.Set("since", since.UTC().Format(time.RFC3339))
	}

	endpoint := fmt.Sprintf("%s/repos/%s/issues/comments?%s", t.base, repo, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to build poll request for %s: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("poll request to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("poll request to %s failed: HTTP %d", endpoint, resp.StatusCode)
	}

	var wire []wireComment
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, false, fmt.Errorf("failed to decode poll response from %s: %w", endpoint, err)
	}

	comments := make([]IssueComment, 0, len(wire))
	for _, wc := range wire {
		number, err := issueNumberFromURL(wc.IssueURL)
		if err != nil {
			return nil, false, fmt.Errorf("failed to parse issue reference %q: %w", wc.IssueURL, err)
		}
		comments = append(comments, IssueComment{
			ID:          wc.ID,
			IssueNumber: number,
			Author:      wc.User.Login,
			Body:        wc.Body,
			CreatedAt:   wc.CreatedAt,
			UpdatedAt:   wc.UpdatedAt,
			URL:         wc.HTMLURL,
		})
	}
	return comments, len(wire) == perPage, nil
}

// issueNumberFromURL extracts the issue/PR number from an issue_url like
// ".../repos/{owner}/{repo}/issues/{number}".
func issueNumberFromURL(issueURL string) (int, error) {
	segments := strings.Split(strings.TrimRight(issueURL, "/"), "/")
	if len(segments) == 0 {
		return 0, fmt.Errorf("empty issue url")
	}
	number, err := strconv.Atoi(segments[len(segments)-1])
	if err != nil {
		return 0, err
	}
	return number, nil
}

// IssueComments walks the issue-conversation pages (per_page at the API
// ceiling) until a short page ends the listing.
func (t *httpTransport) IssueComments(ctx context.Context, repo string, issue int) ([]IssueComment, error) {
	var out []IssueComment
	for page := 1; ; page++ {
		wire, lastPage, err := t.issueCommentsPage(ctx, repo, issue, page)
		if err != nil {
			return nil, err
		}
		out = append(out, wire...)
		if !lastPage {
			return out, nil
		}
	}
}

// issueCommentsPage fetches one page of an issue's comments; lastPage reports
// whether this page was full (i.e. a next page may exist).
func (t *httpTransport) issueCommentsPage(ctx context.Context, repo string, issue, page int) ([]IssueComment, bool, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(githubMaxPerPage))
	q.Set("page", strconv.Itoa(page))

	endpoint := fmt.Sprintf("%s/repos/%s/issues/%d/comments?%s", t.base, repo, issue, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to build comment list request for %s#%d: %w", repo, issue, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("comment list request to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("comment list request to %s failed: HTTP %d", endpoint, resp.StatusCode)
	}

	var wire []wireComment
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, false, fmt.Errorf("failed to decode comment list response from %s: %w", endpoint, err)
	}

	comments := make([]IssueComment, 0, len(wire))
	for _, wc := range wire {
		number, err := issueNumberFromURL(wc.IssueURL)
		if err != nil {
			return nil, false, fmt.Errorf("failed to parse issue reference %q: %w", wc.IssueURL, err)
		}
		comments = append(comments, IssueComment{
			ID:          wc.ID,
			IssueNumber: number,
			Author:      wc.User.Login,
			Body:        wc.Body,
			CreatedAt:   wc.CreatedAt,
			UpdatedAt:   wc.UpdatedAt,
			URL:         wc.HTMLURL,
		})
	}
	return comments, len(wire) == githubMaxPerPage, nil
}

// PostIssueComment posts one comment on the issue conversation via
// POST /repos/{owner}/{repo}/issues/{number}/comments and returns the created
// comment's id.
func (t *httpTransport) PostIssueComment(ctx context.Context, repo string, issue int, body string) (int64, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/issues/%d/comments", t.base, repo, issue)
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return 0, fmt.Errorf("failed to encode comment body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("failed to build comment post request for %s#%d: %w", repo, issue, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("comment post request to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("comment post request to %s failed: HTTP %d", endpoint, resp.StatusCode)
	}

	var created wireComment
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return 0, fmt.Errorf("failed to decode comment post response from %s: %w", endpoint, err)
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("comment post response from %s carries no comment id", endpoint)
	}
	return created.ID, nil
}
