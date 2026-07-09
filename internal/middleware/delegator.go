package middleware

import (
	"context"
	"net/http"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// Delegator implements Hierarchical Agentic Routing (Active Torana Tools).
// Injects a torana_delegate_task tool and intercepts calls to it,
// spinning up a sub-agent to execute the task.
type Delegator struct{}

func NewDelegator() *Delegator { return &Delegator{} }
func (d *Delegator) Name() string { return "delegator" }

// BeforeRequest injects the torana_delegate_task tool.
func (d *Delegator) BeforeRequest(ctx context.Context, req *http.Request, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	if chat == nil || len(chat.Tools) == 0 {
		return chat, nil
	}

	// Inject the delegation tool.
	chat.Tools = append(chat.Tools, engine.ToolDef{
		Name:        "torana_delegate_task",
		Description: "Delegate a sub-task to a cheaper model that runs locally. Use this for isolated, self-contained tasks like finding a file, grepping code, or reading docs. The sub-agent will execute the task and return a summary.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "Clear description of what to accomplish — e.g. 'Find all Go files that use sync.Map'",
				},
				"max_turns": map[string]any{
					"type":        "integer",
					"description": "Maximum tool call turns for the sub-agent (default 5)",
				},
			},
			"required":             []any{"task"},
			"additionalProperties": false,
		},
	})

	return chat, nil
}

// AfterResponse passes events through unchanged.
// In v0.3.0 the tool is injected but interception is reserved for
// a future version that will spin up a full sub-agent loop via the
// offload provider and return a proper tool_result.
func (d *Delegator) AfterResponse(ctx context.Context, resp *http.Response, events <-chan engine.StreamEvent, req *http.Request, chat *engine.ChatRequest) (<-chan engine.StreamEvent, error) {
	out := make(chan engine.StreamEvent, 16)
	go func() {
		defer close(out)
		for ev := range events {
			out <- ev
		}
	}()
	return out, nil
}

// executeTask runs a delegated task locally.
// In v0.3.0 this is a simple mock/exec; future versions will use
// the offload provider for a full sub-agent loop.

