package bedrock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/streamio"
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
		scanner := streamio.NewScanner(body)

		// Index-aware open-block state, keyed by the wire contentBlockIndex:
		// ConverseStream indexes are unique per block within one message, and
		// parallel tool calls each own their index — a single in-tool bool
		// collapsed concurrent calls onto one scope. Thinking blocks are
		// tracked the same way so their stops resolve by index even with tool
		// blocks open.
		openThinking := make(map[int]struct{})
		openTools := make(map[int]struct{})
		// Per-block signature accumulation, keyed by the wire
		// contentBlockIndex: the ConverseStream signature delta may split the
		// provider token across several frames, and the engine ABI carries
		// only the COMPLETED token (a SignatureDelta), so fragments buffer
		// here and are emitted as ONE event at the block's stop.
		signatures := make(map[int]string)

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

			for _, evt := range parseBedrockEvent(line, openThinking, openTools, signatures) {
				ch <- evt
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

// parseBedrockEvent parses a single Bedrock JSON event line into StreamEvents.
// Returns nil for events that should be silently ignored (e.g. messageStart).
// openThinking/openTools track the blocks opened by contentBlockStart frames,
// keyed by wire contentBlockIndex, so tool events resolve to the index that
// opened them: a toolUse DELTA naming an index with no open tool block is an
// explicit error event (never silently reattached to another call). Stops
// resolve against the open sets first — a stop closing a tracked thinking or
// tool block is handled by its kind; a stop naming neither is a text block
// stop (or a stray stop), which has no engine event and is ignored, matching
// the wire's informational text blocks.
//
// Signed thinking emission: a thinking start emits BlockStart at the ORIGINAL
// wire index; thinking deltas stream out as ThinkingDelta events; signature
// fragments accumulate per index in signatures (the wire may split the token
// across deltas, and the ABI carries only the COMPLETED token). At the block's
// stop the parser emits the accumulated SignatureDelta — immediately BEFORE
// the BlockStop so it lands INSIDE the open block, where the stream verifier
// binds it — then the BlockStop at the same index. A signature delta naming
// an index with no open thinking block is an explicit error event.
func parseBedrockEvent(line string, openThinking, openTools map[int]struct{}, signatures map[int]string) []engine.StreamEvent {
	var se bedrockStreamEvent
	if err := json.Unmarshal([]byte(line), &se); err != nil {
		return []engine.StreamEvent{{
			Error: &engine.StreamError{
				Code:    0,
				Message: fmt.Sprintf("bedrock stream parse error: %v", err),
			},
		}}
	}

	switch {
	case se.Error != nil:
		return []engine.StreamEvent{{
			Error: &engine.StreamError{
				Code:    se.Error.Code,
				Message: se.Error.Message,
			},
		}}

	case se.MessageStart != nil:
		// messageStart is informational; ignore.

	case se.ContentBlockStart != nil && se.ContentBlockStart.Start.Thinking != nil:
		// Thinking block start opens an explicit thinking block at the
		// ORIGINAL wire index — the pre-fix parser recorded only state and
		// emitted no BlockStart, dropping the topology the stream verifier
		// needs to bind the provider signature to this block.
		idx := se.ContentBlockStart.ContentBlockIndex
		openThinking[idx] = struct{}{}
		return []engine.StreamEvent{{
			BlockStart: &engine.BlockStart{Index: idx, Kind: engine.BlockKindThinking},
		}}

	case se.ContentBlockStart != nil && se.ContentBlockStart.Start.ToolUse != nil:
		tu := se.ContentBlockStart.Start.ToolUse
		idx := se.ContentBlockStart.ContentBlockIndex
		openTools[idx] = struct{}{}
		return []engine.StreamEvent{{
			ToolCallStart: &engine.ToolCallStart{
				Index: idx,
				ID:    tu.ToolUseID,
				Name:  tu.Name,
			},
		}}
	// text block start is informational (no content yet); ignore.

	case se.ContentBlockDelta != nil:
		delta := se.ContentBlockDelta.Delta
		switch {
		case delta.Thinking != nil:
			return []engine.StreamEvent{{ThinkingDelta: delta.Thinking}}
		case delta.Signature != nil:
			// The signature delta is only defined for an open thinking block
			// (the token belongs to that block). Fragments accumulate per
			// block; the COMPLETED token is emitted as a SignatureDelta at the
			// block's stop — a per-fragment event would carry an incomplete
			// token the ABI cannot represent.
			idx := se.ContentBlockDelta.ContentBlockIndex
			if _, thinking := openThinking[idx]; !thinking {
				return []engine.StreamEvent{{
					Error: &engine.StreamError{
						Code:    0,
						Message: fmt.Sprintf("bedrock: signature delta for unknown index %d", idx),
					},
				}}
			}
			signatures[idx] += *delta.Signature
			return nil
		case delta.Text != nil:
			text := *delta.Text
			return []engine.StreamEvent{{TextDelta: &text}}
		case delta.ToolUse != nil:
			idx := se.ContentBlockDelta.ContentBlockIndex
			if _, open := openTools[idx]; !open {
				return []engine.StreamEvent{{
					Error: &engine.StreamError{
						Code:    0,
						Message: fmt.Sprintf("bedrock: tool call delta for unknown index %d", idx),
					},
				}}
			}
			return []engine.StreamEvent{{
				ToolCallDelta: &engine.ToolCallDelta{
					Index:          idx,
					ArgumentsDelta: delta.ToolUse.Input,
				},
			}}
		}

	case se.ContentBlockStop != nil:
		idx := se.ContentBlockStop.ContentBlockIndex
		if _, thinking := openThinking[idx]; thinking {
			delete(openThinking, idx)
			// The block is complete. Emit the ACCUMULATED signature (one
			// SignatureDelta per completed token, matching the ABI) BEFORE the
			// stop so it lands inside the open block — the stream verifier
			// binds the signature to the explicit thinking block it belongs
			// to. An empty token is a clear marker: nothing to emit.
			sig := signatures[idx]
			delete(signatures, idx)
			if sig != "" {
				return []engine.StreamEvent{
					{SignatureDelta: &sig},
					{BlockStop: &engine.BlockStop{Index: idx}},
				}
			}
			return []engine.StreamEvent{{BlockStop: &engine.BlockStop{Index: idx}}}
		}
		if _, tool := openTools[idx]; tool {
			delete(openTools, idx)
			return []engine.StreamEvent{
				{ToolCallEnd: &engine.ToolCallEnd{Index: idx}},
			}
		}
		return nil // text block stop (or stray stop) — no tool call to end

	case se.MessageStop != nil:
		reason := mapBedrockStopReason(se.MessageStop.StopReason)
		return []engine.StreamEvent{{FinishReason: reason}}

	case se.Metadata != nil && se.Metadata.Usage != nil:
		u := se.Metadata.Usage
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			return []engine.StreamEvent{{
				Usage: &engine.StreamUsage{
					InputTokens:      u.InputTokens,
					OutputTokens:     u.OutputTokens,
					CacheReadTokens:  u.CacheReadInputTokens,
					CacheWriteTokens: u.CacheWriteInputTokens,
				},
			}}
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
	// openThinkingIndex is the wire contentBlockIndex of the thinking block
	// currently rendered on the wire; -1 when none is open. EVERY thinking
	// frame — start, delta, stop, and the signature-bearing delta — writes
	// THIS index, so a thinking block opened at a non-zero index keeps its
	// index on every frame (the pre-fix serializer hard-coded 0). It mirrors
	// blockTopology but is tracked separately because a bare ThinkingDelta
	// (legacy path, no explicit block event) may open the wire block too.
	openThinkingIndex := -1
	// openTools tracks the tool-call blocks opened by ToolCallStart events,
	// keyed by the engine block index, so deltas and stops lower to the wire
	// index that opened them. Parallel tool calls each own their index; a
	// delta/stop for an index that never started (or a duplicate start) is
	// malformed IR and errors instead of collapsing scopes.
	openTools := make(map[int]struct{})

	closeThinking := func() error {
		if openThinkingIndex < 0 {
			return nil
		}
		idx := openThinkingIndex
		openThinkingIndex = -1
		se := bedrockStreamEvent{
			ContentBlockStop: &bedrockContentBlockStop{ContentBlockIndex: idx},
		}
		b, _ := json.Marshal(se)
		_, err := bw.WriteString(string(b) + "\n")
		return err
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
				if openThinkingIndex < 0 {
					openThinkingIndex = evt.BlockStart.Index
					startEvt := bedrockStreamEvent{
						ContentBlockStart: &bedrockContentBlockStart{
							ContentBlockIndex: evt.BlockStart.Index,
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
			if openThinkingIndex < 0 {
				// Legacy bare-delta path: no explicit block event opened the
				// block, so open it on the wire at index 0 (the wire's first
				// content block) as before block events existed.
				openThinkingIndex = 0
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
			// Emit thinking delta on the open thinking block's index
			se := bedrockStreamEvent{
				ContentBlockDelta: &bedrockContentBlockDelta{
					ContentBlockIndex: openThinkingIndex,
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

		if evt.SignatureDelta != nil {
			// The signature rides INSIDE the open thinking block: the
			// ConverseStream signature delta frame is only defined for
			// thinking blocks and carries the block's index, so a signature
			// with no open thinking block has no legal wire slot — an
			// explicit error, never silently dropped or reattached to
			// another block (the pre-fix serializer discarded signatures
			// entirely). The block stays open; its BlockStop closes it.
			if openThinkingIndex < 0 {
				return fmt.Errorf("bedrock: signature delta with no open thinking block")
			}
			se := bedrockStreamEvent{
				ContentBlockDelta: &bedrockContentBlockDelta{
					ContentBlockIndex: openThinkingIndex,
					Delta: bedrockContentBlockDeltaField{
						Signature: evt.SignatureDelta,
					},
				},
			}
			b, _ := json.Marshal(se)
			if _, err := bw.WriteString(string(b) + "\n"); err != nil {
				return fmt.Errorf("bedrock serialize: %w", err)
			}
			continue
		}

		// Tool-call events are index-bound: validate against the open-tool set
		// BEFORE lowering, so a delta/stop for a never-started index or a
		// duplicate start errors explicitly. marshalStreamEvent renders the
		// engine index into the wire ContentBlockIndex.
		switch {
		case evt.ToolCallStart != nil:
			if _, dup := openTools[evt.ToolCallStart.Index]; dup {
				return fmt.Errorf("bedrock: duplicate tool call start at index %d", evt.ToolCallStart.Index)
			}
			openTools[evt.ToolCallStart.Index] = struct{}{}
		case evt.ToolCallDelta != nil:
			if _, open := openTools[evt.ToolCallDelta.Index]; !open {
				return fmt.Errorf("bedrock: tool call delta for unknown index %d", evt.ToolCallDelta.Index)
			}
		case evt.ToolCallEnd != nil:
			if _, open := openTools[evt.ToolCallEnd.Index]; !open {
				return fmt.Errorf("bedrock: tool call end for unknown index %d", evt.ToolCallEnd.Index)
			}
			delete(openTools, evt.ToolCallEnd.Index)
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
	// A closed channel is only a clean end when the serializer context remains
	// live. Avoid flushing/closing a partial Bedrock stream after cancellation.
	if err := ctx.Err(); err != nil {
		return err
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
				ContentBlockIndex: evt.ToolCallStart.Index,
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
				ContentBlockIndex: evt.ToolCallDelta.Index,
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
				ContentBlockIndex: evt.ToolCallEnd.Index,
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
