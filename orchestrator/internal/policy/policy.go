// Package policy loads the orchestrator's gating policy from a ConfigMap
// mount: repository allowlist, sender admission table, and bot list.
//
// The policy content is hashed into a canonical SHA256 version. Every Gate
// decision and Mandate references that hash (docs/designs/0001 §5/§7), so
// the hash is computed from the parsed, sorted content — whitespace and
// ordering noise do not create phantom versions.
//
// Failure semantics (T1 AC): a broken policy at startup must refuse the
// start (Load returns an error and main exits); a broken policy during a
// hot reload keeps the last good version (see Watcher).
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Admission strengths a sender can hold in the admission table
// (docs/designs/0001 Gate: explicit/inferred/拒).
const (
	StrengthExplicit = "explicit"
	StrengthInferred = "inferred"
	StrengthDenied   = "denied"
)

// AdmissionRule maps a sender identity to the strength the Gate will judge.
type AdmissionRule struct {
	ID       string `json:"id"`
	Strength string `json:"strength"`
}

// Policy is the full gating configuration plus its content hash.
type Policy struct {
	Repositories []string        `json:"repositories"`
	Admission    []AdmissionRule `json:"admission"`
	Bots         []string        `json:"bots"`
	// Version is the SHA256 hash of the canonical policy content.
	Version string `json:"version"`
}

// canonical is the hash input: the policy without its Version field.
type canonical struct {
	Repositories []string        `json:"repositories"`
	Admission    []AdmissionRule `json:"admission"`
	Bots         []string        `json:"bots"`
}

// policyFiles are the required policy files in a ConfigMap mount.
var policyFiles = []string{"repos.json", "admission.json", "bots.json"}

// readFile is the file-read seam the quiescence loop goes through:
// os.ReadFile in production, a flipping fake in the torn-read test.
type readFile func(path string) ([]byte, error)

// tornReadAttempts bounds the quiescence re-check and tornReadBackoff is the
// tiny pause between re-reads: a kubelet swaps ConfigMap files one at a time,
// so a single sequential read can observe a torn mix of two versions for one
// poll interval (PR #11 review). Two consecutive identical reads — bounded to
// a few attempts — are the cheapest signal that the swap has settled.
const (
	tornReadAttempts = 3
	tornReadBackoff  = 10 * time.Millisecond
)

// Load reads and validates the policy directory. It is called at startup
// (error ⇒ refuse to start) and by the Watcher on every detected change.
// The read is quiescence-checked (readStable): proceeding on torn content
// would judge and hash a policy nobody ever wrote.
func Load(dir string) (Policy, error) {
	files, err := readStable(os.ReadFile, dir)
	if err != nil {
		return Policy{}, err
	}
	return parsePolicy(files)
}

// parsePolicy is the pure half of Load: decode the raw policy-file contents,
// validate, hash. It never touches the filesystem.
func parsePolicy(files map[string][]byte) (Policy, error) {
	var p Policy
	for _, name := range policyFiles {
		raw, ok := files[name]
		if !ok {
			return Policy{}, fmt.Errorf("policy file %s is missing", name)
		}
		switch name {
		case "repos.json":
			var rules struct {
				Repositories []string `json:"repositories"`
			}
			if err := json.Unmarshal(raw, &rules); err != nil {
				return Policy{}, fmt.Errorf("policy file %s is not valid JSON: %w", name, err)
			}
			p.Repositories = rules.Repositories
		case "admission.json":
			var rules struct {
				Senders []AdmissionRule `json:"senders"`
			}
			if err := json.Unmarshal(raw, &rules); err != nil {
				return Policy{}, fmt.Errorf("policy file %s is not valid JSON: %w", name, err)
			}
			p.Admission = rules.Senders
		case "bots.json":
			var rules struct {
				Bots []string `json:"bots"`
			}
			if err := json.Unmarshal(raw, &rules); err != nil {
				return Policy{}, fmt.Errorf("policy file %s is not valid JSON: %w", name, err)
			}
			p.Bots = rules.Bots
		}
	}

	if err := p.validate(); err != nil {
		return Policy{}, err
	}
	p.Version = p.Hash()
	return p, nil
}

// readStable reads every policy file and re-reads until two consecutive
// reads agree byte for byte, bounded by tornReadAttempts. read is a seam so
// a test can flip content between reads.
func readStable(read readFile, dir string) (map[string][]byte, error) {
	var prev map[string][]byte
	for attempt := 1; attempt <= tornReadAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(tornReadBackoff)
		}
		files, err := readPolicyFiles(read, dir)
		if err != nil {
			return nil, err
		}
		if sameFiles(prev, files) {
			return files, nil
		}
		prev = files
	}
	return nil, fmt.Errorf("policy files in %s are still changing after %d reads; a ConfigMap swap is likely in progress", dir, tornReadAttempts)
}

// readPolicyFiles reads the raw bytes of every required policy file in dir.
func readPolicyFiles(read readFile, dir string) (map[string][]byte, error) {
	files := make(map[string][]byte, len(policyFiles))
	for _, name := range policyFiles {
		raw, err := read(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("policy file %s is missing: %w", name, err)
			}
			return nil, fmt.Errorf("failed to read policy file %s: %w", name, err)
		}
		files[name] = raw
	}
	return files, nil
}

// sameFiles reports whether two raw file sets are byte-identical.
func sameFiles(a, b map[string][]byte) bool {
	if a == nil || b == nil || len(a) != len(b) {
		return false
	}
	for name, raw := range a {
		if !bytes.Equal(raw, b[name]) {
			return false
		}
	}
	return true
}

// validate rejects policy content the Gate could not judge deterministically.
func (p Policy) validate() error {
	for _, repo := range p.Repositories {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" {
			return fmt.Errorf("invalid repository entry %q: must be owner/name", repo)
		}
	}
	seen := make(map[string]bool, len(p.Admission))
	for _, rule := range p.Admission {
		switch rule.Strength {
		case StrengthExplicit, StrengthInferred, StrengthDenied:
		default:
			return fmt.Errorf("invalid admission strength %q for sender %q: must be explicit, inferred or denied", rule.Strength, rule.ID)
		}
		if rule.ID == "" {
			return fmt.Errorf("admission rule with empty sender id")
		}
		if seen[rule.ID] {
			// StrengthFor resolves by file order and Canonical sorts by id:
			// a duplicate id would make the judged strength order-dependent
			// while the hash hides which entry won (PR #11 review, nit).
			return fmt.Errorf("duplicate admission rule id %q: a sender must hold exactly one strength", rule.ID)
		}
		seen[rule.ID] = true
	}
	return nil
}

// Hash computes the canonical content hash: parsed content, sorted, re-marshaled.
func (p Policy) Hash() string {
	raw := p.Canonical()
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Canonical returns the sorted, re-marshaled policy content. Its SHA256 is
// exactly the Version hash, so the content persisted in the Store is the
// verbatim hash input.
func (p Policy) Canonical() []byte {
	c := canonical{
		Repositories: append([]string(nil), p.Repositories...),
		Admission:    append([]AdmissionRule(nil), p.Admission...),
		Bots:         append([]string(nil), p.Bots...),
	}
	sort.Strings(c.Repositories)
	sort.Slice(c.Admission, func(i, j int) bool { return c.Admission[i].ID < c.Admission[j].ID })
	sort.Strings(c.Bots)

	raw, err := json.Marshal(c)
	if err != nil {
		// Marshal of plain strings/structs cannot fail; unreachable guard.
		panic(fmt.Sprintf("policy canonical marshal failed: %v", err))
	}
	return raw
}

// StrengthFor returns the admission strength of a sender, or StrengthDenied
// when the sender is not in the admission table (deny by default — the Gate
// must judge deterministically for every sender).
func (p Policy) StrengthFor(senderID string) string {
	for _, rule := range p.Admission {
		if rule.ID == senderID {
			return rule.Strength
		}
	}
	return StrengthDenied
}

// IsBot reports whether a sender login is a bot whose comments must not
// count as human triggers (docs/designs/0001: bot 名单).
func (p Policy) IsBot(login string) bool {
	for _, b := range p.Bots {
		if b == login {
			return true
		}
	}
	return false
}
