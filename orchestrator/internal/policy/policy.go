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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// Load reads and validates the policy directory. It is called at startup
// (error ⇒ refuse to start) and by the Watcher on every detected change.
func Load(dir string) (Policy, error) {
	var p Policy
	var rules struct {
		Repositories []string        `json:"repositories"`
		Senders      []AdmissionRule `json:"senders"`
		Bots         []string        `json:"bots"`
	}

	for _, name := range policyFiles {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return Policy{}, fmt.Errorf("policy file %s is missing: %w", name, err)
			}
			return Policy{}, fmt.Errorf("failed to read policy file %s: %w", name, err)
		}
		switch name {
		case "repos.json":
			if err := json.Unmarshal(raw, &rules); err != nil {
				return Policy{}, fmt.Errorf("policy file %s is not valid JSON: %w", name, err)
			}
			p.Repositories = rules.Repositories
		case "admission.json":
			if err := json.Unmarshal(raw, &rules); err != nil {
				return Policy{}, fmt.Errorf("policy file %s is not valid JSON: %w", name, err)
			}
			p.Admission = rules.Senders
		case "bots.json":
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

// validate rejects policy content the Gate could not judge deterministically.
func (p Policy) validate() error {
	for _, repo := range p.Repositories {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" {
			return fmt.Errorf("invalid repository entry %q: must be owner/name", repo)
		}
	}
	for _, rule := range p.Admission {
		switch rule.Strength {
		case StrengthExplicit, StrengthInferred, StrengthDenied:
		default:
			return fmt.Errorf("invalid admission strength %q for sender %q: must be explicit, inferred or denied", rule.Strength, rule.ID)
		}
		if rule.ID == "" {
			return fmt.Errorf("admission rule with empty sender id")
		}
	}
	return nil
}

// Hash computes the canonical content hash: parsed content, sorted, re-marshaled.
func (p Policy) Hash() string {
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
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
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
