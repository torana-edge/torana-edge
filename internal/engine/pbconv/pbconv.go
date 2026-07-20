package pbconv

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/pkg/pb"
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
		msg := &pb.Message{
			Role:              string(m.Role),
			Content:           m.Content,
			Thinking:          m.Thinking,
			ThinkingSignature: m.ThinkingSignature,
			RedactedThinking:  m.RedactedThinking,
			ToolCallId:        m.ToolCallID,
			ToolName:          m.ToolName,
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
		out.Messages = append(out.Messages, msg)
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
		msg := engine.Message{
			Role:              engine.Role(m.Role),
			Content:           m.Content,
			Thinking:          m.Thinking,
			ThinkingSignature: m.ThinkingSignature,
			RedactedThinking:  m.RedactedThinking,
			ToolCallID:        m.ToolCallId,
			ToolName:          m.ToolName,
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
		out.Messages = append(out.Messages, msg)
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
		out.Event = &pb.StreamEvent_ToolCallStart{
			ToolCallStart: &pb.ToolCallStart{
				Index:     int32(e.ToolCallStart.Index),
				Id:        e.ToolCallStart.ID,
				Name:      e.ToolCallStart.Name,
				Signature: e.ToolCallStart.Signature,
			},
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
	} else if e.ToolCallEnd != nil {
		out.Event = &pb.StreamEvent_ToolCallEnd{
			ToolCallEnd: &pb.ToolCallEnd{Index: int32(e.ToolCallEnd.Index)},
		}
	} else if e.FinishReason != "" {
		out.Event = &pb.StreamEvent_FinishReason{FinishReason: e.FinishReason}
	} else if e.Usage != nil {
		out.Event = &pb.StreamEvent_Usage{
			Usage: &pb.StreamUsage{
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

func FromPBStreamEvent(e *pb.StreamEvent) *engine.StreamEvent {
	out := &engine.StreamEvent{}
	switch v := e.Event.(type) {
	case *pb.StreamEvent_TextDelta:
		out.TextDelta = &v.TextDelta
	case *pb.StreamEvent_ThinkingDelta:
		out.ThinkingDelta = &v.ThinkingDelta
	case *pb.StreamEvent_ToolCallStart:
		out.ToolCallStart = &engine.ToolCallStart{
			Index:     int(v.ToolCallStart.Index),
			ID:        v.ToolCallStart.Id,
			Name:      v.ToolCallStart.Name,
			Signature: v.ToolCallStart.Signature,
		}
	case *pb.StreamEvent_SignatureDelta:
		sig := v.SignatureDelta
		out.SignatureDelta = &sig
	case *pb.StreamEvent_ToolCallDelta:
		out.ToolCallDelta = &engine.ToolCallDelta{
			Index:          int(v.ToolCallDelta.Index),
			ArgumentsDelta: v.ToolCallDelta.ArgumentsDelta,
		}
	case *pb.StreamEvent_ToolCallEnd:
		out.ToolCallEnd = &engine.ToolCallEnd{
			Index: int(v.ToolCallEnd.Index),
		}
	case *pb.StreamEvent_FinishReason:
		out.FinishReason = v.FinishReason
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
	return out
}
