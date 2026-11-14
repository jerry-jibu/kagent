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

	payload, err := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  "SendMessage",
		ID:      invokeID(),
		Params: sendMessageParams{
			Message: wireMessage{
				Role:      "ROLE_USER",
				Parts:     []wirePart{{Text: message}},
				MessageID: newMessageID(),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to encode the SendMessage request: %w", err)
	}

	endpoint := t.base + "/agents/" + route
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("failed to build the request for %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("invoke of agent %s failed: %w", agent, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("invoke of agent %s failed: HTTP %d", agent, resp.StatusCode)
	}

	var wire jsonrpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return "", fmt.Errorf("failed to decode the response of agent %s: %w", agent, err)
	}
	if wire.Error != nil {
		return "", fmt.Errorf("agent %s answered a JSON-RPC error %d: %s", agent, wire.Error.Code, wire.Error.Message)
	}
	if len(wire.Result) == 0 {
		return "", fmt.Errorf("agent %s answered without a result", agent)
	}

	text, err := extractResponseText(wire.Result)
	if err != nil {
		return "", fmt.Errorf("agent %s response carries no readable answer: %w", agent, err)
	}
	return text, nil
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
	JSONRPC string            `json:"jsonrpc"`
	Method  string            `json:"method"`
	ID      string            `json:"id"`
	Params  sendMessageParams `json:"params"`
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

// sendMessageParams carries the SendMessageRequest's message (a2a-go
// SendMessageRequest: tenant/configuration/metadata optional, message
// required).
type sendMessageParams struct {
	Message wireMessage `json:"message"`
}

// wireMessage mirrors a2a-go's Message wire shape.
type wireMessage struct {
	Role      string     `json:"role"`
	Parts     []wirePart `json:"parts"`
	MessageID string     `json:"messageId"`
	TaskID    string     `json:"taskId,omitempty"`
}

func (m wireMessage) text() string {
	var sb strings.Builder
	for _, p := range m.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// wirePart mirrors a2a-go's flattened Part wire shape: a text part serializes
// as {"text": "..."}.
type wirePart struct {
	Text string `json:"text,omitempty"`
}

// wireTask mirrors the Task fields the response extraction needs.
type wireTask struct {
	ID     string `json:"id"`
	Status struct {
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
			sb.WriteString(p.Text)
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

// invokeID mints the JSON-RPC request id.
func invokeID() string {
	return newMessageID()
}
