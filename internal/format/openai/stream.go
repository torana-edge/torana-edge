package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// StreamAdapter implements format.StreamAdapter for OpenAI SSE streams.
type StreamAdapter struct{}

// --- wire types for parse ---------------------------------------------------

type sseChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Choices []sseChoice    `json:"choices"`
	Error   *sseError      `json:"error,omitempty"`
}

type sseChoice struct {
	Index        int        `json:"index"`
	Delta        sseDelta   `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type sseDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          *string       `json:"content,omitempty"`
	ReasoningContent *string       `json:"reasoning_content,omitempty"`
	ToolCalls        []sseToolCall `json:"tool_calls,omitempty"`
}

type sseToolCall struct {
	Index    int           `json:"index"`
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function sseToolFunc   `json:"function,omitempty"`
}

type sseToolFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type sseError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    *int   `json:"code,omitempty"`
}

// ---------------------------------------------------------------------------
// ParseStream
// ---------------------------------------------------------------------------

// ParseStream reads an OpenAI SSE byte stream and emits StreamEvents to the
// returned channel. The channel is closed when the stream ends or on error.
func (s *StreamAdapter) ParseStream(body io.Reader) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent)
	go func() {
		defer close(ch)
		s.parseStream(body, ch)
	}()
	return ch
}

func (s *StreamAdapter) parseStream(body io.Reader, ch chan<- engine.StreamEvent) {
	scanner := bufio.NewScanner(body)
	// SSE lines can be long for function arguments; bump the buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// toolCallStarted tracks which indices have emitted ToolCallStart.
	toolCallStarted := make(map[int]bool)

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines (SSE field separator).
		if line == "" {
			continue
		}

		// Must be a "data: " line.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")

		// Stream termination.
		if payload == "[DONE]" {
			return
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // skip unparseable lines
		}

		// Handle error events.
		if chunk.Error != nil {
			code := 0
			if chunk.Error.Code != nil {
				code = *chunk.Error.Code
			}
			ch <- engine.StreamEvent{
				Error: &engine.StreamError{
					Code:    code,
					Message: chunk.Error.Message,
				},
			}
			return
		}

		// Process choices.
		for _, choice := range chunk.Choices {
			delta := choice.Delta

			if delta.Role != "" && delta.Content == nil && delta.ReasoningContent == nil && len(delta.ToolCalls) == 0 {
				continue
			}

			// Text content delta.
			if delta.Content != nil && *delta.Content != "" {
				ch <- engine.StreamEvent{
					TextDelta: delta.Content,
				}
			}

			// Reasoning/thinking content delta.
			if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
				ch <- engine.StreamEvent{
					ThinkingDelta: delta.ReasoningContent,
				}
			}

			// Tool calls in delta.
			for _, tc := range delta.ToolCalls {
				// ToolCallStart: first time we see id+name for this index.
				if tc.ID != "" && tc.Function.Name != "" && !toolCallStarted[tc.Index] {
					toolCallStarted[tc.Index] = true
					ch <- engine.StreamEvent{
						ToolCallStart: &engine.ToolCallStart{
							Index: tc.Index,
							ID:    tc.ID,
							Name:  tc.Function.Name,
						},
					}
				}

				// ToolCallDelta: arguments fragment.
				if tc.Function.Arguments != "" {
					if !toolCallStarted[tc.Index] {
						// Arguments arrived before the start chunk; emit as text.
						ch <- engine.StreamEvent{
							TextDelta: &tc.Function.Arguments,
						}
					} else {
						ch <- engine.StreamEvent{
							ToolCallDelta: &engine.ToolCallDelta{
								Index:          tc.Index,
								ArgumentsDelta: tc.Function.Arguments,
							},
						}
					}
				}
			}

			// Finish reason.
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				fr := *choice.FinishReason

				// If tool_calls, emit ToolCallEnd for every started index first.
				if fr == "tool_calls" {
					for idx := range toolCallStarted {
						ch <- engine.StreamEvent{
							ToolCallEnd: &engine.ToolCallEnd{
								Index: idx,
							},
						}
					}
				}

				ch <- engine.StreamEvent{
					FinishReason: fr,
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// SerializeStream
// ---------------------------------------------------------------------------

const streamID = "chatcmpl-torana"

// SerializeStream writes StreamEvents as OpenAI Chat Completions SSE to writer.
// Returns when the channel is closed or on write error.
func (s *StreamAdapter) SerializeStream(w io.Writer, events <-chan engine.StreamEvent) error {
	for evt := range events {
		line, err := serializeEvent(evt)
		if err != nil {
			return fmt.Errorf("openai serialize: %w", err)
		}
		if line == "" {
			continue
		}
		if _, err := fmt.Fprint(w, line); err != nil {
			return fmt.Errorf("openai serialize write: %w", err)
		}
	}
	return nil
}

func serializeEvent(evt engine.StreamEvent) (string, error) {
	switch {
	case evt.TextDelta != nil:
		return textDeltaSSE(*evt.TextDelta), nil

	case evt.ThinkingDelta != nil:
		return thinkingDeltaSSE(*evt.ThinkingDelta), nil

	case evt.ToolCallStart != nil:
		return toolCallStartSSE(evt.ToolCallStart), nil

	case evt.ToolCallDelta != nil:
		return toolCallDeltaSSE(evt.ToolCallDelta), nil

	// ToolCallEnd does not emit a standalone SSE chunk; we only emit on
	// FinishReason (which must precede ToolCallEnd in the stream protocol).

	case evt.FinishReason != "":
		return finishReasonSSE(evt.FinishReason), nil

	case evt.Error != nil:
		return errorSSE(evt.Error), nil
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// SSE line builders
// ---------------------------------------------------------------------------

func textDeltaSSE(text string) string {
	chunk := map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"content": text,
				},
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}

func thinkingDeltaSSE(text string) string {
	chunk := map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"reasoning_content": text,
				},
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}

func toolCallStartSSE(tc *engine.ToolCallStart) string {
	chunk := map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{
						{
							"index": tc.Index,
							"id":    tc.ID,
							"type":  "function",
							"function": map[string]any{
								"name":      tc.Name,
								"arguments": "",
							},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}

func toolCallDeltaSSE(tc *engine.ToolCallDelta) string {
	chunk := map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{
						{
							"index": tc.Index,
							"function": map[string]any{
								"arguments": tc.ArgumentsDelta,
							},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}

func finishReasonSSE(reason string) string {
	chunk := map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": reason,
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n%s", string(b), "data: [DONE]\n\n")
}

func errorSSE(err *engine.StreamError) string {
	chunk := map[string]any{
		"error": map[string]any{
			"message": err.Message,
			"type":    "stream_error",
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}
