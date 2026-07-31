package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	openaifmt "github.com/torana-edge/torana-edge/internal/format/openai"
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
	// signature is the provider's opaque token over this call (Gemini and
	// Code Assist thoughtSignature). clearSignature removes it from the body.
	//
	// Without these the token was invisible to the pipeline: a plugin could
	// change a function call's name or arguments and the ORIGINAL signature
	// stayed in the outgoing JSON, describing content the provider never
	// signed. B2 would see an empty accepted signature and could neither
	// detect nor clear it.
	signature      string
	clearSignature func()
}

// invalidateSignature drops the provider token from BOTH the canonical state
// and the body.
//
// Canonical too, not just the body: a later hook reads tc.signature, and
// leaving it set would let run_after_response observe — and a future path
// propagate — a token that is already invalid.
//
// Called from every permitted mutation path. Clearing only in the
// after-response block meant a stream hook could rewrite a signed Gemini
// function call while the original thoughtSignature was still forwarded.
func (tc *toolCallRef) invalidateSignature() {
	if tc.signature == "" {
		return
	}
	tc.signature = ""
	if tc.clearSignature != nil {
		tc.clearSignature()
	}
}

// responseRefs is the format-independent mutable view of a JSON response.
type responseRefs struct {
	model      string
	content    string
	setContent func(string)
	toolCalls  []toolCallRef
	usage      *engine.StreamUsage // provider-reported token usage (read-only)
	// id and finishReason are OBSERVED host-owned facts. They were hard-coded
	// empty in the hook input, so a plugin received typed fields the provider
	// had actually supplied and saw nothing — the same hostile silence v2
	// exists to remove, just on the read side instead of the write side.
	id           string
	finishReason string
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
	case "gemini", "gemini-codeassist":
		return extractGemini(body)
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
// input/output key names. cacheReadKey/cacheWriteKey name the provider's
// cached-token counts within the same object (empty = not reported by this
// provider in flat form). Returns nil when absent or all-zero.
func usageFrom(body map[string]any, objKey, inKey, outKey, cacheReadKey, cacheWriteKey string) *engine.StreamUsage {
	obj, _ := body[objKey].(map[string]any)
	if obj == nil {
		return nil
	}
	u := &engine.StreamUsage{InputTokens: asInt(obj[inKey]), OutputTokens: asInt(obj[outKey])}
	if cacheReadKey != "" {
		u.CacheReadTokens = asInt(obj[cacheReadKey])
	}
	if cacheWriteKey != "" {
		u.CacheWriteTokens = asInt(obj[cacheWriteKey])
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 {
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

// extractOpenAI handles both OpenAI response shapes. Chat Completions carries
// the reply in choices[].message; the Responses API carries it in a flat
// output[] of typed items and names its token fields differently.
//
// Reading only the Chat shape meant a non-streaming Responses call was
// accounted at zero tokens and its content was invisible to response hooks —
// both silently, since an absent field is indistinguishable from a provider
// that reported nothing. The streaming path has understood the Responses shape
// all along, so the two paths disagreed depending only on whether the client
// asked for a stream.
func extractOpenAI(body map[string]any) responseRefs {
	usage, _ := body["usage"].(map[string]any)
	refs := responseRefs{
		model: asString(body["model"]),
		id:    asString(body["id"]),
		// Field names live in the openai format package, shared with the
		// streaming reader, so the two cannot drift apart again.
		usage: openaifmt.ReadUsage(usage),
	}

	if output, ok := body["output"].([]any); ok {
		extractResponsesOutput(&refs, output)
		return refs
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
			refs.finishReason = asString(choice["finish_reason"])
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

// extractResponsesOutput builds references into a Responses API output[] array.
// Items are typed and flat rather than nested under a choice: assistant text
// arrives as a "message" whose content is a list of output_text parts, and each
// tool call is its own "function_call" item.
func extractResponsesOutput(refs *responseRefs, output []any) {
	// Every output_text part, not just the first. A Responses reply routinely
	// carries several, and binding only the first left the rest read-only —
	// so a redaction plugin would report success having rewritten one
	// paragraph of a multi-part answer.
	var textParts []map[string]any

	for _, item := range output {
		it, _ := item.(map[string]any)
		if it == nil {
			continue
		}
		switch asString(it["type"]) {
		case "message":
			parts, _ := it["content"].([]any)
			for _, p := range parts {
				part, _ := p.(map[string]any)
				if part == nil || asString(part["type"]) != "output_text" {
					continue
				}
				textParts = append(textParts, part)
			}
		case "function_call": //nolint:dupl // distinct wire shape from the Chat path
			itRef := it
			// The Responses wire format carries arguments as a JSON STRING.
			// objArgs writes back a decoded object, which is the Chat shape —
			// so using it here produced a body the client cannot parse. It is
			// only reached when "arguments" is absent or not a string, and even
			// then the setter must write a string.
			args := "{}"
			if raw, ok := itRef["arguments"].(string); ok {
				args = raw
			} else if v, present := itRef["arguments"]; present && v != nil {
				if b, err := json.Marshal(v); err == nil {
					args = string(b)
				}
			}
			setArgs := func(s string) error {
				if !json.Valid([]byte(s)) {
					return fmt.Errorf("tool call arguments are not valid JSON")
				}
				itRef["arguments"] = s
				return nil
			}
			refs.toolCalls = append(refs.toolCalls, toolCallRef{
				id:       asString(itRef["call_id"]),
				name:     asString(itRef["name"]),
				argsJSON: args,
				setName:  func(s string) { itRef["name"] = s },
				setArgs:  setArgs,
			})
		}
	}
	if len(textParts) == 0 {
		return
	}
	// content is the joined text so a plugin sees the whole reply; setContent
	// writes the replacement into the first part and blanks the rest, which is
	// the only faithful way to express "this text is now that text" across a
	// list of parts.
	joined := make([]string, 0, len(textParts))
	for _, part := range textParts {
		joined = append(joined, asString(part["text"]))
	}
	refs.content = strings.Join(joined, "")
	parts := textParts
	refs.setContent = func(replacement string) {
		parts[0]["text"] = replacement
		for _, part := range parts[1:] {
			part["text"] = ""
		}
	}
}

func extractAnthropic(body map[string]any) responseRefs {
	refs := responseRefs{
		model:        asString(body["model"]),
		id:           asString(body["id"]),
		finishReason: asString(body["stop_reason"]),
		usage:        usageFrom(body, "usage", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"),
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
	refs := responseRefs{
		finishReason: asString(body["stopReason"]),
		usage:        usageFrom(body, "usage", "inputTokens", "outputTokens", "cacheReadInputTokens", "cacheWriteInputTokens"),
	}
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

// --- gemini: candidates[].content.parts[].{text | functionCall{name,args}} ---

func extractGemini(body map[string]any) responseRefs {
	// Code Assist (Antigravity CLI) wraps the GenerateContentResponse under
	// "response"; unwrap so extraction/writeback target the real fields. Maps
	// are references, so mutating the inner map still reflects in the outer
	// body the caller re-marshals.
	if inner, ok := body["response"].(map[string]any); ok {
		body = inner
	}
	refs := responseRefs{
		model: asString(body["modelVersion"]),
		usage: usageFrom(body, "usageMetadata", "promptTokenCount", "candidatesTokenCount", "cachedContentTokenCount", ""),
	}
	candidates, _ := body["candidates"].([]any)
	for ci, c := range candidates {
		cand, _ := c.(map[string]any)
		if ci == 0 {
			refs.finishReason = asString(cand["finishReason"])
		}
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
				sigPart := part
				argsJSON, setArgs := objArgs(fcRef, "args")
				refs.toolCalls = append(refs.toolCalls, toolCallRef{
					id:       asString(fc["id"]),
					name:     asString(fc["name"]),
					argsJSON: argsJSON,
					setName:  func(s string) { fcRef["name"] = s },
					setArgs:  setArgs,
					// thoughtSignature sits on the PART, beside functionCall.
					signature:      asString(sigPart["thoughtSignature"]),
					clearSignature: func() { delete(sigPart, "thoughtSignature") },
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
		rs.mergeUsage(refs.usage)
	}

	// --- 1. synthetic stream events per tool call --------------------------
	for ti := range refs.toolCalls {
		tc := &refs.toolCalls[ti]

		// Formats without tool-call IDs (bare gemini) get a synthetic one so
		// plugins can key their buffers; it never reaches the wire.
		syntheticID := tc.id
		if syntheticID == "" {
			syntheticID = fmt.Sprintf("torana_json_tc_%d", ti)
		}

		applyEvents := func(evs []engine.StreamEvent) error {
			for i := range evs {
				ev := &evs[i]
				signedContentChanged := false
				if ev.ToolCallStart != nil && ev.ToolCallStart.Name != "" && ev.ToolCallStart.Name != tc.name {
					tc.name = ev.ToolCallStart.Name
					tc.setName(tc.name)
					signedContentChanged = true
					modified = true
				}
				if ev.ToolCallDelta != nil && ev.ToolCallDelta.ArgumentsDelta != "" && ev.ToolCallDelta.ArgumentsDelta != tc.argsJSON {
					if err := tc.setArgs(ev.ToolCallDelta.ArgumentsDelta); err != nil {
						return err
					}
					tc.argsJSON = ev.ToolCallDelta.ArgumentsDelta
					signedContentChanged = true
					modified = true
				}
				// The stream hook is a permitted mutation path too, so it must
				// invalidate the token exactly as after-response does. It also
				// updates tc.name/tc.argsJSON in place, so the later
				// comparison would see the ALREADY-CHANGED values and clear
				// nothing.
				if signedContentChanged {
					tc.invalidateSignature()
					modified = true
				}
			}
			return nil
		}

		events := []engine.StreamEvent{
			{ToolCallStart: &engine.ToolCallStart{
				Index: ti, ID: syntheticID, Name: tc.name, Signature: tc.signature,
			}},
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
		assistant.ToolCalls = append(assistant.ToolCalls,
			engine.ToolCall{ID: tc.id, Name: tc.name, Arguments: args, Signature: tc.signature})
	}
	respChat.Messages = []engine.Message{assistant}

	// The non-streaming path is the one where a replacement CAN be applied:
	// the body has not been written yet.
	resp := rs.chatResponse(respChat.Model, refs.id, &assistant, refs.finishReason)
	after, err := pl.RunAfterResponse(ctx, reqID, resp, true)
	if err != nil {
		// Propagate. Swallowing it here with `err == nil &&` meant a
		// block-mode plugin that trapped on the response never reached
		// ModifyResponse's refusal path, so the provider's body was forwarded
		// anyway — the same fail-open, one layer further in.
		return bodyBytes, err
	}
	if after != nil && after.Message != nil {
		msg := *after.Message
		if msg.Content != refs.content && refs.setContent != nil {
			refs.setContent(msg.Content)
			modified = true
		}
		// Apply tool-call mutations back by position.
		if len(msg.ToolCalls) == len(refs.toolCalls) {
			for i := range msg.ToolCalls {
				tc := &refs.toolCalls[i]
				mut := msg.ToolCalls[i]
				// signedContentChanged drives clearing the provider token
				// below. A signature covers this call's name and arguments, so
				// leaving it in place after either changes ships a valid-looking
				// provider signature over content the provider never signed.
				signedContentChanged := false
				if mut.Name != "" && mut.Name != tc.name {
					tc.setName(mut.Name)
					signedContentChanged = true
					modified = true
				}
				if mut.Arguments != nil {
					if b, err := json.Marshal(mut.Arguments); err == nil && string(b) != tc.argsJSON {
						if tc.setArgs(string(b)) == nil {
							signedContentChanged = true
							modified = true
						}
					}
				}
				// A plugin can never ADD or REPLACE a signature — it cannot
				// mint one — so the only permitted transition is clearing an
				// existing token whose covered content changed.
				if signedContentChanged {
					tc.invalidateSignature()
					modified = true
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
