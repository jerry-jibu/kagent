package workspace_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

func TestMCPClient(t *testing.T) {
	t.Run("Given a fake agentfs MCP bridge, when the client dials and calls agenticfs_create_workspace, assert that the tool result reports the workspace was created", func(t *testing.T) {
		fake := newFakeAgentfs()
		defer fake.close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := workspace.NewMCPClient(ctx, fake.endpoint())
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		defer client.Close()

		text, err := client.CallTool(ctx, "agenticfs_create_workspace", map[string]any{
			"name":      "ticket-42",
			"intention": "materialize ticket-42 workspace",
		})
		if err != nil {
			t.Fatalf("CallTool failed: %v", err)
		}
		if !strings.Contains(text, `"ticket-42" created`) {
			t.Fatalf("CallTool text = %q, want workspace creation confirmation", text)
		}
	})

	t.Run("Given a fake agentfs MCP bridge, when a tool call fails on the server, assert that the client surfaces the isError text as an error", func(t *testing.T) {
		fake := newFakeAgentfs()
		defer fake.close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := workspace.NewMCPClient(ctx, fake.endpoint())
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		defer client.Close()

		_, err = client.CallTool(ctx, "agenticfs_write_file", map[string]any{
			"workspace": "no-such-workspace",
			"path":      "a/b.txt",
			"content":   "x",
			"intention": "probe",
		})
		if err == nil {
			t.Fatal("CallTool succeeded, want error for unknown workspace")
		}
		if !strings.Contains(err.Error(), "workspace not found") {
			t.Fatalf("error = %v, want the agentfs isError text", err)
		}
	})
}
