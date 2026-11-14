package intake_test

import (
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
)

func TestDecodeCommentPayload(t *testing.T) {
	t.Run("Given a trigger payload exactly as Intake persisted it, when decoded, assert that it round-trips into the CommentPayload contract", func(t *testing.T) {
		raw := `{"kind":"issue-comment","repo":"acme/api","issue":42,"comment_id":1001,` +
			`"author":"alice","body":"@acme-bot fix","url":"https://github.com/acme/api/issues/42#issuecomment-1001",` +
			`"created_at":"2026-11-14T10:00:00Z"}`

		pl, err := intake.DecodeCommentPayload([]byte(raw))
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		if pl.Kind != "issue-comment" || pl.Repo != "acme/api" || pl.Issue != 42 ||
			pl.CommentID != 1001 || pl.Author != "alice" || pl.Body != "@acme-bot fix" {
			t.Fatalf("decoded = %+v", pl)
		}
	})

	t.Run("Given a payload that is not valid JSON, when decoded, assert that the decode fails", func(t *testing.T) {
		if _, err := intake.DecodeCommentPayload([]byte(`{`)); err == nil {
			t.Fatalf("invalid JSON must fail")
		}
	})

	scenarios := []struct {
		name    string
		payload string
		want    string
	}{
		{"a kind-less payload", `{"repo":"acme/api","author":"alice"}`, "kind"},
		{"a repo-less payload", `{"kind":"issue-comment","author":"alice"}`, "repo"},
		{"an author-less payload", `{"kind":"issue-comment","repo":"acme/api"}`, "author"},
	}
	for _, sc := range scenarios {
		t.Run("Given "+sc.name+", when decoded, assert that the contract violation is named", func(t *testing.T) {
			_, err := intake.DecodeCommentPayload([]byte(sc.payload))
			if err == nil {
				t.Fatalf("payload %s must fail the contract decode", sc.payload)
			}
			if !strings.Contains(err.Error(), sc.want) {
				t.Fatalf("error %q must name %q", err, sc.want)
			}
		})
	}
}
