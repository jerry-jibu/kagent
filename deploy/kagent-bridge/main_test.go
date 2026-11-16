package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newFakeUpstreamSession spawns a stand-in `agentfs mcp` (a sh script)
// wired exactly like startSession does: own process group, stdin/stdout
// pipes, a buffered line Scanner, and cleanup that kills the whole tree
// (the script's sleep grandchild inherits the stdout pipe — killing only
// sh would leave the pipe open) and reaps the zombie.
func newFakeUpstreamSession(t *testing.T, script string) *session {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("fake upstream stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("fake upstream stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake upstream: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait() // reap so the test binary never accumulates zombies
	})
	s := &session{cmd: cmd, stdin: stdin, stdout: bufio.NewScanner(stdout)}
	s.stdout.Buffer(make([]byte, 64*1024), 64*1024*1024)
	return s
}

func TestUpstreamRequest(t *testing.T) {
	t.Run("Given an upstream that answers the request id, when one request is sent, assert that the response comes back normalized to jsonrpc 2.0", func(t *testing.T) {
		// The engine's real quirk: it may omit the jsonrpc tag — the bridge
		// must normalize (see upstreamRequest).
		s := newFakeUpstreamSession(t, `read line; echo '{"id":1,"result":{"ok":true}}'`)
		out, err := s.upstreamRequest("tools/list", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("upstreamRequest: %v", err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("response is not a JSON-RPC envelope: %v (%s)", err, out)
		}
		if resp.JSONRPC != "2.0" {
			t.Fatalf("jsonrpc = %q, want normalized \"2.0\"", resp.JSONRPC)
		}
		var gotID int64
		if err := json.Unmarshal(resp.ID, &gotID); err != nil || gotID != 1 {
			t.Fatalf("id = %s, want 1 (%v)", resp.ID, err)
		}
	})

	t.Run("Given a silently hung upstream and a shortened deadline, when one request is sent, assert that the watchdog fails the call quickly instead of blocking until the process exits", func(t *testing.T) {
		old := upstreamDeadline
		upstreamDeadline = 300 * time.Millisecond
		t.Cleanup(func() { upstreamDeadline = old })
		// Reads the request (stdin write succeeds) and never answers.
		s := newFakeUpstreamSession(t, `read line; sleep 30`)
		start := time.Now()
		_, err := s.upstreamRequest("tools/call", json.RawMessage(`{}`))
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("upstreamRequest returned nil error, want watchdog timeout")
		}
		if !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("error = %v, want a watchdog timeout error", err)
		}
		// Without the watchdog this call blocks until the fake process is
		// killed at cleanup (30s sleep); with it, the deadline fires fast.
		if elapsed > 10*time.Second {
			t.Fatalf("call returned after %s, want the watchdog (300ms) to unblock it well before the 30s sleep", elapsed)
		}
	})

	t.Run("Given an upstream that exits without answering, when one request is sent, assert that the call fails with the upstream-exit error", func(t *testing.T) {
		s := newFakeUpstreamSession(t, `sleep 0.3; exit 0`)
		_, err := s.upstreamRequest("tools/call", json.RawMessage(`{}`))
		if err == nil {
			t.Fatal("upstreamRequest returned nil error, want upstream-exit failure")
		}
		if !strings.Contains(err.Error(), "exited before answering") {
			t.Fatalf("error = %v, want \"exited before answering\"", err)
		}
	})
}

func TestWithBlankBase(t *testing.T) {
	// buildCall is the tools/call envelope the bridge sees from the
	// orchestrator for a zero-source workspace.
	buildCall := func(name string, sources []string) json.RawMessage {
		args := map[string]any{"name": name}
		if sources != nil {
			args["sources"] = sources
		}
		raw, err := json.Marshal(map[string]any{
			"name":      "agenticfs_create_workspace",
			"arguments": args,
		})
		if err != nil {
			t.Fatalf("marshal call: %v", err)
		}
		return raw
	}

	newFixture := func(t *testing.T) *bridge {
		t.Helper()
		// agentfsBin is irrelevant to withBlankBase; home is the data root
		// whose basesDir must never be escaped.
		return newBridge("unused-agentfs", t.TempDir())
	}

	t.Run("Given a zero-source call with a ticket-style name, when withBlankBase rewrites it, assert that the base lands inside basesDir and the call gains that base as its single source", func(t *testing.T) {
		b := newFixture(t)
		name := "ticket-0123456789ab" // orchestrator ticket-id shape (store.TicketIDForTrigger)
		out := b.withBlankBase(buildCall(name, nil))
		var call struct {
			Arguments struct {
				Name    string   `json:"name"`
				Sources []string `json:"sources"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(out, &call); err != nil {
			t.Fatalf("rewritten params do not parse: %v (%s)", err, out)
		}
		wantBase := filepath.Join(b.basesDir, name)
		if call.Arguments.Name != name {
			t.Fatalf("name = %q, want %q", call.Arguments.Name, name)
		}
		if len(call.Arguments.Sources) != 1 || call.Arguments.Sources[0] != wantBase {
			t.Fatalf("sources = %v, want [%s]", call.Arguments.Sources, wantBase)
		}
		if info, err := os.Stat(wantBase); err != nil || !info.IsDir() {
			t.Fatalf("blank base %s not created as a directory (stat err=%v)", wantBase, err)
		}
	})

	t.Run("Given a traversal name, when withBlankBase rewrites it, assert that params pass through untouched and nothing is created outside the data root", func(t *testing.T) {
		for _, name := range []string{"../escape", "../../escape", "a/b", "..", ".hidden", "ticket..dup", strings.Repeat("x", 129)} {
			// Replace separators in the subtest name: t.Run would otherwise
			// split the sentence into nested subtests.
			display := strings.ReplaceAll(name, "/", "_")
			t.Run("Given the name "+display+", when withBlankBase rewrites the call, assert that params pass through untouched", func(t *testing.T) {
				b := newFixture(t)
				in := buildCall(name, nil)
				out := b.withBlankBase(in)
				if !bytes.Equal(out, in) {
					t.Fatalf("params were rewritten for unsafe name %q: %s", name, out)
				}
				// The data root itself must have no new entries (an escaped
				// "../escape" would materialize as a sibling of basesDir).
				entries, err := os.ReadDir(b.home)
				if err != nil {
					t.Fatalf("read data root: %v", err)
				}
				if len(entries) != 1 || entries[0].Name() != "bases" {
					t.Fatalf("data root = %v, want only [bases]", entries)
				}
				bases, err := os.ReadDir(b.basesDir)
				if err != nil {
					t.Fatalf("read basesDir: %v", err)
				}
				if len(bases) != 0 {
					t.Fatalf("basesDir = %v, want empty for refused names", bases)
				}
			})
		}
	})

	t.Run("Given a call that already carries sources, when withBlankBase rewrites it, assert that caller-provided bases win and params pass through untouched", func(t *testing.T) {
		b := newFixture(t)
		in := buildCall("ticket-fedcba987654", []string{"/tmp/already-there"})
		out := b.withBlankBase(in)
		if !bytes.Equal(out, in) {
			t.Fatalf("params were rewritten despite caller sources: %s", out)
		}
	})

	t.Run("Given a call with an empty name, when withBlankBase rewrites it, assert that params pass through so the engine refuses the missing name", func(t *testing.T) {
		b := newFixture(t)
		in := buildCall("", nil)
		out := b.withBlankBase(in)
		if !bytes.Equal(out, in) {
			t.Fatalf("params were rewritten despite empty name: %s", out)
		}
	})
}
