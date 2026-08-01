package pbconv

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func ToPBChatRequest(c *engine.ChatRequest) *pb.ChatRequest {
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

	if len(c.ProviderExtensions) > 0 {
		out.ProviderExtensionsJson, _ = json.Marshal(c.ProviderExtensions)
	}
	if len(c.SafetySettings) > 0 {
		out.SafetySettingsJson, _ = json.Marshal(c.SafetySettings)
	}
	if len(c.ToranaMeta) > 0 {
		out.ToranaMetaJson, _ = json.Marshal(c.ToranaMeta)
	}

	for _, m := range c.Messages {
		out.Messages = append(out.Messages, toPBMessage(m))
	}

	for _, t := range c.Tools {
		paramsJson, _ := json.Marshal(t.Parameters)
		td := &pb.ToolDef{
			Name:           t.Name,
			Description:    t.Description,
			ParametersJson: paramsJson,
			Strict:         t.Strict,
		}
		if len(t.CacheControl) > 0 {
			td.CacheControlJson, _ = json.Marshal(t.CacheControl)
		}
		out.Tools = append(out.Tools, td)
	}

	return out
}

func FromPBChatRequest(c *pb.ChatRequest) *engine.ChatRequest {
	if c == nil {
		return nil
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
		json.Unmarshal(c.ProviderExtensionsJson, &out.ProviderExtensions)
	}
	if len(c.SafetySettingsJson) > 0 {
		json.Unmarshal(c.SafetySettingsJson, &out.SafetySettings)
	}
	if len(c.ToranaMetaJson) > 0 {
		json.Unmarshal(c.ToranaMetaJson, &out.ToranaMeta)
	}

	for _, m := range c.Messages {
		out.Messages = append(out.Messages, fromPBMessage(m))
	}

	for _, t := range c.Tools {
		var params map[string]any
		if len(t.ParametersJson) > 0 {
			json.Unmarshal(t.ParametersJson, &params)
		}
		td := engine.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
			Strict:      t.Strict,
		}
		if len(t.CacheControlJson) > 0 {
			json.Unmarshal(t.CacheControlJson, &td.CacheControl)
		}
		out.Tools = append(out.Tools, td)
	}

	return out
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

// BlockKindTracker records which content block is ACTUALLY open at each index
// while a streamed message is converted pb → engine, so a ContentBlockStop —
// which carries only an index — converts back to the engine event that matches
// the block it closes: ToolCallEnd for tool blocks, BlockStop for
// text/thinking/provider blocks.
//
// The v2 wire binds a stop by index alone; the block kind lives on the start
// event. The v2 contract also makes a stop that does not name the currently
// open start INVALID, so this tracker never guesses: a stop with no open block
// at its index is an error, and a second start at an index that already has an
// open block (duplicate/reused topology) is an error. A start at a CLOSED index
// is legal — indexes are unique per streamed MESSAGE, and a later message in
// the same request may reuse them — so only the currently-open set is checked.
//
// A tracker is per streamed message (or per request, when the stream crosses
// host calls). It is NOT safe for concurrent use.
type BlockKindTracker struct {
	// open maps each currently-open block index to the kind of the block
	// opened there. Stops resolve by this actual kind; nothing is guessed.
	open map[int]blockKind
}

// blockKind is the kind of a content block recorded as open at an index.
type blockKind int

const (
	blockKindNone blockKind = iota
	blockKindTool
	blockKindText
	blockKindThinking
	blockKindProvider
)

func (k blockKind) String() string {
	switch k {
	case blockKindTool:
		return "tool"
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

// FromPBStreamEvent converts one event, resolving ContentBlockStop by the kind
// of the block this tracker recorded as open at its index. It returns an error
// for topology the v2 ABI declares invalid: a stop that names no open block
// (unknown, mismatched, or stop-after-close), or a start at an index where a
// block is already open (duplicate/reused). The conversion boundary never
// fabricates a kind.
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
		if t.open == nil {
			t.open = make(map[int]blockKind)
		}
		cbs := v.ContentBlockStart
		if kind, ok := t.open[int(cbs.Index)]; ok {
			return nil, fmt.Errorf("pbconv: duplicate content block start at index %d: a %s block is already open", cbs.Index, kind)
		}
		switch b := cbs.Block.(type) {
		case *pb.ContentBlockStart_ToolCall:
			if b.ToolCall != nil {
				out.ToolCallStart = &engine.ToolCallStart{
					Index:     int(cbs.Index),
					ID:        b.ToolCall.Id,
					Name:      b.ToolCall.Name,
					Signature: b.ToolCall.Signature,
				}
				t.open[int(cbs.Index)] = blockKindTool
			}
		case *pb.ContentBlockStart_Text:
			if b.Text != nil {
				out.BlockStart = &engine.BlockStart{Index: int(cbs.Index), Kind: engine.BlockKindText}
				t.open[int(cbs.Index)] = blockKindText
			}
		case *pb.ContentBlockStart_Thinking:
			if b.Thinking != nil {
				out.BlockStart = &engine.BlockStart{Index: int(cbs.Index), Kind: engine.BlockKindThinking}
				t.open[int(cbs.Index)] = blockKindThinking
			}
		case *pb.ContentBlockStart_Provider:
			if b.Provider != nil {
				out.BlockStart = &engine.BlockStart{
					Index:        int(cbs.Index),
					Kind:         engine.BlockKindProvider,
					ProviderKind: b.Provider.Kind,
				}
				t.open[int(cbs.Index)] = blockKindProvider
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
		kind, ok := t.open[idx]
		if !ok {
			return nil, fmt.Errorf("pbconv: content block stop at index %d has no open block (unknown, mismatched, or already-closed topology)", idx)
		}
		if kind == blockKindTool {
			out.ToolCallEnd = &engine.ToolCallEnd{Index: idx}
		} else {
			out.BlockStop = &engine.BlockStop{Index: idx}
		}
		delete(t.open, idx)
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

func toPBMessage(m engine.Message) *pb.Message {
	msg := &pb.Message{
		Role:              string(m.Role),
		Content:           m.Content,
		Thinking:          m.Thinking,
		ThinkingSignature: m.ThinkingSignature,
		RedactedThinking:  m.RedactedThinking,
		ToolCallId:        m.ToolCallID,
		ToolName:          m.ToolName,
		TrailingSignature: m.TrailingSignature,
		ContentSignature:  m.ContentSignature,
	}
	if len(m.ContentParts) > 0 {
		msg.ContentPartsJson, _ = json.Marshal(m.ContentParts)
	}
	if len(m.CacheControl) > 0 {
		msg.CacheControlJson, _ = json.Marshal(m.CacheControl)
	}
	for _, tc := range m.ToolCalls {
		argsJson, _ := json.Marshal(tc.Arguments)
		msg.ToolCalls = append(msg.ToolCalls, &pb.ToolCall{
			Id:            tc.ID,
			Name:          tc.Name,
			ArgumentsJson: argsJson,
			Signature:     tc.Signature,
		})
	}
	return msg
}

func fromPBMessage(m *pb.Message) engine.Message {
	if m == nil {
		return engine.Message{}
	}
	msg := engine.Message{
		Role:              engine.Role(m.Role),
		Content:           m.Content,
		Thinking:          m.Thinking,
		ThinkingSignature: m.ThinkingSignature,
		RedactedThinking:  m.RedactedThinking,
		ToolCallID:        m.ToolCallId,
		ToolName:          m.ToolName,
		TrailingSignature: m.TrailingSignature,
		ContentSignature:  m.ContentSignature,
	}
	if len(m.ContentPartsJson) > 0 {
		json.Unmarshal(m.ContentPartsJson, &msg.ContentParts)
	}
	if len(m.CacheControlJson) > 0 {
		json.Unmarshal(m.CacheControlJson, &msg.CacheControl)
	}
	for _, tc := range m.ToolCalls {
		var args map[string]any
		if len(tc.ArgumentsJson) > 0 {
			json.Unmarshal(tc.ArgumentsJson, &args)
		}
		msg.ToolCalls = append(msg.ToolCalls, engine.ToolCall{
			ID:        tc.Id,
			Name:      tc.Name,
			Arguments: args,
			Signature: tc.Signature,
		})
	}
	return msg
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
