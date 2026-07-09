package middleware

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/exec"
	"strings"

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

// AfterResponse intercepts torana_delegate_task calls and executes them.
func (d *Delegator) AfterResponse(ctx context.Context, resp *http.Response, events <-chan engine.StreamEvent, req *http.Request, chat *engine.ChatRequest) (<-chan engine.StreamEvent, error) {
	out := make(chan engine.StreamEvent, 16)
	go func() {
		defer close(out)

		// Track tool calls in-flight to detect delegation.
		var currentTool *struct {
			id   string
			name string
			args strings.Builder
		}

		for ev := range events {
			switch {
			case ev.ToolCallStart != nil:
				currentTool = &struct {
					id   string
					name string
					args strings.Builder
				}{
					id:   ev.ToolCallStart.ID,
					name: ev.ToolCallStart.Name,
				}

			case ev.ToolCallDelta != nil && currentTool != nil:
				currentTool.args.WriteString(ev.ToolCallDelta.ArgumentsDelta)

			case ev.ToolCallEnd != nil && currentTool != nil:
				if currentTool.name == "torana_delegate_task" {
					// Execute delegated task — don't forward to harness.
					result := d.executeTask(currentTool.args.String())
					// Inject the result as completion content.
					out <- engine.StreamEvent{TextDelta: stringPtr("\n" + result + "\n")}
				} else {
					out <- ev // forward non-delegate tool calls
				}
				currentTool = nil

			default:
				out <- ev
			}
		}
	}()
	return out, nil
}

// executeTask runs a delegated task locally.
// In v0.3.0 this is a simple mock/exec; future versions will use
// the offload provider for a full sub-agent loop.
func (d *Delegator) executeTask(argsJSON string) string {
	var args struct {
		Task     string `json:"task"`
		MaxTurns int    `json:"max_turns"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "[Delegator] Failed to parse task: " + err.Error()
	}
	if args.Task == "" {
		return "[Delegator] Empty task — nothing to delegate."
	}

	log.Printf("[delegator] executing task: %s", args.Task)

	// Simple heuristic: if the task looks like a grep/find request, run it.
	if strings.Contains(strings.ToLower(args.Task), "grep") ||
		strings.Contains(strings.ToLower(args.Task), "find") {
		cmd := exec.Command("bash", "-c", args.Task)
		cmd.Dir = "/home/aniket/repos/torana/torana-edge"
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "[Delegator] Task failed: " + err.Error() + "\nOutput: " + string(out)
		}
		return string(out)
	}

	// Mock: return a canned response for non-grep tasks.
	return "[Delegator] Task '" + args.Task + "' delegated to sub-agent. Use grep/find tasks for real execution in this version."
}

func stringPtr(s string) *string { return &s }
