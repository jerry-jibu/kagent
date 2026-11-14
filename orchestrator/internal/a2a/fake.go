package a2a

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Transport for tests: responses are scripted per agent
// (and per task id for status queries) and served FIFO; invocations are
// recorded verbatim so tests assert on the messages the orchestrator
// actually sent.
type Fake struct {
	mu               sync.Mutex
	scripted         map[string][]string
	failures         map[string]error
	hanging          map[string]bool
	calls            []Call
	dispatchScript   map[string][]DispatchResult
	dispatchFailures map[string]error
	dispatchCalls    []DispatchCall
	taskScript       map[string][]Task
	taskFailures     map[string]error
	taskCalls        []TaskStateCall
	closeCh          chan struct{}
	closeOnce        sync.Once
}

// Call is one recorded agent invocation.
type Call struct {
	Agent   string
	Message string
}

// NewFake returns a ready Fake.
func NewFake() *Fake {
	return &Fake{
		scripted:         map[string][]string{},
		failures:         map[string]error{},
		hanging:          map[string]bool{},
		dispatchScript:   map[string][]DispatchResult{},
		dispatchFailures: map[string]error{},
		taskScript:       map[string][]Task{},
		taskFailures:     map[string]error{},
		closeCh:          make(chan struct{}),
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

// DispatchCall is one recorded worker dispatch, verbatim.
type DispatchCall struct {
	Agent     string
	Message   string
	ContextID string
	PushURL   string
	PushID    string
}

// TaskStateCall is one recorded task status query.
type TaskStateCall struct {
	Agent  string
	TaskID string
}

// ScriptDispatch queues DispatchResults for the agent; each Dispatch
// consumes one in order. Exhausting the queue fails loudly.
func (f *Fake) ScriptDispatch(agent string, results ...DispatchResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatchScript[agent] = append(f.dispatchScript[agent], results...)
}

// FailDispatch makes every Dispatch of the agent return err — the
// unresponsive-gateway/worker path. A nil err clears the failure.
func (f *Fake) FailDispatch(agent string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.dispatchFailures, agent)
		return
	}
	f.dispatchFailures[agent] = err
}

// DispatchCalls returns the recorded dispatches in order.
func (f *Fake) DispatchCalls() []DispatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DispatchCall(nil), f.dispatchCalls...)
}

// ScriptTaskState queues task snapshots for the task id; each TaskState
// query consumes one in order. Exhausting the queue fails loudly.
func (f *Fake) ScriptTaskState(taskID string, tasks ...Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskScript[taskID] = append(f.taskScript[taskID], tasks...)
}

// FailTaskState makes every TaskState query for the task id return err
// (ErrTaskNotFound exercises the red-line path).
func (f *Fake) FailTaskState(taskID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskFailures[taskID] = err
}

// TaskStateCalls returns the recorded task queries in order.
func (f *Fake) TaskStateCalls() []TaskStateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TaskStateCall(nil), f.taskCalls...)
}

// Dispatch serves the scripted dispatch behavior.
func (f *Fake) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	f.mu.Lock()
	f.dispatchCalls = append(f.dispatchCalls, DispatchCall{
		Agent:     req.Agent,
		Message:   req.Message,
		ContextID: req.ContextID,
		PushURL:   req.PushURL,
		PushID:    req.PushID,
	})
	if err := f.dispatchFailures[req.Agent]; err != nil {
		f.mu.Unlock()
		return DispatchResult{}, err
	}
	pending := f.dispatchScript[req.Agent]
	f.mu.Unlock()

	if len(pending) == 0 {
		return DispatchResult{}, fmt.Errorf("no scripted dispatch result for agent %q", req.Agent)
	}

	f.mu.Lock()
	result := f.dispatchScript[req.Agent][0]
	f.dispatchScript[req.Agent] = f.dispatchScript[req.Agent][1:]
	f.mu.Unlock()
	return result, nil
}

// TaskState serves the scripted task snapshots.
func (f *Fake) TaskState(ctx context.Context, agent, taskID string) (Task, error) {
	f.mu.Lock()
	f.taskCalls = append(f.taskCalls, TaskStateCall{Agent: agent, TaskID: taskID})
	if err := f.taskFailures[taskID]; err != nil {
		f.mu.Unlock()
		return Task{}, err
	}
	pending := f.taskScript[taskID]
	f.mu.Unlock()

	if len(pending) == 0 {
		return Task{}, fmt.Errorf("no scripted task snapshot for %q", taskID)
	}

	f.mu.Lock()
	task := f.taskScript[taskID][0]
	f.taskScript[taskID] = f.taskScript[taskID][1:]
	f.mu.Unlock()
	return task, nil
}
