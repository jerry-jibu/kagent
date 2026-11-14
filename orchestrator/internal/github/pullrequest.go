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
// — a delivery replay after a lost receipt must never open a second PR. Any
// other failure surfaces as an error for the deliverer to retry.
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
		return t.findOpenPullRequest(ctx, repo, in.Head)
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

// findOpenPullRequest resolves the existing open PR for a head branch via
// GET /repos/{owner}/{repo}/pulls?head={owner}:{head}&state=open.
func (t *httpTransport) findOpenPullRequest(ctx context.Context, repo, head string) (PullRequest, error) {
	owner := repo
	if at := strings.IndexByte(repo, '/'); at > 0 {
		owner = repo[:at]
	}
	q := "head=" + owner + ":" + head + "&state=open&per_page=1"
	endpoint := fmt.Sprintf("%s/repos/%s/pulls?%s", t.base, repo, q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return PullRequest{}, fmt.Errorf("failed to build PR lookup for %s: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return PullRequest{}, fmt.Errorf("PR lookup to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PullRequest{}, fmt.Errorf("PR lookup to %s failed: HTTP %d", endpoint, resp.StatusCode)
	}
	var wire []wirePullRequest
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return PullRequest{}, fmt.Errorf("failed to decode PR lookup from %s: %w", endpoint, err)
	}
	if len(wire) == 0 {
		return PullRequest{}, fmt.Errorf("GitHub refused the PR for head %s but no existing open PR was found", head)
	}
	return PullRequest{
		Number: wire[0].Number,
		URL:    wire[0].HTMLURL,
		Head:   wire[0].Head.Ref,
		Base:   wire[0].Base.Ref,
		State:  wire[0].State,
	}, nil
}

// wirePullRequest mirrors the GitHub pull request response object (the
// subset the orchestrator consumes).
type wirePullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    wireRef `json:"head"`
	Base    wireRef `json:"base"`
}

type wireRef struct {
	Ref string `json:"ref"`
}
