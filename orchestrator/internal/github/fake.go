// Package github is the orchestrator's GitHub seam: the Transport interface
// (the one place external GitHub state enters the control plane) plus its
// production HTTP implementation. The orchestrator holds the credentials;
// nothing downstream ever talks to GitHub directly.
package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory GitHub serving the issue-comments poll endpoint and
// the PR creation endpoint with the same wire contract the production
// transport speaks. It exists so tests exercise the real HTTP client against
// a faithful server instead of a mock of the client's own assumptions.
//
// Semantics mirrored from the GitHub REST API
// (GET /repos/{owner}/{repo}/issues/comments): `since` filters on the
// comment's updated time, results are sorted by created time, `per_page`
// (default 30) and `page` (1-based) paginate. PR creation refuses a
// duplicate head branch with 422 "already exists" (and the transport then
// resolves the existing PR through the pulls listing).
type Fake struct {
	mu       sync.Mutex
	comments []IssueComment
	posted   []IssueComment
	pulls    []PullRequest
	nextID   int64
	nextPR   int
	botLogin string
	srv      *httptest.Server
	lastAuth string
	failCode int
	failBody string
	requests int
}

// NewFake starts a Fake GitHub server.
func NewFake() *Fake {
	f := &Fake{nextID: 900000, nextPR: 400, botLogin: "orchestrator"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/comments", f.handleListIssueComments)
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}/comments", f.handleListIssueConversation)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", f.handlePostIssueComment)
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", f.handleCreatePullRequest)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", f.handleListPullRequests)
	f.srv = httptest.NewServer(mux)
	return f
}

// URL returns the fake server's base URL (TransportConfig.BaseURL).
func (f *Fake) URL() string { return f.srv.URL }

// Close shuts the fake server down.
func (f *Fake) Close() { f.srv.Close() }

// AddComment scripts one comment into the fake.
func (f *Fake) AddComment(c IssueComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, c)
}

// AddComments scripts a batch of comments into the fake.
func (f *Fake) AddComments(cs []IssueComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, cs...)
}

// Comments returns a snapshot of the scripted comments.
func (f *Fake) Comments() []IssueComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]IssueComment(nil), f.comments...)
}

// LastAuthorization returns the Authorization header of the most recent
// request, or "" when none was sent.
func (f *Fake) LastAuthorization() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

// FailWith makes the fake respond with an error status and body.
func (f *Fake) FailWith(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCode = status
	f.failBody = body
}

// SetOK clears a failure previously configured with FailWith.
func (f *Fake) SetOK() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCode = 0
	f.failBody = ""
}

// RequestCount returns the number of requests the fake has served.
func (f *Fake) RequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// PostedComments returns the comments created through the POST endpoint, in
// posting order. Comments posted by the orchestrator are also part of the
// fake's comment store, so a marker search over IssueComments sees the
// orchestrator's own earlier deliveries.
func (f *Fake) PostedComments() []IssueComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]IssueComment(nil), f.posted...)
}

// PullRequests returns every PR the fake holds.
func (f *Fake) PullRequests() []PullRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PullRequest(nil), f.pulls...)
}

// HasPullRequest reports whether an open PR exists for the head branch.
func (f *Fake) HasPullRequest(repo, head string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pr := range f.pulls {
		if pr.Head == head {
			return true
		}
	}
	return false
}

// OpenPullRequestCount counts the open PRs for a head branch — the duplicate
// the replay guard must keep at exactly one.
func (f *Fake) OpenPullRequestCount(repo, head string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, pr := range f.pulls {
		if pr.Head == head {
			n++
		}
	}
	return n
}

// handleCreatePullRequest mirrors POST /repos/{owner}/{repo}/pulls: 201 with
// the created PR, 422 "A pull request already exists for ..." when the head
// branch already carries an open PR.
func (f *Fake) handleCreatePullRequest(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.lastAuth = r.Header.Get("Authorization")
	failCode, failBody := f.failCode, f.failBody
	f.mu.Unlock()

	if failCode != 0 {
		http.Error(w, failBody, failCode)
		return
	}

	var in struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pr := range f.pulls {
		if pr.Head == in.Head {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"message":"Validation Failed","errors":["A pull request already exists for %s."]}`, in.Head)))
			return
		}
	}
	pr := PullRequest{
		Number: f.nextPR,
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.PathValue("owner"), r.PathValue("repo"), f.nextPR),
		Head:   in.Head,
		Base:   in.Base,
		State:  "open",
	}
	f.nextPR++
	f.pulls = append(f.pulls, pr)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toWirePullRequest(pr))
}

// handleListPullRequests mirrors GET /repos/{owner}/{repo}/pulls with the
// `head` (owner:branch) and `state` filters the duplicate-PR resolution uses.
func (f *Fake) handleListPullRequests(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	failCode, failBody := f.failCode, f.failBody
	pulls := append([]PullRequest(nil), f.pulls...)
	f.mu.Unlock()

	if failCode != 0 {
		http.Error(w, failBody, failCode)
		return
	}

	// head=owner:branch — the fake filters on the branch part.
	headFilter := strings.TrimPrefix(r.URL.Query().Get("head"), r.PathValue("owner")+":")
	state := r.URL.Query().Get("state")

	wire := make([]wirePullRequest, 0, len(pulls))
	for _, pr := range pulls {
		if headFilter != "" && pr.Head != headFilter {
			continue
		}
		if state != "" && state != "all" && pr.State != state {
			continue
		}
		wire = append(wire, toWirePullRequest(pr))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire)
}

func toWirePullRequest(pr PullRequest) wirePullRequest {
	return wirePullRequest{
		Number:  pr.Number,
		HTMLURL: pr.URL,
		State:   pr.State,
		Head:    wireRef{Ref: pr.Head},
		Base:    wireRef{Ref: pr.Base},
	}
}

func (f *Fake) handleListIssueConversation(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	failCode, failBody := f.failCode, f.failBody
	comments := append([]IssueComment(nil), f.comments...)
	f.mu.Unlock()

	if failCode != 0 {
		http.Error(w, failBody, failCode)
		return
	}

	q := r.URL.Query()
	perPage := 30
	if raw := q.Get("per_page"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			perPage = parsed
		}
	}
	page := 1
	if raw := q.Get("page"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			page = parsed
		}
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.Error(w, "invalid issue number", http.StatusBadRequest)
		return
	}

	matched := make([]IssueComment, 0, len(comments))
	for _, c := range comments {
		if c.IssueNumber == number {
			matched = append(matched, c)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].CreatedAt.Before(matched[j].CreatedAt) })

	start := (page - 1) * perPage
	if start >= len(matched) {
		matched = nil
	} else {
		end := start + perPage
		if end > len(matched) {
			end = len(matched)
		}
		matched = matched[start:end]
	}

	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	wire := make([]wireComment, 0, len(matched))
	for _, c := range matched {
		wire = append(wire, toWire(c, repo))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire)
}

func (f *Fake) handlePostIssueComment(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	failCode, failBody := f.failCode, f.failBody
	f.mu.Unlock()

	if failCode != 0 {
		http.Error(w, failBody, failCode)
		return
	}

	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.Error(w, "invalid issue number", http.StatusBadRequest)
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	now := time.Now().UTC()
	c := IssueComment{
		ID:          f.nextID,
		IssueNumber: number,
		Author:      f.botLogin,
		Body:        body.Body,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	f.nextID++
	f.comments = append(f.comments, c)
	f.posted = append(f.posted, c)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toWire(c, r.PathValue("owner")+"/"+r.PathValue("repo")))
}

func (f *Fake) handleListIssueComments(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	auth := r.Header.Get("Authorization")
	f.lastAuth = auth
	f.requests++
	failCode, failBody := f.failCode, f.failBody
	comments := append([]IssueComment(nil), f.comments...)
	f.mu.Unlock()

	if failCode != 0 {
		http.Error(w, failBody, failCode)
		return
	}

	q := r.URL.Query()
	var since time.Time
	if raw := q.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(w, "invalid since", http.StatusBadRequest)
			return
		}
		since = parsed
	}
	perPage := 30
	if raw := q.Get("per_page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed > 0 {
			perPage = parsed
		}
	}
	page := 1
	if raw := q.Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed > 0 {
			page = parsed
		}
	}

	matched := make([]IssueComment, 0, len(comments))
	for _, c := range comments {
		if !since.IsZero() && c.UpdatedAt.Before(since) {
			continue
		}
		matched = append(matched, c)
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].CreatedAt.Before(matched[j].CreatedAt) })

	start := (page - 1) * perPage
	if start >= len(matched) {
		matched = nil
	} else {
		end := start + perPage
		if end > len(matched) {
			end = len(matched)
		}
		matched = matched[start:end]
	}

	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	wire := make([]wireComment, 0, len(matched))
	for _, c := range matched {
		wire = append(wire, toWire(c, repo))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire)
}

// wireComment mirrors the GitHub issue-comment response object.
type wireComment struct {
	ID        int64     `json:"id"`
	IssueURL  string    `json:"issue_url"`
	User      wireUser  `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
}

type wireUser struct {
	Login string `json:"login"`
}

func toWire(c IssueComment, repo string) wireComment {
	return wireComment{
		ID:        c.ID,
		IssueURL:  fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", repo, c.IssueNumber),
		User:      wireUser{Login: c.Author},
		Body:      c.Body,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
		HTMLURL:   c.URL,
	}
}
