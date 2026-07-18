package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/plugin"
)

// This file routes non-streaming JSON responses through the WASM plugin
// pipeline. The body is decoded into map[string]any and mutated in place, so
// every field the locators don't touch (id, model, usage, finish_reason,
// provider extras) survives byte-for-byte semantically — unlike re-marshaling
// a partial struct, which silently drops them.

// toolCallRef is a mutable view of one tool call inside a decoded response
// body. setName/setArgs write back into the underlying map tree.
type toolCallRef struct {
	id       string
	name     string
	argsJSON string
	setName  func(string)
	setArgs  func(string) error
}

// responseRefs is the format-independent mutable view of a JSON response.
type responseRefs struct {
	model      string
	content    string
	setContent func(string)
	toolCalls  []toolCallRef
	usage      *engine.StreamUsage // provider-reported token usage (read-only)
}

// extractResponse builds mutable references into a decoded response body for
// the given wire format. Unknown formats return no references (pass-through).
func extractResponse(formatName string, body map[string]any) responseRefs {
	switch formatName {
	case "openai":
		return extractOpenAI(body)
	case "anthropic":
		return extractAnthropic(body)
	case "bedrock":
		return extractBedrock(body)
	case "vertex":
		return extractVertex(body)
	}
	return responseRefs{}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int {
	f, _ := v.(float64) // JSON numbers decode as float64
	return int(f)
}

// usageFrom reads token counts from a decoded usage object under the given
// input/output key names. Returns nil when absent or all-zero.
func usageFrom(body map[string]any, objKey, inKey, outKey string) *engine.StreamUsage {
	obj, _ := body[objKey].(map[string]any)
	if obj == nil {
		return nil
	}
	u := &engine.StreamUsage{InputTokens: asInt(obj[inKey]), OutputTokens: asInt(obj[outKey])}
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	return u
}

// objArgs marshals an object-valued args field to JSON text and returns a
// setter that unmarshals mutated text back into the parent map at key.
func objArgs(parent map[string]any, key string) (string, func(string) error) {
	argsJSON := "{}"
	if v, ok := parent[key]; ok && v != nil {
		if b, err := json.Marshal(v); err == nil {
			argsJSON = string(b)
		}
	}
	return argsJSON, func(s string) error {
		var obj any
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return fmt.Errorf("args not valid JSON: %w", err)
		}
		parent[key] = obj
		return nil
	}
}

// --- openai: choices[].message.{content, tool_calls[].function.{name,arguments}} ---

func extractOpenAI(body map[string]any) responseRefs {
	refs := responseRefs{
		model: asString(body["model"]),
		usage: usageFrom(body, "usage", "prompt_tokens", "completion_tokens"),
	}
	choices, _ := body["choices"].([]any)
	for ci, c := range choices {
		choice, _ := c.(map[string]any)
		if choice == nil {
			continue
		}
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			continue
		}
		if ci == 0 {
			refs.content = asString(msg["content"])
			refs.setContent = func(s string) { msg["content"] = s }
		}
		toolCalls, _ := msg["tool_calls"].([]any)
		for _, t := range toolCalls {
			tc, _ := t.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			fnRef := fn
			refs.toolCalls = append(refs.toolCalls, toolCallRef{
				id:       asString(tc["id"]),
				name:     asString(fn["name"]),
				argsJSON: asString(fn["arguments"]), // JSON *string* on the wire
				setName:  func(s string) { fnRef["name"] = s },
				setArgs: func(s string) error {
					fnRef["arguments"] = s
					return nil
				},
			})
		}
	}
	return refs
}

// --- anthropic: content[] blocks (text | tool_use{id,name,input}) ---

func extractAnthropic(body map[string]any) responseRefs {
	refs := responseRefs{
		model: asString(body["model"]),
		usage: usageFrom(body, "usage", "input_tokens", "output_tokens"),
	}
	blocks, _ := body["content"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block == nil {
			continue
		}
		switch asString(block["type"]) {
		case "text":
			if refs.setContent == nil {
				blockRef := block
				refs.content = asString(block["text"])
				refs.setContent = func(s string) { blockRef["text"] = s }
			}
		case "tool_use":
			blockRef := block
			argsJSON, setArgs := objArgs(blockRef, "input")
			refs.toolCalls = append(refs.toolCalls, toolCallRef{
				id:       asString(block["id"]),
				name:     asString(block["name"]),
				argsJSON: argsJSON,
				setName:  func(s string) { blockRef["name"] = s },
				setArgs:  setArgs,
			})
		}
	}
	return refs
}

// --- bedrock: output.message.content[].{text | toolUse{toolUseId,name,input}} ---

func extractBedrock(body map[string]any) responseRefs {
	refs := responseRefs{usage: usageFrom(body, "usage", "inputTokens", "outputTokens")}
	output, _ := body["output"].(map[string]any)
	msg, _ := output["message"].(map[string]any)
	parts, _ := msg["content"].([]any)
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if part == nil {
			continue
		}
		if _, ok := part["text"]; ok && refs.setContent == nil {
			partRef := part
			refs.content = asString(part["text"])
			refs.setContent = func(s string) { partRef["text"] = s }
		}
		if tu, ok := part["toolUse"].(map[string]any); ok {
			tuRef := tu
			argsJSON, setArgs := objArgs(tuRef, "input")
			refs.toolCalls = append(refs.toolCalls, toolCallRef{
				id:       asString(tu["toolUseId"]),
				name:     asString(tu["name"]),
				argsJSON: argsJSON,
				setName:  func(s string) { tuRef["name"] = s },
				setArgs:  setArgs,
			})
		}
	}
	return refs
}

// --- vertex: candidates[].content.parts[].{text | functionCall{name,args}} ---

func extractVertex(body map[string]any) responseRefs {
	// Code Assist (Antigravity CLI) wraps the GenerateContentResponse under
	// "response"; unwrap so extraction/writeback target the real fields. Maps
	// are references, so mutating the inner map still reflects in the outer
	// body the caller re-marshals.
	if inner, ok := body["response"].(map[string]any); ok {
		body = inner
	}
	refs := responseRefs{
		model: asString(body["modelVersion"]),
		usage: usageFrom(body, "usageMetadata", "promptTokenCount", "candidatesTokenCount"),
	}
	candidates, _ := body["candidates"].([]any)
	for _, c := range candidates {
		cand, _ := c.(map[string]any)
		content, _ := cand["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if part == nil {
				continue
			}
			if _, ok := part["text"]; ok && refs.setContent == nil {
				partRef := part
				refs.content = asString(part["text"])
				refs.setContent = func(s string) { partRef["text"] = s }
			}
			if fc, ok := part["functionCall"].(map[string]any); ok {
				fcRef := fc
				argsJSON, setArgs := objArgs(fcRef, "args")
				refs.toolCalls = append(refs.toolCalls, toolCallRef{
					name:     asString(fc["name"]),
					argsJSON: argsJSON,
					setName:  func(s string) { fcRef["name"] = s },
					setArgs:  setArgs,
				})
			}
		}
	}
	return refs
}

// runJSONResponseHooks routes a non-streaming JSON response body through the
// WASM plugin pipeline for any provider format:
//
//  1. Tool calls are replayed as synthetic Start/Delta/End stream events
//     through run_on_stream_chunk — including events plugins emit in
//     response to ToolCallEnd (buffering plugins emit their processed
//     arguments there).
//  2. The assembled response is offered to run_after_response; assistant
//     content and tool-call name/args mutations are applied back.
//
// Only the located fields are written back; everything else in the body is
// preserved as decoded.
func runJSONResponseHooks(ctx context.Context, pl *plugin.PluginPipeline, reqID uint64, formatName string, chat *engine.ChatRequest, bodyBytes []byte) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		// Not JSON we understand — pass through, but never silently: an
		// unparseable 200 body means every response hook is being skipped
		// (a compressed body once leaked plugin-injected fields this way).
		log.Printf("json response: unparseable body (%v) — response hooks skipped", err)
		return bodyBytes, nil
	}

	refs := extractResponse(formatName, body)
	modified := false

	// Record provider-reported token usage for host metrics and _response.
	rs := reqStateFrom(ctx)
	if refs.usage != nil {
		rs.UsageIn, rs.UsageOut = refs.usage.InputTokens, refs.usage.OutputTokens
	}

	// --- 1. synthetic stream events per tool call --------------------------
	for ti := range refs.toolCalls {
		tc := &refs.toolCalls[ti]

		// Formats without tool-call IDs (vertex) get a synthetic one so
		// plugins can key their buffers; it never reaches the wire.
		syntheticID := tc.id
		if syntheticID == "" {
			syntheticID = fmt.Sprintf("torana_json_tc_%d", ti)
		}

		applyEvents := func(evs []engine.StreamEvent) error {
			for i := range evs {
				ev := &evs[i]
				if ev.ToolCallStart != nil && ev.ToolCallStart.Name != "" && ev.ToolCallStart.Name != tc.name {
					tc.name = ev.ToolCallStart.Name
					tc.setName(tc.name)
					modified = true
				}
				if ev.ToolCallDelta != nil && ev.ToolCallDelta.ArgumentsDelta != "" && ev.ToolCallDelta.ArgumentsDelta != tc.argsJSON {
					if err := tc.setArgs(ev.ToolCallDelta.ArgumentsDelta); err != nil {
						return err
					}
					tc.argsJSON = ev.ToolCallDelta.ArgumentsDelta
					modified = true
				}
			}
			return nil
		}

		events := []engine.StreamEvent{
			{ToolCallStart: &engine.ToolCallStart{Index: ti, ID: syntheticID, Name: tc.name}},
		}
		if tc.argsJSON != "" {
			events = append(events, engine.StreamEvent{
				ToolCallDelta: &engine.ToolCallDelta{Index: ti, ArgumentsDelta: tc.argsJSON},
			})
		}
		events = append(events, engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: ti}})

		for i := range events {
			out, err := pl.RunOnStreamChunk(ctx, reqID, &events[i])
			if err != nil {
				return bodyBytes, err
			}
			if err := applyEvents(out); err != nil {
				return bodyBytes, err
			}
		}
	}

	// --- 2. run_after_response ---------------------------------------------
	respChat := &engine.ChatRequest{Model: refs.model}
	if chat != nil {
		respChat.ToranaMeta = chat.ToranaMeta
	}
	if respChat.ToranaMeta == nil {
		respChat.ToranaMeta = map[string]any{}
	}
	// Expose latency/status/usage to response hooks.
	respChat.ToranaMeta["_response"] = rs.responseMeta()
	assistant := engine.Message{Role: engine.RoleAssistant, Content: refs.content}
	for _, tc := range refs.toolCalls {
		var args map[string]any
		json.Unmarshal([]byte(tc.argsJSON), &args)
		assistant.ToolCalls = append(assistant.ToolCalls, engine.ToolCall{ID: tc.id, Name: tc.name, Arguments: args})
	}
	respChat.Messages = []engine.Message{assistant}

	after, err := pl.RunAfterResponse(ctx, reqID, respChat)
	if err == nil && after != nil && len(after.Messages) == 1 {
		msg := after.Messages[0]
		if msg.Content != refs.content && refs.setContent != nil {
			refs.setContent(msg.Content)
			modified = true
		}
		// Apply tool-call mutations back by position.
		if len(msg.ToolCalls) == len(refs.toolCalls) {
			for i := range msg.ToolCalls {
				tc := &refs.toolCalls[i]
				mut := msg.ToolCalls[i]
				if mut.Name != "" && mut.Name != tc.name {
					tc.setName(mut.Name)
					modified = true
				}
				if mut.Arguments != nil {
					if b, err := json.Marshal(mut.Arguments); err == nil && string(b) != tc.argsJSON {
						if tc.setArgs(string(b)) == nil {
							modified = true
						}
					}
				}
			}
		}
	}

	if !modified {
		return bodyBytes, nil
	}
	out, err := json.Marshal(body)
	if err != nil {
		return bodyBytes, err
	}
	return out, nil
}
