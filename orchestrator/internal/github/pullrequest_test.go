package github_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
)

func TestCreatePullRequest(t *testing.T) {
	t.Run("Given a fake GitHub, when a pull request is created for a pushed branch, assert that the returned PR carries its number, HTML URL and head/base branches", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		transport := github.NewTransport(github.TransportConfig{BaseURL: fake.URL(), Token: "tok-1"})

		pr, err := transport.CreatePullRequest(context.Background(), "acme/api", github.PullRequestInput{
			Title: "ticket-abc123: deliver workspace result",
			Head:  "ticket-abc123",
			Base:  "main",
			Body:  "Delivers ticket-abc123 for acme/api#42.",
		})
		if err != nil {
			t.Fatalf("CreatePullRequest failed: %v", err)
		}
		if pr.Number == 0 || pr.URL == "" {
			t.Fatalf("PR identity missing: %+v", pr)
		}
		if pr.Head != "ticket-abc123" || pr.Base != "main" {
			t.Fatalf("PR branches = %s->%s, want ticket-abc123->main", pr.Head, pr.Base)
		}
		if fake.LastAuthorization() != "Bearer tok-1" {
			t.Fatalf("Authorization = %q, want the bearer token", fake.LastAuthorization())
		}
		if !fake.HasPullRequest("acme/api", "ticket-abc123") {
			t.Fatalf("fake GitHub does not hold the created PR")
		}
	})

	t.Run("Given a PR that already exists for the head branch, when CreatePullRequest replays, assert that the existing PR is returned instead of a duplicate — a lost delivery receipt must never open a second PR", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		transport := github.NewTransport(github.TransportConfig{BaseURL: fake.URL(), Token: "tok-1"})
		ctx := context.Background()

		in := github.PullRequestInput{Title: "t", Head: "ticket-replay", Base: "main", Body: "b"}
		first, err := transport.CreatePullRequest(ctx, "acme/api", in)
		if err != nil {
			t.Fatalf("first create failed: %v", err)
		}

		second, err := transport.CreatePullRequest(ctx, "acme/api", in)
		if err != nil {
			t.Fatalf("replay create failed: %v", err)
		}
		if second.Number != first.Number || second.URL != first.URL {
			t.Fatalf("replay returned %+v, want the existing %+v", second, first)
		}
		if fake.OpenPullRequestCount("acme/api", "ticket-replay") != 1 {
			t.Fatalf("replay opened a duplicate PR")
		}
	})

	t.Run("Given a GitHub failure, when CreatePullRequest runs, assert that the error carries the status so the deliverer can retry", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		fake.FailWith(http.StatusBadGateway, "upstream broken")
		transport := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})

		_, err := transport.CreatePullRequest(context.Background(), "acme/api", github.PullRequestInput{
			Title: "t", Head: "h", Base: "main", Body: "b",
		})
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("err = %v, want a 502-carrying error", err)
		}
	})
}
