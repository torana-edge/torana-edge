package vertex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// StreamAdapter translates between Gemini JSON-line streams and StreamEvent channels.
type StreamAdapter struct{}

// --- Stream wire types ---

// geminiStreamChunk is a single JSON line from the Gemini streaming endpoint.
type geminiStreamChunk struct {
	Candidates []geminiStreamCandidate `json:"candidates"`
}

type geminiStreamCandidate struct {
	Content      *geminiStreamContent `json:"content,omitempty"`
	FinishReason string               `json:"finishReason,omitempty"`
	SafetyRatings json.RawMessage     `json:"safetyRatings,omitempty"`
}

type geminiStreamContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// --- ParseStream ---

// ParseStream reads Gemini JSON-line streaming responses and emits StreamEvents.
// It is the caller's responsibility to read from the returned channel; the
// channel is closed when the stream ends or on unrecoverable error.
func (s *StreamAdapter) ParseStream(body io.Reader) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent)

	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			var chunk geminiStreamChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				ch <- engine.StreamEvent{
					Error: &engine.StreamError{
						Code:    -1,
						Message: fmt.Sprintf("vertex: parse stream line: %v", err),
					},
				}
				return
			}

			if len(chunk.Candidates) == 0 {
				continue
			}
			candidate := chunk.Candidates[0]

			// Check finish reason first.
			if candidate.FinishReason != "" {
				reason := mapGeminiFinishReason(candidate.FinishReason)
				if reason != "" {
					ch <- engine.StreamEvent{FinishReason: reason}
				}
				continue
			}

			if candidate.Content == nil || len(candidate.Content.Parts) == 0 {
				continue
			}

			for _, part := range candidate.Content.Parts {
				switch {
				case part.Text != "":
					text := part.Text
					ch <- engine.StreamEvent{TextDelta: &text}

				case part.FunctionCall != nil:
					// Gemini sends complete function calls in one event.
					// Synthesize ToolCallStart → ToolCallDelta → ToolCallEnd.
					name := part.FunctionCall.Name
					id := name // Gemini doesn't provide a separate call ID; use name.

					ch <- engine.StreamEvent{
						ToolCallStart: &engine.ToolCallStart{
							Index: 0,
							ID:    id,
							Name:  name,
						},
					}

					// Serialize the complete args as the delta.
					argsJSON, err := json.Marshal(part.FunctionCall.Args)
					if err != nil {
						ch <- engine.StreamEvent{
							Error: &engine.StreamError{
								Code:    -1,
								Message: fmt.Sprintf("vertex: marshal function call args: %v", err),
							},
						}
						return
					}
					argsDelta := string(argsJSON)
					ch <- engine.StreamEvent{
						ToolCallDelta: &engine.ToolCallDelta{
							Index:          0,
							ArgumentsDelta: argsDelta,
						},
					}

					ch <- engine.StreamEvent{
						ToolCallEnd: &engine.ToolCallEnd{Index: 0},
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- engine.StreamEvent{
				Error: &engine.StreamError{
					Code:    -1,
					Message: fmt.Sprintf("vertex: scanner error: %v", err),
				},
			}
		}
	}()

	return ch
}

// mapGeminiFinishReason maps Gemini finish reasons to canonical values.
func mapGeminiFinishReason(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER":
		return "error"
	default:
		return "stop"
	}
}

// --- SerializeStream ---

// serializeState tracks in-progress tool call accumulation.
type serializeState struct {
	Name     string
	ArgsJSON strings.Builder
}

// SerializeStream writes StreamEvents as Gemini JSON lines to writer.
func (s *StreamAdapter) SerializeStream(w io.Writer, events <-chan engine.StreamEvent) error {
	var toolState *serializeState

	emitText := func(text string) error {
		line := geminiStreamChunk{
			Candidates: []geminiStreamCandidate{{
				Content: &geminiStreamContent{
					Role: "model",
					Parts: []geminiPart{{
						Text: text,
					}},
				},
			}},
		}
		return writeLine(w, line)
	}

	emitFunctionCall := func(name string, argsJSON string) error {
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			args = map[string]any{}
		}
		line := geminiStreamChunk{
			Candidates: []geminiStreamCandidate{{
				Content: &geminiStreamContent{
					Role: "model",
					Parts: []geminiPart{{
						FunctionCall: &geminiFuncCall{
							Name: name,
							Args: args,
						},
					}},
				},
			}},
		}
		return writeLine(w, line)
	}

	for event := range events {
		if event.Error != nil {
			errLine := geminiStreamChunk{
				Candidates: []geminiStreamCandidate{{
					FinishReason: "OTHER",
				}},
			}
			_ = writeLine(w, errLine)
			return fmt.Errorf("vertex: stream error: %s", event.Error.Message)
		}

		if event.FinishReason != "" {
			reason := mapCanonicalToGeminiFinishReason(event.FinishReason)
			line := geminiStreamChunk{
				Candidates: []geminiStreamCandidate{{
					FinishReason: reason,
				}},
			}
			return writeLine(w, line)
		}

		switch {
		case event.TextDelta != nil:
			if err := emitText(*event.TextDelta); err != nil {
				return err
			}

		case event.ToolCallStart != nil:
			toolState = &serializeState{
				Name: event.ToolCallStart.Name,
			}

		case event.ToolCallDelta != nil && toolState != nil:
			toolState.ArgsJSON.WriteString(event.ToolCallDelta.ArgumentsDelta)

		case event.ToolCallEnd != nil && toolState != nil:
			if err := emitFunctionCall(toolState.Name, toolState.ArgsJSON.String()); err != nil {
				return err
			}
			toolState = nil
		}
	}

	return nil
}

// mapCanonicalToGeminiFinishReason maps canonical finish reasons back to Gemini.
func mapCanonicalToGeminiFinishReason(r string) string {
	switch r {
	case "stop":
		return "STOP"
	case "tool_calls", "length":
		return "STOP"
	case "error":
		return "OTHER"
	default:
		return "STOP"
	}
}

func writeLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}
