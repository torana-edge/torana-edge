package anthropic

import (
	"bufio"
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
	Error        *errorEv        `json:"error,omitempty"`
}

type contentBlockEv struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
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
	StopReason string `json:"stop_reason"`
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
func (s *StreamAdapter) SerializeStream(w io.Writer, events <-chan engine.StreamEvent) error {
	var thinkingIndex int
	var inThinking bool
	var textBlockIndex int // tracks text content block indices (#25)
	emit := func(line string) error {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("anthropic serialize: %w", err)
		}
		if _, err := fmt.Fprint(w, "\n"); err != nil {
			return fmt.Errorf("anthropic serialize: %w", err)
		}
		return nil
	}
	closeThinking := func() error {
		if !inThinking {
			return nil
		}
		inThinking = false
		thinkingIndex++
		return emit(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, thinkingIndex-1))
	}

	for ev := range events {
		switch {
		case ev.ThinkingDelta != nil:
			if !inThinking {
				inThinking = true
				if err := emit(fmt.Sprintf(
					`data: {"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`,
					thinkingIndex,
				)); err != nil {
					return err
				}
			}
			if err := emit(fmt.Sprintf(
				`data: {"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`,
				thinkingIndex,
				jsonString(*ev.ThinkingDelta),
			)); err != nil {
				return err
			}

		case ev.TextDelta != nil:
			if err := closeThinking(); err != nil {
				return err
			}
			textBlockIndex++
			data := fmt.Sprintf(
				`data: {"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`,
				textBlockIndex-1,
				jsonString(*ev.TextDelta),
			)
			if err := emit(data); err != nil {
				return err
			}

		case ev.ToolCallStart != nil:
			if err := closeThinking(); err != nil {
				return err
			}
			data := fmt.Sprintf(
				`data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s}}`,
				ev.ToolCallStart.Index,
				jsonString(ev.ToolCallStart.ID),
				jsonString(ev.ToolCallStart.Name),
			)
			if err := emit(data); err != nil {
				return err
			}

		case ev.ToolCallDelta != nil:
			data := fmt.Sprintf(
				`data: {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
				ev.ToolCallDelta.Index,
				jsonString(ev.ToolCallDelta.ArgumentsDelta),
			)
			if err := emit(data); err != nil {
				return err
			}

		case ev.ToolCallEnd != nil:
			data := fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, ev.ToolCallEnd.Index)
			if err := emit(data); err != nil {
				return err
			}

		case ev.FinishReason != "":
			stopReason := "end_turn"
			if ev.FinishReason == "tool_calls" {
				stopReason = "tool_use"
			}
			data := fmt.Sprintf(
				`data: {"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null}}`,
				jsonString(stopReason),
			)
			if err := emit(data); err != nil {
				return err
			}

		case ev.Error != nil:
			data := fmt.Sprintf(
				`data: {"type":"error","error":{"type":"stream_error","message":%s}}`,
				jsonString(ev.Error.Message),
			)
			if err := emit(data); err != nil {
				return err
			}
		}
	}

	// Close any open thinking block before message_stop.
	if err := closeThinking(); err != nil {
		return err
	}

	// Always send message_stop after all events.
	if _, err := fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n\n"); err != nil {
		return fmt.Errorf("anthropic serialize: %w", err)
	}
	return nil
}

// jsonString returns a JSON-encoded string (with quotes).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
