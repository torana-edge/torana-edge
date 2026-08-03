package pbconv

import (
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func toPBChatRequest(c *engine.ChatRequest) *pb.ChatRequest {
	if c == nil {
		return nil
	}
	out := &pb.ChatRequest{
		Model:         c.Model,
		Stream:        c.Stream,
		StopSequences: c.StopSequences,
	}

	if c.MaxTokens != nil {
		out.MaxTokens = new(int32)
		*out.MaxTokens = int32(*c.MaxTokens)
	}
	if c.Temperature != nil {
		out.Temperature = new(float64)
		*out.Temperature = *c.Temperature
	}
	if c.TopP != nil {
		out.TopP = new(float64)
		*out.TopP = *c.TopP
	}

	out.ProviderExtensionsJson = c.ProviderExtensions.Bytes()
	out.SafetySettingsJson = c.SafetySettings.Bytes()
	out.ToranaMetaJson = c.ToranaMeta.Bytes()

	for _, m := range c.Messages {
		out.Messages = append(out.Messages, toPBMessage(m))
	}

	for _, t := range c.Tools {
		td := &pb.ToolDef{
			Name:             t.Name,
			Description:      t.Description,
			ParametersJson:   t.Parameters.Bytes(),
			Strict:           t.Strict,
			CacheControlJson: t.CacheControl.Bytes(),
		}
		out.Tools = append(out.Tools, td)
	}

	return out
}

// FromPBChatRequest converts a canonical PB request into the engine form.
// It is ERROR-RETURNING: the required object fields (tool arguments and
// tool schemas) are reconstructed through the validating wrappers, so a PB
// carrying malformed JSON cannot silently become a nil/partial engine
// value. Every call site's input either came from ToPBChatRequest (validated
// bytes) or from a replacement that passed SDK ValidateReplacement, so the
// error path is the defensive backstop, not the normal flow.
func FromPBChatRequest(c *pb.ChatRequest) (*engine.ChatRequest, error) {
	if c == nil {
		return nil, nil
	}
	out := &engine.ChatRequest{
		Model:         c.Model,
		Stream:        c.Stream,
		StopSequences: c.StopSequences,
	}

	if c.MaxTokens != nil {
		val := int(*c.MaxTokens)
		out.MaxTokens = &val
	}
	if c.Temperature != nil {
		val := *c.Temperature
		out.Temperature = &val
	}
	if c.TopP != nil {
		val := *c.TopP
		out.TopP = &val
	}

	if len(c.ProviderExtensionsJson) > 0 {
		v, err := engine.ParseOptionalJSONObject(c.ProviderExtensionsJson)
		if err != nil {
			return nil, fmt.Errorf("pb provider_extensions_json: %w", err)
		}
		out.ProviderExtensions = v
	}
	if len(c.SafetySettingsJson) > 0 {
		v, err := engine.ParseOptionalJSONArray(c.SafetySettingsJson)
		if err != nil {
			return nil, fmt.Errorf("pb safety_settings_json: %w", err)
		}
		out.SafetySettings = v
	}
	if len(c.ToranaMetaJson) > 0 {
		v, err := engine.ParseOptionalJSONObject(c.ToranaMetaJson)
		if err != nil {
			return nil, fmt.Errorf("pb torana_meta_json: %w", err)
		}
		out.ToranaMeta = v
	}

	for i, m := range c.Messages {
		if m == nil {
			return nil, fmt.Errorf("pb messages[%d] is nil", i)
		}
		msg, err := fromPBMessage(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msg)
	}

	for i, t := range c.Tools {
		if t == nil {
			return nil, fmt.Errorf("pb tools[%d] is nil", i)
		}
		params, err := engine.ParseRequiredObjectOrEmpty(t.ParametersJson)
		if err != nil {
			return nil, fmt.Errorf("pb tool %q parameters_json: %w", t.Name, err)
		}
		td := engine.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
			Strict:      t.Strict,
		}
		if len(t.CacheControlJson) > 0 {
			cc, cerr := engine.ParseOptionalJSONObject(t.CacheControlJson)
			if cerr != nil {
				return nil, fmt.Errorf("pb tool %q cache_control_json: %w", t.Name, cerr)
			}
			td.CacheControl = cc
		}
		out.Tools = append(out.Tools, td)
	}

	return out, nil
}

func ToPBStreamEvent(e *engine.StreamEvent) *pb.StreamEvent {
	out := &pb.StreamEvent{}
	if e.TextDelta != nil {
		out.Event = &pb.StreamEvent_TextDelta{TextDelta: *e.TextDelta}
	} else if e.ThinkingDelta != nil {
		out.Event = &pb.StreamEvent_ThinkingDelta{ThinkingDelta: *e.ThinkingDelta}
	} else if e.ToolCallStart != nil {
		// v2 has no ToolCallStart: a tool call opens a content block like any
		// other content, so one sequence covers text, thinking and tools, and
		// the block index is what binds deltas and signatures to it.
		out.Event = &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{
				Index: int32(e.ToolCallStart.Index),
				Block: &pb.ContentBlockStart_ToolCall{ToolCall: &pb.ToolCallRef{
					Id:        e.ToolCallStart.ID,
					Name:      e.ToolCallStart.Name,
					Signature: e.ToolCallStart.Signature,
				}},
			},
		}
	} else if e.BlockStart != nil {
		// Explicit non-tool content blocks map to their matching v2 arm so the
		// wire carries the full block topology: every content block opens with
		// a start event naming its kind.
		switch e.BlockStart.Kind {
		case engine.BlockKindText:
			out.Event = &pb.StreamEvent_ContentBlockStart{
				ContentBlockStart: &pb.ContentBlockStart{
					Index: int32(e.BlockStart.Index),
					Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}},
				},
			}
		case engine.BlockKindThinking:
			out.Event = &pb.StreamEvent_ContentBlockStart{
				ContentBlockStart: &pb.ContentBlockStart{
					Index: int32(e.BlockStart.Index),
					Block: &pb.ContentBlockStart_Thinking{Thinking: &pb.ThinkingBlock{}},
				},
			}
		case engine.BlockKindProvider:
			// ProviderBlock.kind passes through VERBATIM: the ABI treats it as
			// topology the host must not change, so the engine IR carries the
			// provider's own kind string (BlockStart.ProviderKind) and this
			// conversion writes it out unchanged — never a manufactured
			// constant.
			out.Event = &pb.StreamEvent_ContentBlockStart{
				ContentBlockStart: &pb.ContentBlockStart{
					Index: int32(e.BlockStart.Index),
					Block: &pb.ContentBlockStart_Provider{Provider: &pb.ProviderBlock{Kind: e.BlockStart.ProviderKind}},
				},
			}
		default:
			// Unknown kind — drop rather than invent. Only well-formed engine
			// events reach the wire, so this is defensive.
			return out
		}
	} else if e.SignatureDelta != nil {
		out.Event = &pb.StreamEvent_SignatureDelta{SignatureDelta: *e.SignatureDelta}
	} else if e.ToolCallDelta != nil {
		out.Event = &pb.StreamEvent_ToolCallDelta{
			ToolCallDelta: &pb.ToolCallDelta{
				Index:          int32(e.ToolCallDelta.Index),
				ArgumentsDelta: e.ToolCallDelta.ArgumentsDelta,
			},
		}
	} else if e.BlockStop != nil {
		out.Event = &pb.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pb.ContentBlockStop{Index: int32(e.BlockStop.Index)},
		}
	} else if e.ToolCallEnd != nil {
		out.Event = &pb.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pb.ContentBlockStop{Index: int32(e.ToolCallEnd.Index)},
		}
	} else if e.FinishReason != "" {
		// v2 carries the finish reason on MessageStop rather than as a
		// standalone event, so the end of a message is one thing to observe.
		out.Event = &pb.StreamEvent_MessageStop{
			MessageStop: &pb.MessageStop{FinishReason: e.FinishReason},
		}
	} else if e.Usage != nil {
		out.Event = &pb.StreamEvent_Usage{
			Usage: &pb.Usage{
				InputTokens:      int32(e.Usage.InputTokens),
				OutputTokens:     int32(e.Usage.OutputTokens),
				CacheReadTokens:  int32(e.Usage.CacheReadTokens),
				CacheWriteTokens: int32(e.Usage.CacheWriteTokens),
			},
		}
	} else if e.Error != nil {
		out.Event = &pb.StreamEvent_Error{
			Error: &pb.StreamError{
				Code:    int32(e.Error.Code),
				Message: e.Error.Message,
			},
		}
	}
	return out
}

// BlockKindTracker records which content blocks are ACTUALLY open while a
// streamed message is converted pb → engine, so a ContentBlockStop — which
// carries only an index — converts back to the engine event that matches the
// block it closes: ToolCallEnd for tool blocks, BlockStop for
// text/thinking/provider blocks.
//
// The v2 wire binds a stop by index alone; the block kind lives on the start
// event. The v2 contract: non-tool blocks (text/thinking/provider) are
// exclusive — at most ONE may be open, and nothing else (not even a tool
// block) may be open alongside it; TOOL blocks may be open concurrently at
// distinct indexes (parallel tool calls ride the native protocols), but never
// while a non-tool block is open; and indexes are unique across the ENTIRE
// streamed message — never reused after a block closes. This tracker enforces
// all of it, so it never guesses: a start that violates the exclusivity rules
// is an error, a start whose index was already used in this message is an
// error even after its block closed, and a stop that does not name an open
// block of the kind it closes is an error.
//
// One response stream is ONE streamed message. A tracker is request-scoped
// (streams cross host calls) and never sees MessageStart, so it cannot infer
// a reset — indexes stay unique for the whole request. It is NOT safe for
// concurrent use.
type BlockKindTracker struct {
	// openNonTool is the one text/thinking/provider block currently open in
	// this message, if any. Non-tool blocks are exclusive: a start while one
	// is open is an error regardless of the new block's kind (including a
	// tool block), and no tool block may be open alongside it.
	openNonTool *openBlock
	// openTools records every tool block currently open, keyed by its block
	// index. Multiple tool blocks may be open at once (parallel tool calls),
	// each at a unique index; membership is what makes a stop a ToolCallEnd.
	openTools map[int]struct{}
	// seen records every index that has opened a block in this message.
	// Indexes are unique per message: a start whose index was already used —
	// even after its block closed — is invalid topology.
	seen map[int]struct{}
}

// openBlock is a non-tool content block open at an index, with the kind
// needed to lower a matching stop to BlockStop.
type openBlock struct {
	kind  blockKind
	index int
}

// blockKind is the kind of a content block recorded as open at an index.
type blockKind int

const (
	blockKindNone blockKind = iota
	blockKindText
	blockKindThinking
	blockKindProvider
)

func (k blockKind) String() string {
	switch k {
	case blockKindText:
		return "text"
	case blockKindThinking:
		return "thinking"
	case blockKindProvider:
		return "provider"
	default:
		return "unknown"
	}
}

// startNonTool validates and records a text/thinking/provider block start: it
// is exclusive — it may not collide with the open non-tool block, nor open
// while any tool block is open — and its index joins the seen set.
func (t *BlockKindTracker) startNonTool(idx int, kind blockKind) error {
	if t.openNonTool != nil {
		return fmt.Errorf("pbconv: content block start at index %d while a %s block at index %d is still open", idx, t.openNonTool.kind, t.openNonTool.index)
	}
	if len(t.openTools) > 0 {
		return fmt.Errorf("pbconv: content block start at index %d while %d tool block(s) are still open", idx, len(t.openTools))
	}
	t.openNonTool = &openBlock{kind: kind, index: idx}
	t.seen[idx] = struct{}{}
	return nil
}

// FromPBStreamEvent converts one event, resolving ContentBlockStop by the kind
// of the block this tracker recorded as open. It returns an error for topology
// the v2 ABI declares invalid: a non-tool start while a non-tool block or any
// tool block is open, a tool start while a non-tool block is open, a start
// whose index was already used in this message (even after its block closed),
// or a stop that does not name an open block of the kind it closes. The
// conversion boundary never fabricates a kind.
func (t *BlockKindTracker) FromPBStreamEvent(e *pb.StreamEvent) (*engine.StreamEvent, error) {
	out := &engine.StreamEvent{}
	switch v := e.Event.(type) {
	case *pb.StreamEvent_TextDelta:
		out.TextDelta = &v.TextDelta
	case *pb.StreamEvent_ThinkingDelta:
		out.ThinkingDelta = &v.ThinkingDelta
	case *pb.StreamEvent_ContentBlockStart:
		if v.ContentBlockStart == nil {
			return out, nil
		}
		cbs := v.ContentBlockStart
		idx := int(cbs.Index)
		if t.seen == nil {
			t.seen = make(map[int]struct{})
		}
		if _, used := t.seen[idx]; used {
			return nil, fmt.Errorf("pbconv: content block index %d reused within one streamed message", idx)
		}
		switch b := cbs.Block.(type) {
		case *pb.ContentBlockStart_ToolCall:
			if b.ToolCall == nil {
				return out, nil
			}
			// A tool block may open alongside other tool blocks (parallel
			// calls ride the native protocols), but never while a non-tool
			// block is open: the index-bound wire cannot interleave a
			// non-tool block with anything else.
			if t.openNonTool != nil {
				return nil, fmt.Errorf("pbconv: content block start at index %d while a %s block at index %d is still open", idx, t.openNonTool.kind, t.openNonTool.index)
			}
			if t.openTools == nil {
				t.openTools = make(map[int]struct{})
			}
			out.ToolCallStart = &engine.ToolCallStart{
				Index:     idx,
				ID:        b.ToolCall.Id,
				Name:      b.ToolCall.Name,
				Signature: b.ToolCall.Signature,
			}
			t.openTools[idx] = struct{}{}
			t.seen[idx] = struct{}{}
		case *pb.ContentBlockStart_Text:
			if b.Text == nil {
				return out, nil
			}
			if err := t.startNonTool(idx, blockKindText); err != nil {
				return nil, err
			}
			out.BlockStart = &engine.BlockStart{Index: idx, Kind: engine.BlockKindText}
		case *pb.ContentBlockStart_Thinking:
			if b.Thinking == nil {
				return out, nil
			}
			if err := t.startNonTool(idx, blockKindThinking); err != nil {
				return nil, err
			}
			out.BlockStart = &engine.BlockStart{Index: idx, Kind: engine.BlockKindThinking}
		case *pb.ContentBlockStart_Provider:
			if b.Provider == nil {
				return out, nil
			}
			if err := t.startNonTool(idx, blockKindProvider); err != nil {
				return nil, err
			}
			out.BlockStart = &engine.BlockStart{
				Index:        idx,
				Kind:         engine.BlockKindProvider,
				ProviderKind: b.Provider.Kind,
			}
		}
	case *pb.StreamEvent_SignatureDelta:
		sig := v.SignatureDelta
		out.SignatureDelta = &sig
	case *pb.StreamEvent_ToolCallDelta:
		out.ToolCallDelta = &engine.ToolCallDelta{
			Index:          int(v.ToolCallDelta.Index),
			ArgumentsDelta: v.ToolCallDelta.ArgumentsDelta,
		}
	case *pb.StreamEvent_ContentBlockStop:
		if v.ContentBlockStop == nil {
			return out, nil
		}
		idx := int(v.ContentBlockStop.Index)
		// A stop binds by index to the block it closes, kind-matched: a tool
		// index closes a tool block (ToolCallEnd), the open non-tool index
		// closes a non-tool block (BlockStop). Anything else — a stop that
		// names a non-tool index while a different block is open, or an index
		// with no open block at all — is unknown topology, never a guess.
		if t.openNonTool != nil {
			if idx != t.openNonTool.index {
				return nil, fmt.Errorf("pbconv: content block stop at index %d does not match the open %s block at index %d", idx, t.openNonTool.kind, t.openNonTool.index)
			}
			out.BlockStop = &engine.BlockStop{Index: idx}
			t.openNonTool = nil
			return out, nil
		}
		if _, ok := t.openTools[idx]; ok {
			out.ToolCallEnd = &engine.ToolCallEnd{Index: idx}
			delete(t.openTools, idx)
			return out, nil
		}
		return nil, fmt.Errorf("pbconv: content block stop at index %d has no open block (unknown, mismatched, or already-closed topology)", idx)
	case *pb.StreamEvent_MessageStop:
		out.FinishReason = v.MessageStop.FinishReason
	case *pb.StreamEvent_Usage:
		out.Usage = &engine.StreamUsage{
			InputTokens:      int(v.Usage.InputTokens),
			OutputTokens:     int(v.Usage.OutputTokens),
			CacheReadTokens:  int(v.Usage.CacheReadTokens),
			CacheWriteTokens: int(v.Usage.CacheWriteTokens),
		}
	case *pb.StreamEvent_Error:
		out.Error = &engine.StreamError{
			Code:    int(v.Error.Code),
			Message: v.Error.Message,
		}
	}
	return out, nil
}

// Message conversion belongs to the request side only. The response side has
// its own narrow shape (ResponseMessage), so duplicating the request mapping
// for it would claim fields the host cannot deliver on a response.

// toPBMessage projects the ordered message body one-for-one onto the shared
// pb/v2 shape. This is the REQUEST-PATH source of truth; the engine's
// conversation.go fingerprint projection is pinned byte-equal to it by the
// differential matrix.
func toPBMessage(m engine.Message) *pb.Message {
	msg := &pb.Message{Role: string(m.Role)}
	for _, b := range m.Blocks {
		rb := &pb.RequestBlock{}
		switch {
		case b.Text != nil:
			rb.Kind = &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{
				Text: b.Text.Text, Signature: b.Text.Signature,
				PartMetadataJson: b.Text.PartMetadataJson.Bytes(),
			}}
		case b.Thinking != nil:
			rb.Kind = &pb.RequestBlock_Thinking{Thinking: &pb.RequestThinkingBlock{
				Text: b.Thinking.Text, Signature: b.Thinking.Signature,
				PartMetadataJson: b.Thinking.PartMetadataJson.Bytes(),
			}}
		case b.RedactedThinking != nil:
			rb.Kind = &pb.RequestBlock_RedactedThinking{RedactedThinking: &pb.RequestRedactedThinkingBlock{Data: b.RedactedThinking.Data}}
		case b.ToolUse != nil:
			rb.Kind = &pb.RequestBlock_ToolUse{ToolUse: &pb.RequestToolUseBlock{
				Id:               b.ToolUse.ID,
				Name:             b.ToolUse.Name,
				ArgumentsJson:    b.ToolUse.Arguments.Bytes(),
				Signature:        b.ToolUse.Signature,
				PartMetadataJson: b.ToolUse.PartMetadataJson.Bytes(),
			}}
		case b.ToolResult != nil:
			tr := &pb.RequestToolResultBlock{
				ToolCallId:       b.ToolResult.ToolCallID,
				ToolName:         b.ToolResult.ToolName,
				PartMetadataJson: b.ToolResult.PartMetadataJson.Bytes(),
				Signature:        b.ToolResult.Signature,
			}
			if b.ToolResult.WillContinue != nil {
				tr.WillContinue = proto.Bool(*b.ToolResult.WillContinue)
			}
			if b.ToolResult.Scheduling != nil {
				tr.Scheduling = proto.String(*b.ToolResult.Scheduling)
			}
			for _, c := range b.ToolResult.Content {
				tcb := &pb.ToolResultContentBlock{}
				switch {
				case c.Unknown != nil:
					tcb.Kind = &pb.ToolResultContentBlock_Unknown{Unknown: &pb.ToolResultUnknownBlock{
						Kind: c.Unknown.Kind, PayloadJson: c.Unknown.Payload.Bytes(),
					}}
				case c.CacheBreakpoint != nil:
					tcb.Kind = &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{
						MarkerJson: c.CacheBreakpoint.Marker.Bytes(),
					}}
				default:
					tcb.Kind = &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: c.Text}}
				}
				tr.Content = append(tr.Content, tcb)
			}
			rb.Kind = &pb.RequestBlock_ToolResult{ToolResult: tr}
		case b.CacheBreakpoint != nil:
			rb.Kind = &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{
				MarkerJson: b.CacheBreakpoint.Marker.Bytes(),
			}}
		case b.Unknown != nil:
			rb.Kind = &pb.RequestBlock_Unknown{Unknown: &pb.RequestUnknownBlock{
				Kind:             b.Unknown.Kind,
				PayloadJson:      b.Unknown.Payload.Bytes(),
				PartMetadataJson: b.Unknown.PartMetadataJson.Bytes(),
				Signature:        b.Unknown.Signature,
			}}
		case b.TrailingSignature != nil:
			rb.Kind = &pb.RequestBlock_TrailingSignature{TrailingSignature: &pb.RequestTrailingSignatureBlock{
				Signature:        b.TrailingSignature.Signature,
				PartMetadataJson: b.TrailingSignature.PartMetadataJson.Bytes(),
			}}
		}
		msg.Blocks = append(msg.Blocks, rb)
	}
	return msg
}

// fromPBMessage converts the ordered body back. TOTAL over arbitrary pb
// graphs: nil blocks, typed-nil oneof arms, and malformed object bytes are
// indexed refusals, never panics, never silent empties. The wrapper
// reconstruction is the defensive backstop; validated inputs (host-produced
// or SDK-gated) never reach the error path.
func fromPBMessage(m *pb.Message) (engine.Message, error) {
	if m == nil {
		return engine.Message{}, fmt.Errorf("pb message is nil")
	}
	msg := engine.Message{Role: engine.Role(m.Role)}
	for j, b := range m.Blocks {
		if b == nil {
			return engine.Message{}, fmt.Errorf("pb message blocks[%d] is nil", j)
		}
		blk, err := fromPBBlock(b, fmt.Sprintf("pb message blocks[%d]", j))
		if err != nil {
			return engine.Message{}, err
		}
		msg.Blocks = append(msg.Blocks, blk)
	}
	return msg, nil
}

func fromPBBlock(b *pb.RequestBlock, what string) (engine.Block, error) {
	switch k := b.Kind.(type) {
	case *pb.RequestBlock_Text:
		if k.Text == nil {
			return engine.Block{}, fmt.Errorf("%s text arm is a typed nil", what)
		}
		pm, err := pbPartMetadata(k.Text.PartMetadataJson, what+".text")
		if err != nil {
			return engine.Block{}, err
		}
		return engine.Block{Text: &engine.TextBlock{Text: k.Text.Text, Signature: k.Text.Signature, PartMetadataJson: pm}}, nil
	case *pb.RequestBlock_Thinking:
		if k.Thinking == nil {
			return engine.Block{}, fmt.Errorf("%s thinking arm is a typed nil", what)
		}
		pm, err := pbPartMetadata(k.Thinking.PartMetadataJson, what+".thinking")
		if err != nil {
			return engine.Block{}, err
		}
		return engine.Block{Thinking: &engine.ThinkingBlock{Text: k.Thinking.Text, Signature: k.Thinking.Signature, PartMetadataJson: pm}}, nil
	case *pb.RequestBlock_RedactedThinking:
		if k.RedactedThinking == nil {
			return engine.Block{}, fmt.Errorf("%s redacted_thinking arm is a typed nil", what)
		}
		return engine.Block{RedactedThinking: &engine.RedactedThinkingBlock{Data: k.RedactedThinking.Data}}, nil
	case *pb.RequestBlock_ToolUse:
		if k.ToolUse == nil {
			return engine.Block{}, fmt.Errorf("%s tool_use arm is a typed nil", what)
		}
		args, err := engine.ParseRequiredObjectOrEmpty(k.ToolUse.ArgumentsJson)
		if err != nil {
			return engine.Block{}, fmt.Errorf("%s.tool_use arguments_json: %w", what, err)
		}
		pm, err := pbPartMetadata(k.ToolUse.PartMetadataJson, what+".tool_use")
		if err != nil {
			return engine.Block{}, err
		}
		return engine.Block{ToolUse: &engine.ToolUseBlock{
			ID: k.ToolUse.Id, Name: k.ToolUse.Name, Arguments: args, Signature: k.ToolUse.Signature,
			PartMetadataJson: pm,
		}}, nil
	case *pb.RequestBlock_ToolResult:
		if k.ToolResult == nil {
			return engine.Block{}, fmt.Errorf("%s tool_result arm is a typed nil", what)
		}
		pm, err := pbPartMetadata(k.ToolResult.PartMetadataJson, what+".tool_result")
		if err != nil {
			return engine.Block{}, err
		}
		tr := &engine.ToolResultBlock{
			ToolCallID:       k.ToolResult.ToolCallId,
			ToolName:         k.ToolResult.ToolName,
			PartMetadataJson: pm,
			Signature:        k.ToolResult.Signature,
		}
		if k.ToolResult.WillContinue != nil {
			v := k.ToolResult.GetWillContinue()
			tr.WillContinue = &v
		}
		if k.ToolResult.Scheduling != nil {
			v := k.ToolResult.GetScheduling()
			tr.Scheduling = &v
		}
		for c, cb := range k.ToolResult.Content {
			if cb == nil {
				return engine.Block{}, fmt.Errorf("%s.tool_result content[%d] is nil", what, c)
			}
			elem, err := fromPBToolResultContent(cb, fmt.Sprintf("%s.tool_result content[%d]", what, c))
			if err != nil {
				return engine.Block{}, err
			}
			tr.Content = append(tr.Content, elem)
		}
		return engine.Block{ToolResult: tr}, nil
	case *pb.RequestBlock_CacheBreakpoint:
		if k.CacheBreakpoint == nil {
			return engine.Block{}, fmt.Errorf("%s cache_breakpoint arm is a typed nil", what)
		}
		marker, err := engine.ParseRequiredJSONObject(k.CacheBreakpoint.MarkerJson)
		if err != nil {
			return engine.Block{}, fmt.Errorf("%s.cache_breakpoint marker_json: %w", what, err)
		}
		return engine.Block{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: marker}}, nil
	case *pb.RequestBlock_Unknown:
		if k.Unknown == nil {
			return engine.Block{}, fmt.Errorf("%s unknown arm is a typed nil", what)
		}
		payload, err := engine.ParseRequiredJSONObject(k.Unknown.PayloadJson)
		if err != nil {
			return engine.Block{}, fmt.Errorf("%s.unknown payload_json: %w", what, err)
		}
		pm, err := pbPartMetadata(k.Unknown.PartMetadataJson, what+".unknown")
		if err != nil {
			return engine.Block{}, err
		}
		return engine.Block{Unknown: &engine.UnknownBlock{
			Kind: k.Unknown.Kind, Payload: payload, PartMetadataJson: pm, Signature: k.Unknown.Signature,
		}}, nil
	case *pb.RequestBlock_TrailingSignature:
		if k.TrailingSignature == nil {
			return engine.Block{}, fmt.Errorf("%s trailing_signature arm is a typed nil", what)
		}
		pm, err := pbPartMetadata(k.TrailingSignature.PartMetadataJson, what+".trailing_signature")
		if err != nil {
			return engine.Block{}, err
		}
		return engine.Block{TrailingSignature: &engine.TrailingSignatureBlock{
			Signature: k.TrailingSignature.Signature, PartMetadataJson: pm,
		}}, nil
	default:
		return engine.Block{}, fmt.Errorf("%s has no kind arm", what)
	}
}

func fromPBToolResultContent(cb *pb.ToolResultContentBlock, what string) (engine.ToolResultContentBlock, error) {
	switch k := cb.Kind.(type) {
	case *pb.ToolResultContentBlock_Text:
		if k.Text == nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("%s text arm is a typed nil", what)
		}
		return engine.ToolResultContentBlock{Text: k.Text.Text}, nil
	case *pb.ToolResultContentBlock_Unknown:
		if k.Unknown == nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("%s unknown arm is a typed nil", what)
		}
		payload, err := engine.ParseRequiredJSONObject(k.Unknown.PayloadJson)
		if err != nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("%s.unknown payload_json: %w", what, err)
		}
		return engine.ToolResultContentBlock{Unknown: &engine.UnknownBlock{Kind: k.Unknown.Kind, Payload: payload}}, nil
	case *pb.ToolResultContentBlock_CacheBreakpoint:
		if k.CacheBreakpoint == nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("%s cache_breakpoint arm is a typed nil", what)
		}
		marker, err := engine.ParseRequiredJSONObject(k.CacheBreakpoint.MarkerJson)
		if err != nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("%s.cache_breakpoint marker_json: %w", what, err)
		}
		return engine.ToolResultContentBlock{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: marker}}, nil
	default:
		return engine.ToolResultContentBlock{}, fmt.Errorf("%s has no kind arm", what)
	}
}

func ToPBChatResponse(r *engine.ChatResponse) *pb.ChatResponse {
	if r == nil {
		return nil
	}
	out := &pb.ChatResponse{
		Model:          r.Model,
		Id:             r.ID,
		FinishReason:   r.FinishReason,
		UpstreamStatus: int32(r.UpstreamStatus),
		DurationMs:     r.DurationMS,
	}
	if r.Message != nil {
		out.Message = toPBResponseMessage(r.Message)
	}
	if r.Usage != nil {
		out.Usage = &pb.Usage{
			InputTokens:      int32(r.Usage.InputTokens),
			OutputTokens:     int32(r.Usage.OutputTokens),
			CacheReadTokens:  int32(r.Usage.CacheReadTokens),
			CacheWriteTokens: int32(r.Usage.CacheWriteTokens),
		}
	}
	if len(r.ProviderExtensions) > 0 {
		out.ProviderExtensionsJson, _ = json.Marshal(r.ProviderExtensions)
	}
	return out
}

func FromPBChatResponse(r *pb.ChatResponse) *engine.ChatResponse {
	if r == nil {
		return nil
	}
	out := &engine.ChatResponse{
		Model:          r.Model,
		ID:             r.Id,
		FinishReason:   r.FinishReason,
		UpstreamStatus: int(r.UpstreamStatus),
		DurationMS:     r.DurationMs,
	}
	if r.Message != nil {
		out.Message = fromPBResponseMessage(r.Message)
	}
	if r.Usage != nil {
		out.Usage = &engine.StreamUsage{
			InputTokens:      int(r.Usage.InputTokens),
			OutputTokens:     int(r.Usage.OutputTokens),
			CacheReadTokens:  int(r.Usage.CacheReadTokens),
			CacheWriteTokens: int(r.Usage.CacheWriteTokens),
		}
	}
	if len(r.ProviderExtensionsJson) > 0 {
		json.Unmarshal(r.ProviderExtensionsJson, &out.ProviderExtensions)
	}
	return out
}

// toPBResponseMessage maps the canonical response message onto the wire.
// ArgumentsJSON is copied, never decoded: the pb message outlives the engine
// value the caller may keep mutating (the apply path rewrites argsJSON in
// place), and aliasing would let that mutation change the accepted baseline.
// Content is a *string, which Go strings make safe to alias — nothing can
// mutate through it.
func toPBResponseMessage(m *engine.ResponseMessage) *pb.ResponseMessage {
	out := &pb.ResponseMessage{Content: m.Content}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &pb.ToolCall{
			Id:            tc.ID,
			Name:          tc.Name,
			ArgumentsJson: cloneBytes(tc.ArgumentsJSON),
			Signature:     tc.Signature,
		})
	}
	return out
}

// fromPBResponseMessage maps the wire response message back to the canonical
// shape. Bytes are copied in both directions: the pb message is decoded from
// bytes a guest produced, and the returned engine value outlives the pb
// message's ownership.
func fromPBResponseMessage(m *pb.ResponseMessage) *engine.ResponseMessage {
	out := &engine.ResponseMessage{Content: m.Content}
	for _, tc := range m.ToolCalls {
		if tc == nil {
			// Defensive: the SDK refuses nil tool calls in validated results,
			// but this conversion also runs on host-built inputs. A nil entry
			// would panic downstream; skip it rather than crash the process.
			continue
		}
		out.ToolCalls = append(out.ToolCalls, engine.ResponseToolCall{
			ID:            tc.Id,
			Name:          tc.Name,
			ArgumentsJSON: cloneBytes(tc.ArgumentsJson),
			Signature:     tc.Signature,
		})
	}
	return out
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// pbPartMetadata converts the pb bytes carrier to the engine object form
// (absent bytes -> absent object; present bytes must be a strict JSON
// object — the SDK validator already gates it, this is the defensive
// backstop).
func pbPartMetadata(raw []byte, what string) (engine.RequiredJSONObject, error) {
	if len(raw) == 0 {
		return engine.RequiredJSONObject{}, nil
	}
	obj, err := engine.ParseRequiredJSONObject(raw)
	if err != nil {
		return engine.RequiredJSONObject{}, fmt.Errorf("%s part_metadata_json: %w", what, err)
	}
	return obj, nil
}
