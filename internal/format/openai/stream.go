package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// StreamAdapter implements format.StreamAdapter for OpenAI SSE streams.
type StreamAdapter struct{}

// --- wire types for parse ---------------------------------------------------

type sseChunk struct {
	ID      string      `json:"id,omitempty"`
	Object  string      `json:"object,omitempty"`
	Choices []sseChoice `json:"choices,omitempty"`
	Usage   *sseUsage   `json:"usage,omitempty"`
	Error   *sseError   `json:"error,omitempty"`

	// Responses API fields
	Type     string           `json:"type,omitempty"`
	ItemID   string           `json:"item_id,omitempty"`
	Delta    *string          `json:"delta,omitempty"`
	Item     *responsesItem   `json:"item,omitempty"`
	Response *responsesObject `json:"response,omitempty"`
}

type responsesItem struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type,omitempty"`
	Name   string `json:"name,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

type responsesObject struct {
	Status string          `json:"status,omitempty"`
	Usage  *responsesUsage `json:"usage,omitempty"`
}

// responsesUsage is deliberately separate from Chat Completions usage: the
// Responses API calls these fields input/output tokens and nests prompt-cache
// details under input_tokens_details.
type responsesUsage struct {
	InputTokens        int                          `json:"input_tokens"`
	OutputTokens       int                          `json:"output_tokens"`
	InputTokensDetails *responsesInputTokensDetails `json:"input_tokens_details,omitempty"`
}

type responsesInputTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type sseUsage struct {
	PromptTokens          int                     `json:"prompt_tokens"`
	CompletionTokens      int                     `json:"completion_tokens"`
	TotalTokens           int                     `json:"total_tokens"`
	PromptTokensDetails   *ssePromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	PromptCacheHitTokens  int                     `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int                     `json:"prompt_cache_miss_tokens,omitempty"`
}

// ssePromptTokensDetails carries the automatic prompt-cache hit count
// (cached_tokens is a subset of prompt_tokens, billed at a discount).
type ssePromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          *string       `json:"content,omitempty"`
	ReasoningContent *string       `json:"reasoning_content,omitempty"`
	ToolCalls        []sseToolCall `json:"tool_calls,omitempty"`
}

type sseToolCall struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function sseToolFunc `json:"function,omitempty"`
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

	// Responses API argument events identify their function call by item_id,
	// while the call_id and name are supplied by output_item.added. Keep that
	// lifecycle state so interleaved calls remain associated with the right
	// canonical tool-call index.
	itemIDToIndex := make(map[string]int)
	nextIndex := 0

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

		// Usage arrives on the final chunk (empty choices) when the client —
		// or the proxy on its behalf — asked for stream_options.include_usage.
		if chunk.Usage != nil && (chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0) {
			u := &engine.StreamUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				u.CacheReadTokens = d.CachedTokens
			} else {
				u.CacheReadTokens = chunk.Usage.PromptCacheHitTokens
			}
			ch <- engine.StreamEvent{Usage: u}
		}

		if chunk.Type != "" {
			s.parseResponsesEvent(chunk, ch, itemIDToIndex, &nextIndex)
			continue
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

				// If tool_calls, emit ToolCallEnd for every started index
				// first, in ascending order — map iteration order is
				// nondeterministic.
				if fr == "tool_calls" {
					indexes := make([]int, 0, len(toolCallStarted))
					for idx := range toolCallStarted {
						indexes = append(indexes, idx)
					}
					sort.Ints(indexes)
					for _, idx := range indexes {
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

func (s *StreamAdapter) parseResponsesEvent(chunk sseChunk, ch chan<- engine.StreamEvent, itemIDToIndex map[string]int, nextIndex *int) {
	switch chunk.Type {
	case "response.output_text.delta":
		if chunk.Delta != nil && *chunk.Delta != "" {
			ch <- engine.StreamEvent{
				TextDelta: chunk.Delta,
			}
		}

	case "response.output_item.added":
		if chunk.Item != nil && chunk.Item.Type == "function_call" {
			idx := *nextIndex
			itemIDToIndex[chunk.Item.ID] = idx
			*nextIndex = idx + 1

			ch <- engine.StreamEvent{
				ToolCallStart: &engine.ToolCallStart{
					Index: idx,
					ID:    chunk.Item.CallID,
					Name:  chunk.Item.Name,
				},
			}
		}

	case "response.function_call_arguments.delta":
		if chunk.Delta != nil && *chunk.Delta != "" && chunk.ItemID != "" {
			if idx, ok := itemIDToIndex[chunk.ItemID]; ok {
				ch <- engine.StreamEvent{
					ToolCallDelta: &engine.ToolCallDelta{
						Index:          idx,
						ArgumentsDelta: *chunk.Delta,
					},
				}
			}
		}

	case "response.function_call_arguments.done":
		if chunk.ItemID != "" {
			if idx, ok := itemIDToIndex[chunk.ItemID]; ok {
				ch <- engine.StreamEvent{
					ToolCallEnd: &engine.ToolCallEnd{
						Index: idx,
					},
				}
			}
		}

	case "response.completed":
		if chunk.Response != nil {
			if chunk.Response.Status == "completed" {
				ch <- engine.StreamEvent{
					FinishReason: "stop",
				}
			}
			if chunk.Response.Usage != nil {
				u := &engine.StreamUsage{
					InputTokens:  chunk.Response.Usage.InputTokens,
					OutputTokens: chunk.Response.Usage.OutputTokens,
				}
				if details := chunk.Response.Usage.InputTokensDetails; details != nil {
					u.CacheReadTokens = details.CachedTokens
					u.CacheWriteTokens = details.CacheWriteTokens
				}
				ch <- engine.StreamEvent{
					Usage: u,
				}
			}
		}

	case "response.failed":
		if chunk.Error != nil {
			ch <- engine.StreamEvent{
				Error: &engine.StreamError{
					Message: chunk.Error.Message,
				},
			}
		}
	}
}

// ---------------------------------------------------------------------------
// SerializeStream
// ---------------------------------------------------------------------------

const streamID = "chatcmpl-torana"

// SerializeStream writes StreamEvents from the channel as SSE to writer.
func (s *StreamAdapter) SerializeStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error {
	if chat, ok := ctx.Value(engine.ChatRequestKey).(*engine.ChatRequest); ok {
		if variant, ok := chat.ProviderExtensions["_openai_variant"].(string); ok && variant == "responses" {
			return s.serializeResponsesStream(w, events)
		}
	}
	return s.serializeChatStream(w, events)
}

func (s *StreamAdapter) serializeChatStream(w io.Writer, events <-chan engine.StreamEvent) error {
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
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("openai serialize write: %w", err)
	}
	return nil
}

func (s *StreamAdapter) serializeResponsesStream(w io.Writer, events <-chan engine.StreamEvent) error {
	toolCallStarted := make(map[int]string)        // index -> ID
	toolCallArgs := make(map[int]*strings.Builder) // index -> accumulated arguments

	for evt := range events {
		switch {
		case evt.Error != nil:
			payload := map[string]any{
				"type": "response.failed",
				"error": map[string]any{
					"message": evt.Error.Message,
				},
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", string(b)); err != nil {
				return err
			}
			return fmt.Errorf("openai responses stream error: %s", evt.Error.Message)

		case evt.TextDelta != nil:
			payload := map[string]any{
				"type":  "response.output_text.delta",
				"delta": *evt.TextDelta,
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n", string(b)); err != nil {
				return err
			}

		case evt.ThinkingDelta != nil:
			payload := map[string]any{
				"type":  "response.output_text.delta",
				"delta": *evt.ThinkingDelta,
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n", string(b)); err != nil {
				return err
			}

		case evt.ToolCallStart != nil:
			tc := evt.ToolCallStart
			toolCallStarted[tc.Index] = tc.ID
			toolCallArgs[tc.Index] = &strings.Builder{}

			payload := map[string]any{
				"type": "response.output_item.added",
				"item": map[string]any{
					"id":      "item_" + tc.ID,
					"type":    "function_call",
					"name":    tc.Name,
					"call_id": tc.ID,
				},
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: response.output_item.added\ndata: %s\n\n", string(b)); err != nil {
				return err
			}

		case evt.ToolCallDelta != nil:
			tcd := evt.ToolCallDelta
			id, ok := toolCallStarted[tcd.Index]
			if !ok {
				continue
			}
			if builder, ok := toolCallArgs[tcd.Index]; ok {
				builder.WriteString(tcd.ArgumentsDelta)
			}

			payload := map[string]any{
				"type":    "response.function_call_arguments.delta",
				"item_id": "item_" + id,
				"delta":   tcd.ArgumentsDelta,
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: response.function_call_arguments.delta\ndata: %s\n\n", string(b)); err != nil {
				return err
			}

		case evt.ToolCallEnd != nil:
			tce := evt.ToolCallEnd
			id, ok := toolCallStarted[tce.Index]
			if !ok {
				continue
			}
			args := ""
			if builder, ok := toolCallArgs[tce.Index]; ok {
				args = builder.String()
			}

			payloadDone := map[string]any{
				"type":    "response.function_call_arguments.done",
				"item_id": "item_" + id,
			}
			bDone, _ := json.Marshal(payloadDone)
			if _, err := fmt.Fprintf(w, "event: response.function_call_arguments.done\ndata: %s\n\n", string(bDone)); err != nil {
				return err
			}

			payloadItem := map[string]any{
				"type": "response.output_item.done",
				"item": map[string]any{
					"id":        "item_" + id,
					"type":      "function_call",
					"call_id":   id,
					"arguments": args,
				},
			}
			bItem, _ := json.Marshal(payloadItem)
			if _, err := fmt.Fprintf(w, "event: response.output_item.done\ndata: %s\n\n", string(bItem)); err != nil {
				return err
			}

		case evt.FinishReason != "":
			payload := map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
				},
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", string(b)); err != nil {
				return err
			}

		case evt.Usage != nil:
			payload := map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"usage": map[string]any{
						"prompt_tokens":     evt.Usage.InputTokens,
						"completion_tokens": evt.Usage.OutputTokens,
						"total_tokens":      evt.Usage.InputTokens + evt.Usage.OutputTokens,
					},
				},
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", string(b)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
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

	case evt.Usage != nil:
		return usageSSE(evt.Usage), nil

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
		"id":     streamID,
		"object": "chat.completion.chunk",
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
		"id":     streamID,
		"object": "chat.completion.chunk",
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
		"id":     streamID,
		"object": "chat.completion.chunk",
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
		"id":     streamID,
		"object": "chat.completion.chunk",
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
		"id":     streamID,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": reason,
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}

// usageSSE is the final usage chunk (empty choices), the shape OpenAI sends
// when stream_options.include_usage is set.
func usageSSE(u *engine.StreamUsage) string {
	usage := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
	chunk := map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{},
		"usage":   usage,
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
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
