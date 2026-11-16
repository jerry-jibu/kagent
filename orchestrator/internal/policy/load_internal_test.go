package policy

// The torn-read quiescence seam (PR #11 review): readStable must proceed on
// stable content, keep re-reading through one torn read, and refuse content
// that never settles. read is a seam, so the flipping fake needs no fake
// filesystem harness.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadStable(t *testing.T) {
	t.Run("Given a reader whose files never change between reads, when readStable runs, assert that it returns after exactly two agreeing passes (the consecutive-read comparison)", func(t *testing.T) {
		reads := 0
		files, err := readStable(func(string) ([]byte, error) {
			reads++
			return []byte(`{"bots": []}`), nil
		}, t.TempDir())
		if err != nil {
			t.Fatalf("readStable failed: %v", err)
		}
		if reads != 2*len(policyFiles) {
			t.Fatalf("readStable made %d reads, want two agreeing passes of %d", reads, len(policyFiles))
		}
		if string(files["bots.json"]) != `{"bots": []}` {
			t.Fatalf("bots.json = %q, want the read content", files["bots.json"])
		}
	})

	t.Run("Given a kubelet swap that tears exactly one read (the second file flips mid-read), when readStable runs, assert that it re-reads and settles on the post-swap content", func(t *testing.T) {
		calls := 0
		files, err := readStable(func(path string) ([]byte, error) {
			calls++
			// First pass: repos.json read before the swap, bots.json after.
			if calls <= len(policyFiles) {
				if filepath.Base(path) == "bots.json" {
					return []byte(`{"bots": ["old-bot"]}`), nil
				}
				return []byte(`{"bots": ["new-bot"]}`), nil
			}
			return []byte(`{"bots": ["new-bot"]}`), nil
		}, t.TempDir())
		if err != nil {
			t.Fatalf("readStable failed on a single torn pass: %v", err)
		}
		if string(files["bots.json"]) != `{"bots": ["new-bot"]}` {
			t.Fatalf("bots.json = %q, want the settled post-swap content", files["bots.json"])
		}
	})

	t.Run("Given a reader whose content keeps flipping forever, when readStable runs, assert that it refuses with a still-changing error instead of proceeding on torn content", func(t *testing.T) {
		calls := 0
		_, err := readStable(func(string) ([]byte, error) {
			calls++
			return []byte(fmt.Sprintf(`{"bots": ["bot-%d"]}`, calls)), nil
		}, t.TempDir())
		if err == nil {
			t.Fatal("content that never settles must be refused")
		}
		if !strings.Contains(err.Error(), "still changing") {
			t.Fatalf("error = %v, want the still-changing reason", err)
		}
	})

	t.Run("Given a reader that reports a missing file, when readStable runs, assert that the missing-file error names the file", func(t *testing.T) {
		_, err := readStable(func(string) ([]byte, error) {
			return nil, os.ErrNotExist
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("error = %v, want the missing-file error", err)
		}
	})
}
