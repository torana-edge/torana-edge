package bedrock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// Stream implements format.StreamAdapter for Bedrock ConverseStream.
// Bedrock emits one JSON object per line (not standard SSE with "data:" prefix).
type Stream struct{}

// --- Wire types for ConverseStream events ---

type bedrockStreamEvent struct {
	MessageStart      *bedrockMessageStart      `json:"messageStart,omitempty"`
	ContentBlockStart *bedrockContentBlockStart `json:"contentBlockStart,omitempty"`
	ContentBlockDelta *bedrockContentBlockDelta `json:"contentBlockDelta,omitempty"`
	ContentBlockStop  *bedrockContentBlockStop  `json:"contentBlockStop,omitempty"`
	MessageStop       *bedrockMessageStop       `json:"messageStop,omitempty"`
	// Error responses
	Type  string `json:"__type,omitempty"`
	Error *bedrockError `json:"error,omitempty"`
}

type bedrockMessageStart struct {
	Role string `json:"role"`
}

type bedrockContentBlockStart struct {
	ContentBlockIndex int                    `json:"contentBlockIndex"`
	Start             bedrockContentBlockStartField `json:"start"`
}

type bedrockContentBlockStartField struct {
	ToolUse  *bedrockToolUseStart  `json:"toolUse,omitempty"`
	Thinking *bedrockThinkingStart `json:"thinking,omitempty"`
}

type bedrockToolUseStart struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
}

type bedrockThinkingStart struct{}

type bedrockContentBlockDelta struct {
	ContentBlockIndex int                           `json:"contentBlockIndex"`
	Delta             bedrockContentBlockDeltaField `json:"delta"`
}

type bedrockContentBlockDeltaField struct {
	Text      *string              `json:"text,omitempty"`
	ToolUse   *bedrockToolUseDelta `json:"toolUse,omitempty"`
	Thinking  *string              `json:"thinking,omitempty"`
	Signature *string              `json:"signature,omitempty"`
}

type bedrockToolUseDelta struct {
	Input string `json:"input"`
}

type bedrockContentBlockStop struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
}

type bedrockMessageStop struct {
	StopReason string `json:"stopReason"`
}

type bedrockError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- ParseStream ---

func (s *Stream) ParseStream(body io.Reader) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(body)
		// Bedrock events can be larger than the default 64KB buffer for tool-heavy responses.
		scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

		var inThinking bool
		var signatureBuf string

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			// Bedrock ConverseStream sends raw JSON, not "data:"-prefixed SSE.
			// But be tolerant of "data:" prefix just in case.
			line = strings.TrimPrefix(line, "data:")
			line = strings.TrimSpace(line)
			if line == "" || line == "[DONE]" {
				continue
			}

			evt := parseBedrockEvent(line, &inThinking, &signatureBuf)
			if evt != nil {
				ch <- *evt
			}
		}

		// Scanner error
		if err := scanner.Err(); err != nil && err != io.EOF {
			ch <- engine.StreamEvent{
				Error: &engine.StreamError{
					Code:    0,
					Message: fmt.Sprintf("bedrock stream read error: %v", err),
				},
			}
		}
	}()
	return ch
}
// parseBedrockEvent parses a single Bedrock JSON event line into a StreamEvent.
// Returns nil for events that should be silently ignored (e.g. messageStart).
func parseBedrockEvent(line string, inThinking *bool, signatureBuf *string) *engine.StreamEvent {
	var se bedrockStreamEvent
	if err := json.Unmarshal([]byte(line), &se); err != nil {
		return &engine.StreamEvent{
			Error: &engine.StreamError{
				Code:    0,
				Message: fmt.Sprintf("bedrock stream parse error: %v", err),
			},
		}
	}

	switch {
	case se.Error != nil:
		return &engine.StreamEvent{
			Error: &engine.StreamError{
				Code:    se.Error.Code,
				Message: se.Error.Message,
			},
		}

	case se.MessageStart != nil:
		// messageStart is informational; ignore.

	case se.ContentBlockStart != nil && se.ContentBlockStart.Start.Thinking != nil:
		*inThinking = true
		// thinking block start is informational; no event emitted.

	case se.ContentBlockStart != nil && se.ContentBlockStart.Start.ToolUse != nil:
		tu := se.ContentBlockStart.Start.ToolUse
		return &engine.StreamEvent{
			ToolCallStart: &engine.ToolCallStart{
				Index: 0, // Bedrock has only one tool call per turn
				ID:    tu.ToolUseID,
				Name:  tu.Name,
			},
		}
	// text block start is informational (no content yet); ignore.

	case se.ContentBlockDelta != nil:
		delta := se.ContentBlockDelta.Delta
		switch {
		case delta.Thinking != nil:
			return &engine.StreamEvent{ThinkingDelta: delta.Thinking}
		case delta.Signature != nil:
			*signatureBuf += *delta.Signature
			return nil
		case delta.Text != nil:
			text := *delta.Text
			return &engine.StreamEvent{TextDelta: &text}
		case delta.ToolUse != nil:
			return &engine.StreamEvent{
				ToolCallDelta: &engine.ToolCallDelta{
					Index:          0,
					ArgumentsDelta: delta.ToolUse.Input,
				},
			}
		}

	case se.ContentBlockStop != nil:
		if *inThinking {
			*inThinking = false
			return nil // thinking block stop; no event to emit
		}
		return &engine.StreamEvent{
			ToolCallEnd: &engine.ToolCallEnd{Index: 0},
		}

	case se.MessageStop != nil:
		reason := mapBedrockStopReason(se.MessageStop.StopReason)
		return &engine.StreamEvent{FinishReason: reason}
	}

	return nil
}

// mapBedrockStopReason converts Bedrock stop reasons to canonical finish reasons.
func mapBedrockStopReason(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "content_filtered":
		return "error"
	default:
		return reason
	}
}

// --- SerializeStream ---

func (s *Stream) SerializeStream(w io.Writer, events <-chan engine.StreamEvent) error {
	bw := bufio.NewWriter(w)
	thinkingOpen := false

	closeThinking := func() error {
		if !thinkingOpen {
			return nil
		}
		thinkingOpen = false
		se := bedrockStreamEvent{
			ContentBlockStop: &bedrockContentBlockStop{ContentBlockIndex: 0},
		}
		b, _ := json.Marshal(se)
		_, err := bw.WriteString(string(b) + "\n")
		return err
	}

	for evt := range events {
		if evt.ThinkingDelta != nil {
			if !thinkingOpen {
				thinkingOpen = true
				// Emit content block start for thinking
				startEvt := bedrockStreamEvent{
					ContentBlockStart: &bedrockContentBlockStart{
						ContentBlockIndex: 0,
						Start: bedrockContentBlockStartField{
							Thinking: &bedrockThinkingStart{},
						},
					},
				}
				b, _ := json.Marshal(startEvt)
				if _, err := bw.WriteString(string(b) + "\n"); err != nil {
					return fmt.Errorf("bedrock serialize: %w", err)
				}
			}
			// Emit thinking delta
			se := bedrockStreamEvent{
				ContentBlockDelta: &bedrockContentBlockDelta{
					ContentBlockIndex: 0,
					Delta: bedrockContentBlockDeltaField{
						Thinking: evt.ThinkingDelta,
					},
				},
			}
			b, _ := json.Marshal(se)
			if _, err := bw.WriteString(string(b) + "\n"); err != nil {
				return fmt.Errorf("bedrock serialize: %w", err)
			}
			continue
		}

		// Close thinking block before non-thinking events
		if err := closeThinking(); err != nil {
			return fmt.Errorf("bedrock serialize: %w", err)
		}

		lines := marshalStreamEvent(evt)
		for _, line := range lines {
			if _, err := bw.WriteString(line); err != nil {
				return fmt.Errorf("bedrock serialize: %w", err)
			}
		}
	}

	// Close any open thinking block at end of stream
	if err := closeThinking(); err != nil {
		return fmt.Errorf("bedrock serialize: %w", err)
	}

	return bw.Flush()
}

// marshalStreamEvent converts a StreamEvent into one or more Bedrock ConverseStream JSON lines.
func marshalStreamEvent(evt engine.StreamEvent) []string {
	switch {
	case evt.Error != nil:
		se := bedrockStreamEvent{
			Error: &bedrockError{
				Code:    evt.Error.Code,
				Message: evt.Error.Message,
			},
		}
		b, _ := json.Marshal(se)
		return []string{string(b) + "\n"}

	case evt.TextDelta != nil:
		se := bedrockStreamEvent{
			ContentBlockDelta: &bedrockContentBlockDelta{
				ContentBlockIndex: 0,
				Delta: bedrockContentBlockDeltaField{
					Text: evt.TextDelta,
				},
			},
		}
		b, _ := json.Marshal(se)
		return []string{string(b) + "\n"}

	case evt.ToolCallStart != nil:
		se := bedrockStreamEvent{
			ContentBlockStart: &bedrockContentBlockStart{
				ContentBlockIndex: 0,
				Start: bedrockContentBlockStartField{
					ToolUse: &bedrockToolUseStart{
						ToolUseID: evt.ToolCallStart.ID,
						Name:      evt.ToolCallStart.Name,
					},
				},
			},
		}
		b, _ := json.Marshal(se)
		return []string{string(b) + "\n"}

	case evt.ToolCallDelta != nil:
		se := bedrockStreamEvent{
			ContentBlockDelta: &bedrockContentBlockDelta{
				ContentBlockIndex: 0,
				Delta: bedrockContentBlockDeltaField{
					ToolUse: &bedrockToolUseDelta{
						Input: evt.ToolCallDelta.ArgumentsDelta,
					},
				},
			},
		}
		b, _ := json.Marshal(se)
		return []string{string(b) + "\n"}

	case evt.ToolCallEnd != nil:
		se := bedrockStreamEvent{
			ContentBlockStop: &bedrockContentBlockStop{
				ContentBlockIndex: 0,
			},
		}
		b, _ := json.Marshal(se)
		return []string{string(b) + "\n"}

	case evt.FinishReason != "":
		reason := reverseBedrockStopReason(evt.FinishReason)
		se := bedrockStreamEvent{
			MessageStop: &bedrockMessageStop{
				StopReason: reason,
			},
		}
		b, _ := json.Marshal(se)
		return []string{string(b) + "\n"}
	}

	return nil
}

// reverseBedrockStopReason converts canonical finish reasons to Bedrock stop reasons.
func reverseBedrockStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}
