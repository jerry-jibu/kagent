package a2a

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTPTransport is the production Transport: it speaks JSON-RPC 2.0 to the
// kagent A2A gateway (go/core/internal/a2agateway; wire contract of
// a2a-go v2 — method "SendMessage", roles ROLE_USER/ROLE_AGENT, flattened
// parts). The orchestrator is kagent's caller, with the same standing as the
// CLI and UI (docs/designs/0001 §5).
type HTTPTransport struct {
	base   string
	client *http.Client
}

// HTTPConfig configures the production transport.
type HTTPConfig struct {
	// BaseURL is the gateway origin, e.g. "http://kagent-api.kagent.svc:8083".
	BaseURL string
	// Client overrides the HTTP client (timeouts, transports). Nil defaults
	// to a client whose timeout the invoke's own ctx deadline should
	// outpace; 90s covers DefaultInvokeTimeout plus transport slack.
	Client *http.Client
}

// NewHTTPTransport returns the production transport dialing the gateway.
func NewHTTPTransport(cfg HTTPConfig) *HTTPTransport {
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	return &HTTPTransport{
		base:   strings.TrimSuffix(cfg.BaseURL, "/"),
		client: client,
	}
}

// protocolVersion is the A2A protocol version this transport speaks, sent on
// every request as the A2A-Version service parameter header (a2a-go
// a2a.SvcParamVersion). The gateway serves "1.0" and rejects requests that
// omit the header (it negotiates against the legacy default "0.3").
const protocolVersion = "1.0"

// agentRoute resolves the gateway route of an agent: "namespace/name" as-is,
// a bare name under the default "kagent" namespace. The gateway binds the
// tenant from the URL path (go/core/internal/a2agateway http.go).
func agentRoute(agent string) (string, error) {
	ns, name, found := strings.Cut(agent, "/")
	if !found {
		ns, name = "kagent", agent
	}
	if ns == "" || name == "" {
		return "", fmt.Errorf("agent route %q must be name or namespace/name", agent)
	}
	return ns + "/" + name, nil
}

// InvokeAgent sends one non-streaming SendMessage to the agent and returns
// the final response text: the agent message when the gateway answers with
// one, otherwise the completed task's last agent turn (artifacts as
// fallback). It blocks until a terminal answer or ctx is done; every failure
// surfaces as an error the caller turns into a retryable outcome.
func (t *HTTPTransport) InvokeAgent(ctx context.Context, agent, message string) (string, error) {
	route, err := agentRoute(agent)
	if err != nil {
		return "", err
	}

	result, err := t.rpc(ctx, route, "SendMessage", sendMessageParams{
		Message: wireMessage{
			Role:      "ROLE_USER",
			Parts:     []wirePart{{Text: message}},
			MessageID: newMessageID(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("invoke of agent %s failed: %w", agent, err)
	}

	text, err := extractResponseText(result)
	if err != nil {
		return "", fmt.Errorf("agent %s response carries no readable answer: %w", agent, err)
	}
	return text, nil
}

// Dispatch sends ticket work as a ReturnImmediately SendMessage with the
// orchestrator's push receiver registered (docs/designs/0001 D2) and returns
// the task identity the gateway minted. A response that carries no Task
// (e.g. a bare Message) is refused: a dispatch without a task identity would
// strand the assignment with nothing to reconcile against.
func (t *HTTPTransport) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	route, err := agentRoute(req.Agent)
	if err != nil {
		return DispatchResult{}, err
	}

	params := sendMessageParams{
		Message: wireMessage{
			Role:      "ROLE_USER",
			Parts:     []wirePart{{Text: req.Message}},
			MessageID: newMessageID(),
			ContextID: req.ContextID,
		},
		Config: &sendMessageConfig{
			ReturnImmediately: true,
			PushConfig:        pushConfigFor(req, route),
		},
	}

	result, err := t.rpc(ctx, route, "SendMessage", params)
	if err != nil {
		return DispatchResult{}, err
	}

	task, err := taskFromResult(result)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("agent %s dispatch returned no task identity: %w", req.Agent, err)
	}
	return DispatchResult{
		TaskID:    task.ID,
		ContextID: task.ContextID,
		State:     task.Status.State,
	}, nil
}

// TaskState fetches one task's current snapshot (docs/designs/0001 §4.3: the
// Pass beat's reconcile source of truth). The gateway's task-not-found error
// surfaces as ErrTaskNotFound — the red-line trigger, distinct from a
// transport failure, which stays a plain error.
func (t *HTTPTransport) TaskState(ctx context.Context, agent, taskID string) (Task, error) {
	route, err := agentRoute(agent)
	if err != nil {
		return Task{}, err
	}

	result, err := t.rpc(ctx, route, "GetTask", getTaskParams{ID: taskID})
	if err != nil {
		return Task{}, err
	}

	task, err := taskFromResult(result)
	if err != nil {
		return Task{}, fmt.Errorf("task %s snapshot is not an A2A Task: %w", taskID, err)
	}
	return Task{
		ID:         task.ID,
		ContextID:  task.ContextID,
		State:      task.Status.State,
		StatusText: task.Status.Message.text(),
	}, nil
}

// rpc sends one JSON-RPC request to the agent route and returns the result
// envelope. JSON-RPC errors surface with their code: -32001 maps to
// ErrTaskNotFound, everything else stays a plain error.
func (t *HTTPTransport) rpc(ctx context.Context, route, method string, params any) (json.RawMessage, error) {
	payload, err := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		ID:      newMessageID(), // the JSON-RPC request id, minted like the A2A message id
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode the %s request: %w", method, err)
	}

	endpoint := t.base + "/agents/" + route
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to build the request for %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", protocolVersion)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s of agent %s failed: %w", method, route, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s of agent %s failed: HTTP %d", method, route, resp.StatusCode)
	}

	var wire jsonrpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("failed to decode the response of agent %s: %w", route, err)
	}
	if wire.Error != nil {
		if wire.Error.Code == codeTaskNotFound {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("agent %s answered a JSON-RPC error %d: %s", route, wire.Error.Code, wire.Error.Message)
	}
	if len(wire.Result) == 0 {
		return nil, fmt.Errorf("agent %s answered without a result", route)
	}
	return wire.Result, nil
}

// pushConfigFor builds the push registration for a dispatch. Tenant is the
// agent route — the gateway refuses a mismatched tenant
// (go/core/internal/a2agateway push validation).
func pushConfigFor(req DispatchRequest, route string) *pushConfig {
	if req.PushURL == "" {
		return nil
	}
	return &pushConfig{
		URL:    req.PushURL,
		ID:     req.PushID,
		Tenant: route,
	}
}

// taskFromResult unwraps the gateway's result envelope: a Task-carrying
// result nests the task under "task" ({"task": {...}}, the a2a-go
// SendMessageResponse/GetTaskResponse shape) — the flat task was an imagined
// shape the real gateway never sends.
func taskFromResult(result json.RawMessage) (wireTask, error) {
	var wrapper struct {
		Task *wireTask `json:"task,omitempty"`
	}
	var task wireTask
	if err := json.Unmarshal(result, &wrapper); err != nil {
		return task, fmt.Errorf("result is not an A2A Task envelope: %w", err)
	}
	if wrapper.Task == nil || wrapper.Task.ID == "" {
		return task, fmt.Errorf("result carries no task")
	}
	return *wrapper.Task, nil
}

// extractResponseText pulls the final answer text out of a SendMessage
// result (docs/designs/0004 §2: the non-streaming send returns the final
// Message or the terminal Task).
func extractResponseText(result json.RawMessage) (string, error) {
	var wrapper struct {
		Message *wireMessage `json:"message,omitempty"`
		Task    *wireTask    `json:"task,omitempty"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil {
		return "", fmt.Errorf("result is not an A2A Message or Task: %w", err)
	}
	switch {
	case wrapper.Message != nil:
		return wrapper.Message.text(), nil
	case wrapper.Task != nil:
		return wrapper.Task.text(), nil
	default:
		return "", fmt.Errorf("result is neither a Message nor a Task")
	}
}

// jsonrpcRequest is the JSON-RPC 2.0 client envelope.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	ID      string `json:"id"`
	Params  any    `json:"params"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// codeTaskNotFound is the JSON-RPC error code the A2A gateway answers a
// GetTask for an unknown task with (a2a-go internal/jsonrpc codeToError).
const codeTaskNotFound = -32001

// sendMessageParams carries the SendMessageRequest's message (a2a-go
// SendMessageRequest: tenant/configuration/metadata optional, message
// required).
type sendMessageParams struct {
	Message wireMessage        `json:"message"`
	Config  *sendMessageConfig `json:"configuration,omitempty"`
}

// sendMessageConfig mirrors a2a-go's SendMessageConfig wire shape for the
// fields the dispatcher uses (docs/designs/0004 §2 three-mode table).
type sendMessageConfig struct {
	ReturnImmediately bool        `json:"returnImmediately,omitempty"`
	PushConfig        *pushConfig `json:"taskPushNotificationConfig,omitempty"`
}

// pushConfig mirrors a2a-go's PushConfig wire shape.
type pushConfig struct {
	URL    string `json:"url"`
	ID     string `json:"id,omitempty"`
	Tenant string `json:"tenant,omitempty"`
}

// getTaskParams mirrors a2a-go's GetTaskRequest wire shape.
type getTaskParams struct {
	ID string `json:"id"`
}

// wireMessage mirrors a2a-go's Message wire shape.
type wireMessage struct {
	Role      string     `json:"role"`
	Parts     []wirePart `json:"parts"`
	MessageID string     `json:"messageId"`
	TaskID    string     `json:"taskId,omitempty"`
	ContextID string     `json:"contextId,omitempty"`
}

func (m wireMessage) text() string {
	var sb strings.Builder
	for _, p := range m.Parts {
		sb.WriteString(p.text())
	}
	return sb.String()
}

// wirePart mirrors a2a-go's flattened Part wire shape: a text part serializes
// as {"text": "..."}; a structured-output part (kagent harness outputSchema)
// serializes as {"data": {...}, "mediaType": "application/json"}.
type wirePart struct {
	Text      string         `json:"text,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	MediaType string         `json:"mediaType,omitempty"`
}

// text is the part's text content. A structured part's text is its JSON
// encoding: the evaluator's Decision travels as data, and the parse contract
// expects a JSON object.
func (p wirePart) text() string {
	if p.Text != "" {
		return p.Text
	}
	if len(p.Data) > 0 {
		if raw, err := json.Marshal(p.Data); err == nil {
			return string(raw)
		}
	}
	return ""
}

// wireTask mirrors the Task fields the response extraction needs.
type wireTask struct {
	ID        string `json:"id"`
	ContextID string `json:"contextId"`
	Status    struct {
		State   string       `json:"state"`
		Message *wireMessage `json:"message,omitempty"`
	} `json:"status"`
	History   []wireMessage `json:"history,omitempty"`
	Artifacts []struct {
		Name  string     `json:"name,omitempty"`
		Parts []wirePart `json:"parts"`
	} `json:"artifacts,omitempty"`
}

// text returns the task's final answer: the terminal status message when the
// gateway attached one, else the last agent turn in history, else the
// artifacts. The order matches where a completed kagent task actually puts
// its answer.
func (t wireTask) text() string {
	if t.Status.Message != nil {
		if s := t.Status.Message.text(); s != "" {
			return s
		}
	}
	for i := len(t.History) - 1; i >= 0; i-- {
		if t.History[i].Role == "ROLE_AGENT" {
			if s := t.History[i].text(); s != "" {
				return s
			}
		}
	}
	for _, a := range t.Artifacts {
		var sb strings.Builder
		for _, p := range a.Parts {
			sb.WriteString(p.text())
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}

// newMessageID mints a random A2A message id.
func newMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; a timestamp id keeps the
		// request representable.
		return fmt.Sprintf("orch-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
