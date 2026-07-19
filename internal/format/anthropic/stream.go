package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// sseEvent is a raw SSE data payload as parsed JSON.
type sseEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index,omitempty"`
	ContentBlock *contentBlockEv `json:"content_block,omitempty"`
	Delta        *deltaEv        `json:"delta,omitempty"`
	Message      *messageEv      `json:"message,omitempty"`
	Usage        *usageEv        `json:"usage,omitempty"` // message_delta carries output tokens here
	Error        *errorEv        `json:"error,omitempty"`
}

type usageEv struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

type contentBlockEv struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

type deltaEv struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

type messageEv struct {
	StopReason string   `json:"stop_reason"`
	Usage      *usageEv `json:"usage,omitempty"` // message_start carries input tokens here
}

type errorEv struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// StreamAdapter implements format.StreamAdapter for Anthropic SSE streams.
type StreamAdapter struct{}

// ParseStream reads an Anthropic SSE stream and emits canonical StreamEvents.
func (s *StreamAdapter) ParseStream(body io.Reader) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(body)
		var blockType string // tracks current content block type: "", "text", "tool_use", "thinking"
		var inputTokens int  // from message_start, reported with output at message_delta
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			// Skip empty data and [DONE] markers.
			if payload == "" || payload == "[DONE]" {
				continue
			}

			var ev sseEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				ch <- engine.StreamEvent{
					Error: &engine.StreamError{
						Code:    500,
						Message: fmt.Sprintf("anthropic stream parse: %v", err),
					},
				}
				continue
			}

			switch {
			case ev.Type == "message_start":
				if ev.Message != nil && ev.Message.Usage != nil {
					inputTokens = ev.Message.Usage.InputTokens
				}

			case ev.Type == "content_block_start":
				if ev.ContentBlock != nil {
					switch ev.ContentBlock.Type {
					case "tool_use":
						blockType = "tool_use"
						ch <- engine.StreamEvent{
							ToolCallStart: &engine.ToolCallStart{
								Index: ev.Index,
								ID:    ev.ContentBlock.ID,
								Name:  ev.ContentBlock.Name,
							},
						}
					case "thinking":
						blockType = "thinking"
					case "text":
						blockType = "text"
					}
				}

			case ev.Type == "content_block_delta":
				if ev.Delta == nil {
					continue
				}
				switch ev.Delta.Type {
				case "text_delta":
					text := ev.Delta.Text
					ch <- engine.StreamEvent{
						TextDelta: &text,
					}
				case "input_json_delta":
					ch <- engine.StreamEvent{
						ToolCallDelta: &engine.ToolCallDelta{
							Index:          ev.Index,
							ArgumentsDelta: ev.Delta.PartialJSON,
						},
					}
				case "thinking_delta":
					thinking := ev.Delta.Thinking
					ch <- engine.StreamEvent{
						ThinkingDelta: &thinking,
					}
				case "signature_delta":
					// Accumulate signature, don't emit as event
				}

			case ev.Type == "content_block_stop":
				if blockType == "tool_use" {
					ch <- engine.StreamEvent{
						ToolCallEnd: &engine.ToolCallEnd{Index: ev.Index},
					}
				}
				blockType = ""

			case ev.Type == "message_delta":
				// Usage precedes FinishReason so serializers can embed it in
				// their final frame (message_delta carries output tokens;
				// input tokens were captured at message_start).
				if ev.Usage != nil && (inputTokens > 0 || ev.Usage.OutputTokens > 0) {
					ch <- engine.StreamEvent{
						Usage: &engine.StreamUsage{
							InputTokens:  inputTokens,
							OutputTokens: ev.Usage.OutputTokens,
						},
					}
				}
				if ev.Delta != nil {
					switch ev.Delta.StopReason {
					case "end_turn":
						ch <- engine.StreamEvent{FinishReason: "stop"}
					case "tool_use":
						ch <- engine.StreamEvent{FinishReason: "tool_calls"}
					case "max_tokens":
						ch <- engine.StreamEvent{FinishReason: "length"}
					default:
						ch <- engine.StreamEvent{FinishReason: ev.Delta.StopReason}
					}
				}

			case ev.Type == "error":
				ch <- engine.StreamEvent{
					Error: &engine.StreamError{
						Code:    500,
						Message: ev.Error.Message,
					},
				}
			}
		}
	}()
	return ch
}

// SerializeStream writes StreamEvents as Anthropic SSE to the writer.
func (s *StreamAdapter) SerializeStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error {
	var thinkingIndex int
	var inThinking bool
	var inText bool
	var blockIndex int
	// pendingUsage buffers a Usage event so it rides the message_delta frame
	// (where Anthropic clients read token usage). If usage arrives after the
	// finish frame was already written, a standalone message_delta carries it.
	var pendingUsage *engine.StreamUsage
	finishWritten := false
	// toolBlock maps the event's tool-call index (upstream numbering) to the
	// Anthropic content-block index assigned at ToolCallStart, so deltas and
	// stops land on the same block even when text/thinking blocks precede
	// tool calls or multiple tool calls occur in one turn.
	toolBlock := make(map[int]int)
	// emit writes one SSE frame in Anthropic's wire shape: an `event:` line
	// naming the type, then the `data:` line. Strict SDK clients (Claude
	// Code) dispatch on the event line — bare data frames make them treat
	// the stream as malformed, abort it, and silently retry the request
	// non-streaming (caught live: every streamed turn was re-sent as JSON).
	emit := func(eventType, payload string) error {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
			return fmt.Errorf("anthropic serialize: %w", err)
		}
		return nil
	}
	// Anthropic streams MUST open with message_start (the SDK's message
	// accumulator is seeded by it). The upstream envelope was consumed by
	// ParseStream, so synthesize one; input tokens ride the closing
	// message_delta's usage instead.
	started := false
	ensureStarted := func() error {
		if started {
			return nil
		}
		started = true
		if err := emit("message_start",
			`{"type":"message_start","message":{"id":"msg_torana_stream","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		); err != nil {
			return err
		}
		return emit("ping", `{"type":"ping"}`)
	}
	closeThinking := func() error {
		if !inThinking {
			return nil
		}
		inThinking = false
		blockIndex++
		return emit("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, blockIndex-1))
	}
	closeText := func() error {
		if !inText {
			return nil
		}
		inText = false
		blockIndex++
		return emit("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, blockIndex-1))
	}

	for ev := range events {
		if ev.Error == nil {
			if err := ensureStarted(); err != nil {
				return err
			}
		}
		switch {
		case ev.ThinkingDelta != nil:
			if !inThinking {
				inThinking = true
				if err := emit("content_block_start", fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`,
					thinkingIndex,
				)); err != nil {
					return err
				}
			}
			if err := emit("content_block_delta", fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`,
				thinkingIndex,
				jsonString(*ev.ThinkingDelta),
			)); err != nil {
				return err
			}

		case ev.TextDelta != nil:
			if err := closeThinking(); err != nil {
				return err
			}
			if !inText {
				inText = true
				if err := emit("content_block_start", fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`,
					blockIndex,
				)); err != nil {
					return err
				}
			}
			if err := emit("content_block_delta", fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`,
				blockIndex,
				jsonString(*ev.TextDelta),
			)); err != nil {
				return err
			}

		case ev.ToolCallStart != nil:
			if err := closeThinking(); err != nil {
				return err
			}
			if err := closeText(); err != nil {
				return err
			}
			toolBlock[ev.ToolCallStart.Index] = blockIndex
			data := fmt.Sprintf(
				`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
				blockIndex,
				jsonString(ev.ToolCallStart.ID),
				jsonString(ev.ToolCallStart.Name),
			)
			blockIndex++
			if err := emit("content_block_start", data); err != nil {
				return err
			}

		case ev.ToolCallDelta != nil:
			idx, ok := toolBlock[ev.ToolCallDelta.Index]
			if !ok {
				idx = ev.ToolCallDelta.Index
			}
			if err := emit("content_block_delta", fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
				idx,
				jsonString(ev.ToolCallDelta.ArgumentsDelta),
			)); err != nil {
				return err
			}

		case ev.ToolCallEnd != nil:
			idx, ok := toolBlock[ev.ToolCallEnd.Index]
			if !ok {
				idx = ev.ToolCallEnd.Index
			}
			delete(toolBlock, ev.ToolCallEnd.Index)
			if err := emit("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx)); err != nil {
				return err
			}

		case ev.Usage != nil:
			if finishWritten {
				// Finish frame already out — emit usage on its own delta frame.
				if err := emit("message_delta", fmt.Sprintf(
					`{"type":"message_delta","delta":{},"usage":%s}`,
					usageJSON(ev.Usage),
				)); err != nil {
					return err
				}
			} else {
				pendingUsage = ev.Usage
			}

		case ev.FinishReason != "":
			stopReason := "end_turn"
			if ev.FinishReason == "tool_calls" {
				stopReason = "tool_use"
			}
			usageField := ""
			if pendingUsage != nil {
				usageField = fmt.Sprintf(`,"usage":%s`, usageJSON(pendingUsage))
				pendingUsage = nil
			}
			finishWritten = true
			if err := emit("message_delta", fmt.Sprintf(
				`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null}%s}`,
				jsonString(stopReason),
				usageField,
			)); err != nil {
				return err
			}

		case ev.Error != nil:
			if err := emit("error", fmt.Sprintf(
				`{"type":"error","error":{"type":"stream_error","message":%s}}`,
				jsonString(ev.Error.Message),
			)); err != nil {
				return err
			}
		}
	}

	// An empty event stream still yields a valid message envelope.
	if err := ensureStarted(); err != nil {
		return err
	}

	// Close any open thinking block before message_stop.
	if err := closeThinking(); err != nil {
		return err
	}

	// Close any open text block before message_stop.
	if err := closeText(); err != nil {
		return err
	}

	// Usage seen but no finish frame followed — don't drop it.
	if pendingUsage != nil {
		if err := emit("message_delta", fmt.Sprintf(
			`{"type":"message_delta","delta":{},"usage":%s}`,
			usageJSON(pendingUsage),
		)); err != nil {
			return err
		}
	}

	// Always send message_stop after all events.
	return emit("message_stop", `{"type":"message_stop"}`)
}

// jsonString returns a JSON-encoded string (with quotes).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// usageJSON renders a StreamUsage in Anthropic's usage shape.
func usageJSON(u *engine.StreamUsage) string {
	return fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d}`, u.InputTokens, u.OutputTokens)
}
