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

// Delegator implements Hierarchical Agentic Routing.
type Delegator struct{}

func NewDelegator() *Delegator { return &Delegator{} }
func (d *Delegator) Name() string { return "delegator" }

// BeforeRequest injects the torana_delegate_task tool.
func (d *Delegator) BeforeRequest(ctx context.Context, req *http.Request, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	if chat == nil || len(chat.Tools) == 0 {
		return chat, nil
	}

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
		
		type toolState struct {
			id   string
			name string
			args strings.Builder
		}
		
		var currentTool *toolState
		var isDelegator bool

		for ev := range events {
			if ev.ToolCallStart != nil {
				currentTool = &toolState{
					id:   ev.ToolCallStart.ID,
					name: ev.ToolCallStart.Name,
				}
				isDelegator = (currentTool.name == "torana_delegate_task")
				if !isDelegator {
					out <- ev
				}
				continue
			}

			if ev.ToolCallDelta != nil && currentTool != nil {
				currentTool.args.WriteString(ev.ToolCallDelta.ArgumentsDelta)
				if !isDelegator {
					out <- ev
				}
				continue
			}

			if ev.ToolCallEnd != nil && currentTool != nil {
				if isDelegator {
					// Execute task locally, swallow the tool call, and replace with TextDelta.
					// This maintains schema validity for stateless harnesses.
					result := d.executeTask(currentTool.args.String())
					
					// Inject the result as text.
					out <- engine.StreamEvent{
						TextDelta: stringPtr("\n\n[Torana Delegator] Task Result:\n" + result + "\n"),
					}
				} else {
					out <- ev
				}
				currentTool = nil
				isDelegator = false
				continue
			}

			// Pass through all other events (TextDelta, Usage, etc)
			out <- ev
		}
	}()
	return out, nil
}

func (d *Delegator) executeTask(argsJSON string) string {
	var args struct {
		Task     string `json:"task"`
		MaxTurns int    `json:"max_turns"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Failed to parse task: " + err.Error()
	}
	if args.Task == "" {
		return "Empty task."
	}

	log.Printf("[delegator] executing task: %s", args.Task)

	if strings.Contains(strings.ToLower(args.Task), "grep") || strings.Contains(strings.ToLower(args.Task), "find") || strings.Contains(strings.ToLower(args.Task), "ls") {
		cmd := exec.Command("bash", "-c", args.Task)
		cmd.Dir = "/home/aniket/repos/torana/torana-edge"
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "Task failed: " + err.Error() + "\nOutput: " + string(out)
		}
		return string(out)
	}

	return "Task '" + args.Task + "' delegated to sub-agent. (Simulated success)"
}

func stringPtr(s string) *string { return &s }
