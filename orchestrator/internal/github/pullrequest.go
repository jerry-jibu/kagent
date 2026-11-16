package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PullRequestInput is one PR creation request (0001 §4.1 ⑥⑦: the orchestrator
// opens the PR for the delivered ticket branch). Head and Base are branch
// names inside the same repository.
type PullRequestInput struct {
	Title string
	Head  string
	Base  string
	Body  string
}

// PullRequest is the created (or already-existing) pull request. URL is the
// html_url the result comment links to. State mirrors the GitHub PR state
// ("open" for everything the orchestrator creates).
type PullRequest struct {
	Number int
	URL    string
	Head   string
	Base   string
	State  string
}

// CreatePullRequest opens a pull request from the ticket branch to the base
// branch. It is observably idempotent: when a PR for the head branch already
// exists (GitHub refuses the duplicate with 422), the EXISTING PR is returned
// — a delivery replay after a lost receipt must never open a second PR. This
// holds even when the existing PR was since closed or merged: GitHub still
// answers 422, but its state=open listing no longer shows the PR, so the
// resolution falls back to state=all and returns the closed PR with its real
// state (PR #11 review) rather than looping a permanent error until the
// delivery deadline. Any other failure surfaces as an error for the
// deliverer to retry.
func (t *httpTransport) CreatePullRequest(ctx context.Context, repo string, in PullRequestInput) (PullRequest, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/pulls", t.base, repo)
	payload, err := json.Marshal(map[string]string{
		"title": in.Title,
		"head":  in.Head,
		"base":  in.Base,
		"body":  in.Body,
	})
	if err != nil {
		return PullRequest{}, fmt.Errorf("failed to encode the PR body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return PullRequest{}, fmt.Errorf("failed to build PR request for %s: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return PullRequest{}, fmt.Errorf("PR request to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var created wirePullRequest
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			return PullRequest{}, fmt.Errorf("failed to decode PR response from %s: %w", endpoint, err)
		}
		if created.Number == 0 || created.HTMLURL == "" {
			return PullRequest{}, fmt.Errorf("PR response from %s carries no identity", endpoint)
		}
		return PullRequest{
			Number: created.Number,
			URL:    created.HTMLURL,
			Head:   created.Head.Ref,
			Base:   created.Base.Ref,
			State:  created.State,
		}, nil

	case resp.StatusCode == http.StatusUnprocessableEntity && isDuplicatePR(resp):
		// A PR for this head branch already exists — find and return it.
		return t.findExistingPullRequest(ctx, repo, in.Head)
	default:
		return PullRequest{}, fmt.Errorf("PR request to %s failed: HTTP %d", endpoint, resp.StatusCode)
	}
}

// isDuplicatePR reports whether the 422 response is GitHub's
// "A pull request already exists" refusal (as opposed to any other
// validation failure). The body is small; the caller abandons it afterwards.
func isDuplicatePR(resp *http.Response) bool {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), "already exists")
}

// findExistingPullRequest resolves the PR GitHub already holds for a head
// branch: the open one when one exists, otherwise — a replay racing a
// merged/closed PR answers 422 "already exists" while the state=open lookup
// comes back EMPTY (PR #11 review) — the most recent PR of any state.
// Returning that closed PR keeps the replay idempotent and the terminal
// state honest: the deliverer records the PR that genuinely exists, with the
// state GitHub reports, instead of retrying a permanent "no existing PR"
// error until the deadline. Pretending success is not a hazard here — the
// PR link and number are real, and State carries "closed" for any caller
// that cares.
func (t *httpTransport) findExistingPullRequest(ctx context.Context, repo, head string) (PullRequest, error) {
	open, err := t.lookupPullRequests(ctx, repo, head, "open")
	if err != nil {
		return PullRequest{}, err
	}
	if len(open) > 0 {
		return open[0], nil
	}
	all, err := t.lookupPullRequests(ctx, repo, head, "all")
	if err != nil {
		return PullRequest{}, err
	}
	if len(all) == 0 {
		return PullRequest{}, fmt.Errorf("GitHub refused the PR for head %s but no existing PR was found", head)
	}
	return all[0], nil
}

// lookupPullRequests lists the PRs GitHub holds for a head branch with the
// given state filter (GET /repos/{owner}/{repo}/pulls?head={owner}:{branch}
// &state=...). GitHub orders by creation descending, so the first entry is
// the most recent PR for the branch.
func (t *httpTransport) lookupPullRequests(ctx context.Context, repo, head, state string) ([]PullRequest, error) {
	owner := repo
	if at := strings.IndexByte(repo, '/'); at > 0 {
		owner = repo[:at]
	}
	q := "head=" + owner + ":" + head + "&state=" + state + "&per_page=1"
	endpoint := fmt.Sprintf("%s/repos/%s/pulls?%s", t.base, repo, q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build PR lookup for %s: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PR lookup to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("PR lookup to %s failed: HTTP %d", endpoint, resp.StatusCode)
	}
	var wire []wirePullRequest
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("failed to decode PR lookup from %s: %w", endpoint, err)
	}
	prs := make([]PullRequest, 0, len(wire))
	for _, w := range wire {
		prs = append(prs, PullRequest{
			Number: w.Number,
			URL:    w.HTMLURL,
			Head:   w.Head.Ref,
			Base:   w.Base.Ref,
			State:  w.State,
		})
	}
	return prs, nil
}

// wirePullRequest mirrors the GitHub pull request response object (the
// subset the orchestrator consumes).
type wirePullRequest struct {
	Number  int     `json:"number"`
	HTMLURL string  `json:"html_url"`
	State   string  `json:"state"`
	Head    wireRef `json:"head"`
	Base    wireRef `json:"base"`
}

type wireRef struct {
	Ref string `json:"ref"`
}
