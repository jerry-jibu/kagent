package intake

import (
	"strings"
)

// baseBranchMarker is the line prefix a requester writes in a comment body
// to point the ticket's work at a branch (Story 27: issue 指向分支). Exact
// lowercase spelling: git branch names are case-sensitive, so the directive
// is too — "Base-branch:" is not the marker.
const baseBranchMarker = "base-branch:"

// BaseBranchFromBody extracts the base branch a trigger's comment body
// points at: the first line whose trimmed form starts with "base-branch:"
// decides, and its value must be a single branch token. Any other shape —
// no marker, an empty value, a value containing whitespace — yields "":
// the caller then applies the deployment-level default
// (--workspace-base-branch). The parse is deterministic and content-shaped;
// the extracted branch is work scoping, not authorization — the Mandate's
// authority still comes from the Gate verdict alone (docs/designs/0001 §7).
func BaseBranchFromBody(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		value, found := strings.CutPrefix(trimmed, baseBranchMarker)
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, " \t") {
			// A marker without a single branch token is not the directive.
			return ""
		}
		return value
	}
	return ""
}
