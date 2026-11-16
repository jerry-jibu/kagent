package delivery

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The PR title truncation must be rune-bound (PR #11 review, Nit): a byte
// slice at 71 lands mid-rune in CJK text and corrupts the character into
// U+FFFD, which is exactly what a requester sees in their PR title.
func TestTruncateTitle(t *testing.T) {
	t.Run("Given a CJK line of exactly 72 runes, when the PR title is truncated, assert that it passes through whole — the boundary does not cut", func(t *testing.T) {
		in := strings.Repeat("修", 72) // 144 bytes, 72 runes
		if got := truncateTitle(in); got != in {
			t.Fatalf("truncateTitle cut a 72-rune line: got %d runes, want it unchanged", len([]rune(got)))
		}
	})

	t.Run("Given a CJK line of 73 runes, when the PR title is truncated, assert that the result is 72 valid runes ending with the ellipsis — no character is corrupted", func(t *testing.T) {
		in := strings.Repeat("修", 72) + "了" // 73 runes, 147 bytes
		got := truncateTitle(in)

		runes := []rune(got)
		if len(runes) != 72 {
			t.Fatalf("truncated title = %d runes, want 72 including the ellipsis", len(runes))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncated title is not valid UTF-8: %q", got)
		}
		if runes[len(runes)-1] != '…' {
			t.Fatalf("truncated title ends with %q, want the ellipsis", runes[len(runes)-1])
		}
		if strings.HasPrefix(in, string(runes[:len(runes)-1])) {
			return // the kept runes are the line's own prefix — nothing corrupted
		}
		t.Fatalf("truncated title corrupted the line: %q is not a prefix-preserving cut of %q", got, in)
	})

	t.Run("Given an ASCII line under the bound, when the PR title is truncated, assert that it passes through unchanged", func(t *testing.T) {
		in := "rewrote the parser"
		if got := truncateTitle(in); got != in {
			t.Fatalf("truncateTitle = %q, want the line unchanged", got)
		}
	})
}
