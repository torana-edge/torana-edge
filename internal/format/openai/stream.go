package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/format/streamio"
)

// StreamAdapter implements format.StreamAdapter for OpenAI SSE streams.
type StreamAdapter struct{}

// --- wire types for parse ---------------------------------------------------

type sseChunk struct {
	ID      string         `json:"id,omitempty"`
	Object  string         `json:"object,omitempty"`
	Choices []sseChoice    `json:"choices,omitempty"`
	Usage   map[string]any `json:"usage,omitempty"`
	Error   *sseError      `json:"error,omitempty"`

	// Responses API fields
	Type         string           `json:"type,omitempty"`
	ItemID       string           `json:"item_id,omitempty"`
	Delta        *string          `json:"delta,omitempty"`
	Item         *responsesItem   `json:"item,omitempty"`
	Response     *responsesObject `json:"response,omitempty"`
	OutputIndex  int              `json:"output_index,omitempty"`
	ContentIndex int              `json:"content_index,omitempty"`
}

type responsesItem struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type,omitempty"`
	Name   string `json:"name,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

type responsesObject struct {
	ID     string         `json:"id,omitempty"`
	Model  string         `json:"model,omitempty"`
	Status string         `json:"status,omitempty"`
	Usage  map[string]any `json:"usage,omitempty"`
}

// Both usage fields above are raw maps on purpose: OpenAI's two variants name
// their token fields differently, and ReadUsage in usage.go owns that mapping
// for the streaming and non-streaming paths alike. Typed structs here meant the
// field names existed in two places, and one of them fell behind.

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

type chatToolCallState struct {
	id      string
	name    string
	started bool
	pending []string
}

type responsesToolCallState struct {
	index int
	kind  engine.ToolInvocationKind
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
	scanner := streamio.NewScanner(body)

	// Chat Completions providers may split id, name, and arguments across
	// chunks in any order. Buffer arguments until the complete start identity
	// exists; tool JSON must never be reclassified as visible assistant text.
	toolCalls := make(map[int]*chatToolCallState)
	hasUnresolvedToolCall := func() bool {
		for _, state := range toolCalls {
			if !state.started {
				return true
			}
		}
		return false
	}
	emitUnresolvedToolCall := func() {
		ch <- engine.StreamEvent{Error: &engine.StreamError{
			Code:    -1,
			Message: "openai: incomplete streamed tool-call start",
		}}
	}

	// Responses API payload events identify their function/custom call by
	// item_id, while call_id, name, and invocation family are supplied by
	// output_item.added. Keep that lifecycle state so interleaved calls remain
	// associated with the right canonical tool-call index.
	itemIDToIndex := make(map[string]responsesToolCallState)
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
			if hasUnresolvedToolCall() {
				emitUnresolvedToolCall()
			}
			return
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			ch <- engine.StreamEvent{Error: &engine.StreamError{
				Code:    -1,
				Message: "openai: malformed streamed event",
			}}
			return
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
		if u := ReadUsage(chunk.Usage); u != nil {
			ch <- engine.StreamEvent{Usage: u}
		}

		if chunk.Type != "" {
			if err := s.parseResponsesEvent(chunk, ch, itemIDToIndex, &nextIndex); err != nil {
				ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: err.Error()}}
				return
			}
			continue
		}
		if len(chunk.Choices) > 1 {
			ch <- engine.StreamEvent{Error: &engine.StreamError{
				Code:    -1,
				Message: "openai: multiple streamed choices are unsupported",
			}}
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
				state := toolCalls[tc.Index]
				if state == nil {
					state = &chatToolCallState{}
					toolCalls[tc.Index] = state
				}
				if tc.ID != "" {
					state.id = tc.ID
				}
				if tc.Function.Name != "" {
					state.name = tc.Function.Name
				}

				// ToolCallStart: first time the accumulated id+name is complete.
				if state.id != "" && state.name != "" && !state.started {
					state.started = true
					ch <- engine.StreamEvent{
						ToolCallStart: &engine.ToolCallStart{
							Index: tc.Index,
							ID:    state.id,
							Name:  state.name,
						},
					}
					for _, arguments := range state.pending {
						ch <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{
							Index:          tc.Index,
							ArgumentsDelta: arguments,
						}}
					}
					state.pending = nil
				}

				// ToolCallDelta: arguments fragment.
				if tc.Function.Arguments != "" {
					if !state.started {
						state.pending = append(state.pending, tc.Function.Arguments)
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
					if hasUnresolvedToolCall() {
						emitUnresolvedToolCall()
						return
					}
					indexes := make([]int, 0, len(toolCalls))
					for idx := range toolCalls {
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
	if err := scanner.Err(); err != nil {
		ch <- engine.StreamEvent{Error: &engine.StreamError{
			Code:    500,
			Message: fmt.Sprintf("openai stream read: %v", err),
		}}
		return
	}
	if hasUnresolvedToolCall() {
		emitUnresolvedToolCall()
	}
}

func (s *StreamAdapter) parseResponsesEvent(chunk sseChunk, ch chan<- engine.StreamEvent, itemIDToIndex map[string]responsesToolCallState, nextIndex *int) error {
	switch chunk.Type {
	case "response.created":
		if chunk.Response != nil {
			ch <- engine.StreamEvent{MessageStart: &engine.StreamMessageStart{
				Role: "assistant", ID: chunk.Response.ID, Model: chunk.Response.Model,
			}}
		}
	case "response.output_text.delta":
		if chunk.Delta != nil && *chunk.Delta != "" {
			ch <- engine.StreamEvent{
				TextDelta: chunk.Delta,
			}
		}

	case "response.output_item.added":
		if chunk.Item != nil && (chunk.Item.Type == "function_call" || chunk.Item.Type == "custom_tool_call") {
			idx := *nextIndex
			kind := engine.ToolInvocationFunction
			if chunk.Item.Type == "custom_tool_call" {
				kind = engine.ToolInvocationFreeform
			}
			itemIDToIndex[chunk.Item.ID] = responsesToolCallState{index: idx, kind: kind}
			*nextIndex = idx + 1

			ch <- engine.StreamEvent{
				ToolCallStart: &engine.ToolCallStart{
					Index:          idx,
					ID:             chunk.Item.CallID,
					Name:           chunk.Item.Name,
					InvocationKind: kind,
				},
			}
		}

	case "response.function_call_arguments.delta":
		if chunk.Delta != nil && *chunk.Delta != "" && chunk.ItemID != "" {
			if state, ok := itemIDToIndex[chunk.ItemID]; ok {
				if state.kind != engine.ToolInvocationFunction {
					return fmt.Errorf("openai: function arguments delta targets a free-form tool call")
				}
				ch <- engine.StreamEvent{
					ToolCallDelta: &engine.ToolCallDelta{
						Index:          state.index,
						ArgumentsDelta: *chunk.Delta,
					},
				}
			}
		}

	case "response.function_call_arguments.done":
		if chunk.ItemID != "" {
			if state, ok := itemIDToIndex[chunk.ItemID]; ok {
				if state.kind != engine.ToolInvocationFunction {
					return fmt.Errorf("openai: function arguments completion targets a free-form tool call")
				}
				ch <- engine.StreamEvent{
					ToolCallEnd: &engine.ToolCallEnd{
						Index: state.index,
					},
				}
			}
		}

	case "response.custom_tool_call_input.delta":
		if chunk.Delta != nil && chunk.ItemID != "" {
			if state, ok := itemIDToIndex[chunk.ItemID]; ok {
				if state.kind != engine.ToolInvocationFreeform {
					return fmt.Errorf("openai: free-form input delta targets a function tool call")
				}
				input := *chunk.Delta
				ch <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: state.index, InputTextDelta: &input}}
			}
		}

	case "response.custom_tool_call_input.done":
		if chunk.ItemID != "" {
			if state, ok := itemIDToIndex[chunk.ItemID]; ok {
				if state.kind != engine.ToolInvocationFreeform {
					return fmt.Errorf("openai: free-form input completion targets a function tool call")
				}
				ch <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: state.index}}
			}
		}

	case "response.completed", "response.incomplete":
		if chunk.Response != nil {
			finish := "stop"
			if chunk.Type == "response.incomplete" || chunk.Response.Status == "incomplete" {
				finish = "length"
			}
			ch <- engine.StreamEvent{FinishReason: finish}
			if u := ReadUsage(chunk.Response.Usage); u != nil {
				ch <- engine.StreamEvent{Usage: u}
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
	return nil
}

// ---------------------------------------------------------------------------
// SerializeStream
// ---------------------------------------------------------------------------

const streamID = "chatcmpl-torana"

// SerializeStream writes StreamEvents from the channel as SSE to writer.
func (s *StreamAdapter) SerializeStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error {
	if chat, ok := ctx.Value(engine.ChatRequestKey).(*engine.ChatRequest); ok {
		if chat.OpenAIVariant == engine.OpenAIResponses {
			return s.serializeResponsesStream(ctx, w, events)
		}
	}
	return s.serializeChatStream(ctx, w, events)
}

// blockTopology tracks the one content block open in the stream being
// serialized, enforcing the plugin ABI's single-open-block + unique-index
// discipline on explicit block events BEFORE they are lowered to the wire. A
// second start while a block is open (any index), a start whose index was
// already used in this message, or a stop that does not name the open block is
// malformed topology and errors — never silently accepted.
//
// Tool-call blocks ride the raw ToolCallStart/Delta/End events and do NOT go
// through this state (their protocol allows several open calls at once); it
// exists only for the explicit text/thinking BlockStart/BlockStop events the
// host's verified IR emits.
type blockTopology struct {
	prefix string
	kind   engine.BlockKind
	index  int
	open   bool
	seen   map[int]struct{}
}

// start records an explicit block start, or errors if the discipline is
// violated. It does not decide what the wire renders — the caller lowers each
// kind to its protocol arm.
func (b *blockTopology) start(index int, kind engine.BlockKind) error {
	if b.open {
		return fmt.Errorf("%s: content block start at index %d while a %s block at index %d is still open", b.prefix, index, b.kind, b.index)
	}
	if b.seen == nil {
		b.seen = make(map[int]struct{})
	}
	if _, used := b.seen[index]; used {
		return fmt.Errorf("%s: content block index %d reused within one streamed message", b.prefix, index)
	}
	b.kind, b.index, b.open = kind, index, true
	b.seen[index] = struct{}{}
	return nil
}

// stop validates that the stop names the open block and returns the kind of
// the block it closed (so the caller can lower the close to its protocol arm).
func (b *blockTopology) stop(index int) (engine.BlockKind, error) {
	if !b.open {
		return engine.BlockKindText, fmt.Errorf("%s: content block stop at index %d has no open block", b.prefix, index)
	}
	if index != b.index {
		return engine.BlockKindText, fmt.Errorf("%s: content block stop at index %d does not match the open %s block at index %d", b.prefix, index, b.kind, b.index)
	}
	kind := b.kind
	b.open = false
	return kind, nil
}

func (s *StreamAdapter) serializeChatStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error {
	blocks := &blockTopology{prefix: "openai"}
	for {
		var evt engine.StreamEvent
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok = <-events:
		}
		if !ok {
			break
		}
		line, err := serializeEvent(evt, blocks)
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
	// ctx.Done and a closed events channel may both be ready. If the channel
	// branch won the select above, never turn that aborted stream into a clean
	// OpenAI completion marker.
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("openai serialize write: %w", err)
	}
	return nil
}

func (s *StreamAdapter) serializeResponsesStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error {
	type responseToolState struct {
		id, name    string
		kind        engine.ToolInvocationKind
		payload     strings.Builder
		outputIndex int
		item        map[string]any
	}
	toolCalls := make(map[int]*responseToolState)
	blocks := &blockTopology{prefix: "openai"}
	responseID := "resp_torana"
	model := ""
	started := false
	finishReason := ""
	var usage *engine.StreamUsage
	var outputs []any
	textStarted := false
	textClosed := false
	textIndex := -1
	textID := ""
	var text strings.Builder

	emit := func(kind string, payload map[string]any) error {
		payload["type"] = kind
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
		return err
	}
	ensureStarted := func() error {
		if started {
			return nil
		}
		started = true
		return emit("response.created", map[string]any{"response": map[string]any{
			"id": responseID, "object": "response", "model": model,
			"status": "in_progress", "output": []any{},
		}})
	}
	ensureText := func() error {
		if textStarted {
			return nil
		}
		if err := ensureStarted(); err != nil {
			return err
		}
		textStarted = true
		textIndex = len(outputs)
		textID = fmt.Sprintf("msg_%d", textIndex)
		item := map[string]any{"id": textID, "type": "message", "role": "assistant", "content": []any{}}
		outputs = append(outputs, item)
		if err := emit("response.output_item.added", map[string]any{"output_index": textIndex, "item": item}); err != nil {
			return err
		}
		return emit("response.content_part.added", map[string]any{
			"output_index": textIndex, "content_index": 0, "item_id": textID,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	closeText := func() error {
		if !textStarted || textClosed {
			return nil
		}
		textClosed = true
		value := text.String()
		part := map[string]any{"type": "output_text", "text": value, "annotations": []any{}}
		item := map[string]any{"id": textID, "type": "message", "role": "assistant", "content": []any{part}}
		outputs[textIndex] = item
		if err := emit("response.output_text.done", map[string]any{"output_index": textIndex, "content_index": 0, "item_id": textID, "text": value}); err != nil {
			return err
		}
		if err := emit("response.content_part.done", map[string]any{"output_index": textIndex, "content_index": 0, "item_id": textID, "part": part}); err != nil {
			return err
		}
		return emit("response.output_item.done", map[string]any{"output_index": textIndex, "item": item})
	}

	for {
		var evt engine.StreamEvent
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok = <-events:
		}
		if !ok {
			break
		}
		switch {
		case evt.MessageStart != nil:
			if evt.MessageStart.ID != "" {
				responseID = evt.MessageStart.ID
			}
			model = evt.MessageStart.Model
			if err := ensureStarted(); err != nil {
				return err
			}
		case evt.BlockStart != nil:
			// Provider blocks have no representation on the Responses wire:
			// they error rather than vanish. Canonical text/thinking blocks
			// have no start/stop frames either, so the start lowers to no
			// wire content (an empty block naturally produces none) and the
			// deltas ride the output_text.delta arm — sequence-validated
			// first, never cast.
			if evt.BlockStart.Kind == engine.BlockKindProvider {
				return fmt.Errorf("openai: provider block kind %q is not supported by this serializer", evt.BlockStart.ProviderKind)
			}
			if err := blocks.start(evt.BlockStart.Index, evt.BlockStart.Kind); err != nil {
				return err
			}

		case evt.BlockStop != nil:
			if _, err := blocks.stop(evt.BlockStop.Index); err != nil {
				return err
			}

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
			if err := ensureText(); err != nil {
				return err
			}
			text.WriteString(*evt.TextDelta)
			if err := emit("response.output_text.delta", map[string]any{
				"output_index": textIndex, "content_index": 0, "item_id": textID, "delta": *evt.TextDelta,
			}); err != nil {
				return err
			}

		case evt.ThinkingDelta != nil:
			// The Responses wire has no public thinking-content stream arm. Keep
			// the established lowering to assistant output text so canonical
			// thinking blocks remain serializable across protocol boundaries.
			if err := ensureText(); err != nil {
				return err
			}
			text.WriteString(*evt.ThinkingDelta)
			if err := emit("response.output_text.delta", map[string]any{
				"output_index": textIndex, "content_index": 0, "item_id": textID, "delta": *evt.ThinkingDelta,
			}); err != nil {
				return err
			}

		case evt.ToolCallStart != nil:
			if err := closeText(); err != nil {
				return err
			}
			if err := ensureStarted(); err != nil {
				return err
			}
			tc := evt.ToolCallStart
			toolType := "function_call"
			if tc.InvocationKind == engine.ToolInvocationFreeform {
				toolType = "custom_tool_call"
			}
			outputIndex := len(outputs)
			itemID := "item_" + tc.ID
			if tc.ID == "" {
				itemID = fmt.Sprintf("item_%d", outputIndex)
			}
			item := map[string]any{"id": itemID, "type": toolType, "name": tc.Name, "call_id": tc.ID}
			toolCalls[tc.Index] = &responseToolState{id: tc.ID, name: tc.Name, kind: tc.InvocationKind, outputIndex: outputIndex, item: item}
			outputs = append(outputs, item)

			payload := map[string]any{
				"type": "response.output_item.added",
				"item": map[string]any{
					"id":      itemID,
					"type":    toolType,
					"name":    tc.Name,
					"call_id": tc.ID,
				},
				"output_index": outputIndex,
			}
			if err := emit("response.output_item.added", payload); err != nil {
				return err
			}

		case evt.ToolCallDelta != nil:
			tcd := evt.ToolCallDelta
			state, ok := toolCalls[tcd.Index]
			if !ok {
				continue
			}
			fragment := tcd.ArgumentsDelta
			eventType := "response.function_call_arguments.delta"
			if state.kind == engine.ToolInvocationFreeform {
				if tcd.InputTextDelta == nil || tcd.ArgumentsDelta != "" {
					return fmt.Errorf("openai: free-form tool delta at index %d carries the wrong payload family", tcd.Index)
				}
				fragment = *tcd.InputTextDelta
				eventType = "response.custom_tool_call_input.delta"
			} else if tcd.InputTextDelta != nil {
				return fmt.Errorf("openai: function tool delta at index %d carries free-form input", tcd.Index)
			}
			state.payload.WriteString(fragment)

			payload := map[string]any{
				"item_id":      state.item["id"],
				"output_index": state.outputIndex,
				"delta":        fragment,
			}
			if err := emit(eventType, payload); err != nil {
				return err
			}

		case evt.ToolCallEnd != nil:
			tce := evt.ToolCallEnd
			state, ok := toolCalls[tce.Index]
			if !ok {
				continue
			}
			assembled := state.payload.String()
			doneType := "response.function_call_arguments.done"
			itemType := "function_call"
			doneField := "arguments"
			if state.kind == engine.ToolInvocationFreeform {
				doneType = "response.custom_tool_call_input.done"
				itemType = "custom_tool_call"
				doneField = "input"
			}

			payloadDone := map[string]any{
				"item_id":      state.item["id"],
				"output_index": state.outputIndex,
				doneField:      assembled,
			}
			if err := emit(doneType, payloadDone); err != nil {
				return err
			}

			state.item["type"], state.item["call_id"], state.item["name"] = itemType, state.id, state.name
			state.item[doneField] = assembled
			outputs[state.outputIndex] = state.item
			if err := emit("response.output_item.done", map[string]any{"output_index": state.outputIndex, "item": state.item}); err != nil {
				return err
			}
			delete(toolCalls, tce.Index)

		case evt.FinishReason != "":
			finishReason = evt.FinishReason

		case evt.Usage != nil:
			copyUsage := *evt.Usage
			usage = &copyUsage
		}
	}
	// See serializeChatStream: closure is not proof of normal completion when
	// cancellation raced the channel receive.
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(toolCalls) != 0 {
		return fmt.Errorf("openai responses: stream ended with an open tool call")
	}
	if err := closeText(); err != nil {
		return err
	}
	if finishReason == "" {
		return nil
	}
	if err := ensureStarted(); err != nil {
		return err
	}
	status, eventType := "completed", "response.completed"
	if finishReason == "length" {
		status, eventType = "incomplete", "response.incomplete"
	}
	response := map[string]any{"id": responseID, "object": "response", "model": model, "status": status, "output": outputs}
	if usage != nil {
		response["usage"] = map[string]any{
			"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
			"total_tokens": usage.InputTokens + usage.OutputTokens,
		}
	}
	return emit(eventType, map[string]any{"response": response})
}

func serializeEvent(evt engine.StreamEvent, blocks *blockTopology) (string, error) {
	if err := format.RejectFreeformStreamEvent(evt, "openai chat"); err != nil {
		return "", err
	}
	switch {
	case evt.BlockStart != nil:
		// Provider blocks have no representation on the chat wire: they
		// error rather than vanish. Canonical text/thinking blocks have no
		// start/stop frames either, so the start lowers to no wire content
		// (an empty block naturally produces none) and the deltas ride the
		// content/reasoning_content arms — sequence-validated first, never
		// cast.
		if evt.BlockStart.Kind == engine.BlockKindProvider {
			return "", fmt.Errorf("openai: provider block kind %q is not supported by this serializer", evt.BlockStart.ProviderKind)
		}
		if err := blocks.start(evt.BlockStart.Index, evt.BlockStart.Kind); err != nil {
			return "", err
		}
		return "", nil

	case evt.BlockStop != nil:
		if _, err := blocks.stop(evt.BlockStop.Index); err != nil {
			return "", err
		}
		return "", nil

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
