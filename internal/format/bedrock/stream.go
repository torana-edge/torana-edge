package bedrock

import (
	"bufio"
	"context"
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
	Metadata          *bedrockMetadata          `json:"metadata,omitempty"`
	// Error responses
	Type  string        `json:"__type,omitempty"`
	Error *bedrockError `json:"error,omitempty"`
}

// bedrockMetadata is the ConverseStream trailer carrying token usage.
// It arrives AFTER messageStop on the wire.
type bedrockMetadata struct {
	Usage *bedrockUsage `json:"usage,omitempty"`
}

type bedrockUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
	// Converse prompt-cache accounting (only present when cachePoints are in
	// play): read = served from cache, write = written this turn.
	CacheReadInputTokens  int `json:"cacheReadInputTokens,omitempty"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens,omitempty"`
}

type bedrockMessageStart struct {
	Role string `json:"role"`
}

type bedrockContentBlockStart struct {
	ContentBlockIndex int                           `json:"contentBlockIndex"`
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
		var inToolUse bool
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

			evt := parseBedrockEvent(line, &inThinking, &inToolUse, &signatureBuf)
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
func parseBedrockEvent(line string, inThinking *bool, inToolUse *bool, signatureBuf *string) *engine.StreamEvent {
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
		*inToolUse = true
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
			*inToolUse = true
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
		if !*inToolUse {
			return nil // text block stop — no tool call to end
		}
		*inToolUse = false
		return &engine.StreamEvent{
			ToolCallEnd: &engine.ToolCallEnd{Index: 0},
		}

	case se.MessageStop != nil:
		reason := mapBedrockStopReason(se.MessageStop.StopReason)
		return &engine.StreamEvent{FinishReason: reason}

	case se.Metadata != nil && se.Metadata.Usage != nil:
		u := se.Metadata.Usage
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			return &engine.StreamEvent{
				Usage: &engine.StreamUsage{
					InputTokens:      u.InputTokens,
					OutputTokens:     u.OutputTokens,
					CacheReadTokens:  u.CacheReadInputTokens,
					CacheWriteTokens: u.CacheWriteInputTokens,
				},
			}
		}
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

// blockTopology tracks the one content block open in the stream being
// serialized, enforcing the v2 ABI's single-open-block + unique-index
// discipline on explicit block events BEFORE they are lowered to the wire. A
// second start while a block is open (any index), a start whose index was
// already used in this message, or a stop that does not name the open block is
// malformed topology and errors — never silently accepted.
//
// Tool-call blocks ride the raw ToolCallStart/Delta/End events and do NOT go
// through this state; it exists only for the explicit text/thinking
// BlockStart/BlockStop events the host's verified IR emits.
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

func (s *Stream) SerializeStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error {
	bw := bufio.NewWriter(w)
	blocks := &blockTopology{prefix: "bedrock"}
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
		if evt.BlockStart != nil {
			// Explicit block events (plugin-emitted or relayed). Provider
			// blocks have no ConverseStream representation at all, so they
			// error rather than vanish. Text/thinking are canonical Torana
			// content: thinking blocks render faithfully as
			// contentBlockStart(thinking) frames — never cast — while text
			// blocks lower to the text delta path (the wire has no text
			// start/stop frames, so an empty text block produces no wire
			// content). Sequence discipline runs before lowering either.
			switch evt.BlockStart.Kind {
			case engine.BlockKindProvider:
				return fmt.Errorf("bedrock: provider block kind %q is not supported by this serializer", evt.BlockStart.ProviderKind)
			case engine.BlockKindText:
				if err := blocks.start(evt.BlockStart.Index, engine.BlockKindText); err != nil {
					return err
				}
			case engine.BlockKindThinking:
				if err := blocks.start(evt.BlockStart.Index, engine.BlockKindThinking); err != nil {
					return err
				}
				if !thinkingOpen {
					thinkingOpen = true
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
			}
			continue
		}

		if evt.BlockStop != nil {
			// Close the block the stop names: sequence-validated, never
			// silently accepted. A thinking stop renders a contentBlockStop
			// frame; a text stop has no frame on the ConverseStream wire.
			closed, err := blocks.stop(evt.BlockStop.Index)
			if err != nil {
				return err
			}
			if closed == engine.BlockKindThinking {
				if err := closeThinking(); err != nil {
					return fmt.Errorf("bedrock serialize: %w", err)
				}
			}
			continue
		}

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

	case evt.Usage != nil:
		se := bedrockStreamEvent{
			Metadata: &bedrockMetadata{
				Usage: &bedrockUsage{
					InputTokens:           evt.Usage.InputTokens,
					OutputTokens:          evt.Usage.OutputTokens,
					TotalTokens:           evt.Usage.InputTokens + evt.Usage.OutputTokens,
					CacheReadInputTokens:  evt.Usage.CacheReadTokens,
					CacheWriteInputTokens: evt.Usage.CacheWriteTokens,
				},
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
