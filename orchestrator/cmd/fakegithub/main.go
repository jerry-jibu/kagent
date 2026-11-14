// Command fakegithub serves the orchestrator's GitHub fake as a standalone
// process for e2e runs (V1 scripted walkthrough, docs/designs/0003): it
// seeds one trigger comment at startup, prints the fake's base URL, and
// logs every posted comment and PR so the e2e script can grep the chain
// evidence. It is a test tool only — the production face is api.github.com.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
)

func main() {
	repo := flag.String("repo", "acme/api", "seed repository (owner/name)")
	issue := flag.Int("issue", 7, "seed issue number")
	author := flag.String("author", "maintainer-login", "seed comment author")
	body := flag.String("body", "@agent please fix the greeting", "seed comment body")
	commentID := flag.Int64("comment-id", 900001, "seed comment id (the deterministic trigger id)")
	expectToken := flag.String("expect-token", "", "bearer token the orchestrator must present (logged once seen)")
	flag.Parse()

	f := github.NewFake()
	now := time.Now().UTC()
	f.AddComment(github.IssueComment{
		ID:          *commentID,
		IssueNumber: *issue,
		Author:      *author,
		Body:        *body,
		CreatedAt:   now,
		UpdatedAt:   now,
		URL:         fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", *repo, *issue, *commentID),
	})

	fmt.Printf("FAKE_GITHUB_URL=%s\n", f.URL())
	fmt.Printf("FAKE_SEED=repo:%s issue:%d author:%s comment:%d\n", *repo, *issue, *author, *commentID)
	os.Stdout.Sync()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	seen := map[string]bool{}
	for range time.Tick(500 * time.Millisecond) {
		for _, c := range f.PostedComments() {
			key := fmt.Sprintf("comment:%d", c.ID)
			if !seen[key] {
				seen[key] = true
				logger.Info("posted comment", "issue", c.IssueNumber, "id", c.ID,
					"marker", effectMarkerOf(c.Body), "body_prefix", firstLine(c.Body))
			}
		}
		for _, pr := range f.PullRequests() {
			key := fmt.Sprintf("pr:%d", pr.Number)
			if !seen[key] {
				seen[key] = true
				logger.Info("pull request", "number", pr.Number, "head", pr.Head,
					"base", pr.Base, "url", pr.URL)
			}
		}
		if *expectToken != "" && f.LastAuthorization() != "" && !seen["auth"] {
			seen["auth"] = true
			logger.Info("last authorization", "value", f.LastAuthorization(),
				"matches", f.LastAuthorization() == "Bearer "+*expectToken)
		}
	}
}

// effectMarkerOf extracts the deterministic effect marker the orchestrator
// embeds in every comment it posts (docs/designs/0001 §6).
func effectMarkerOf(body string) string {
	const open = "<!-- superagent:effect:"
	if at := strings.Index(body, open); at >= 0 {
		if end := strings.Index(body[at:], " -->"); end > 0 {
			return body[at : at+end+4]
		}
	}
	return ""
}

func firstLine(s string) string {
	if at := strings.IndexByte(s, '\n'); at >= 0 {
		return s[:at]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
