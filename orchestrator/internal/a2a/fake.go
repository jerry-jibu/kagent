package a2a

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Transport for tests: responses are scripted per agent
// and served FIFO; invocations are recorded verbatim so tests assert on the
// messages the orchestrator actually sent.
type Fake struct {
	mu        sync.Mutex
	scripted  map[string][]string
	failures  map[string]error
	hanging   map[string]bool
	calls     []Call
	closeCh   chan struct{}
	closeOnce sync.Once
}

// Call is one recorded agent invocation.
type Call struct {
	Agent   string
	Message string
}

// NewFake returns a ready Fake.
func NewFake() *Fake {
	return &Fake{
		scripted: map[string][]string{},
		failures: map[string]error{},
		hanging:  map[string]bool{},
		closeCh:  make(chan struct{}),
	}
}

// Close releases the fake. It satisfies no Transport requirement; it exists
// so tests can use `defer f.Close()` uniformly with the network-backed fakes.
func (f *Fake) Close() {
	f.closeOnce.Do(func() { close(f.closeCh) })
}

// Script queues responses for the agent; each InvokeAgent consumes one in
// order. Exhausting the queue makes further invocations fail loudly instead
// of inventing responses.
func (f *Fake) Script(agent string, responses ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripted[agent] = append(f.scripted[agent], responses...)
}

// Fail makes every invocation of the agent return err (e.g. an unreachable
// evaluator).
func (f *Fake) Fail(agent string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[agent] = err
}

// Hang makes every invocation of the agent block until the caller's context
// is done — the timeout path of the failure semantics.
func (f *Fake) Hang(agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hanging[agent] = true
}

// Calls returns the recorded invocations in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallCount returns how many times the agent was invoked.
func (f *Fake) CallCount(agent string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Agent == agent {
			n++
		}
	}
	return n
}

// InvokeAgent serves the scripted behavior.
func (f *Fake) InvokeAgent(ctx context.Context, agent, message string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{Agent: agent, Message: message})
	if err := f.failures[agent]; err != nil {
		f.mu.Unlock()
		return "", err
	}
	if f.hanging[agent] {
		f.mu.Unlock()
		<-ctx.Done()
		return "", ctx.Err()
	}
	pending := f.scripted[agent]
	f.mu.Unlock()

	if len(pending) == 0 {
		return "", fmt.Errorf("no scripted response for agent %q", agent)
	}

	f.mu.Lock()
	response := f.scripted[agent][0]
	f.scripted[agent] = f.scripted[agent][1:]
	f.mu.Unlock()
	return response, nil
}
