package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
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
//
// toolCallRef covers the SELECTED response only (choice 0 / candidate 0); the
// wire positions of every args slot — selected or not — live in
// responseRefs.rawSlots, which the restore pass reads exclusively.
type toolCallRef struct {
	id       string
	name     string
	argsJSON string
	// argsChanged is set whenever setArgs ran (stream delta or after-response
	// replacement). For the call's raw slot it drives the restore decision:
	// unchanged slots are spliced back to the provider's verbatim bytes,
	// changed object slots carry the guest's argsJSON bytes instead.
	argsChanged bool
	setName     func(string)
	setArgs     func(string) error
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

// rawArgSlot records ONE provider-body tool-argument slot for the byte-restore
// pass. Unlike toolCallRef (the mutable view of SELECTED calls only), rawSlots
// covers every tool-argument slot on the wire, including unselected
// choices/candidates: those bytes would otherwise be re-encoded by the
// marshaled-map round-trip (sorted object keys, big integers rounded through
// float64) even though the pipeline never touched them.
type rawArgSlot struct {
	// path locates the args slot in the FINAL marshaled output (gemini
	// codeassist paths start with "response" for the wrapper key).
	path []any
	// rawArgs is the provider's VERBATIM bytes of the slot in the original
	// body, straight out of the wire with no decode/re-encode: for object
	// slots (anthropic input, bedrock toolUse.input, gemini
	// functionCall.args) the raw object bytes; for string slots (openai
	// function.arguments, Responses output item arguments) the full quoted
	// JSON string including quotes. nil when the slot is absent in the raw
	// body.
	rawArgs []byte
	// objSlot marks an object-valued args slot. On change the guest's new
	// bytes are spliced in; for string slots the re-marshaled map already
	// holds the new string, so no splice is needed.
	objSlot bool
	// call is the index into refs.toolCalls when this slot belongs to a
	// selected (mutable) call, or -1 for an unselected alternative.
	call int
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
	// hasMessage reports whether the provider response actually contains an
	// assistant turn (message/candidate/content blocks or tool calls). When
	// false, the pipeline hands run_after_response a ChatResponse with
	// Message=nil — never a fabricated empty assistant message.
	hasMessage bool
	model      string
	content    string
	setContent func(string)
	toolCalls  []toolCallRef
	// rawSlots lists every tool-argument slot in the body, selected or not,
	// so the restore pass can splice provider-verbatim bytes back for
	// everything the pipeline did not rewrite. Selected slots carry their
	// toolCallRef index in call; unselected alternatives carry -1.
	rawSlots []rawArgSlot
	usage    *engine.StreamUsage // provider-reported token usage (read-only)
	// id and finishReason are OBSERVED host-owned facts. They were hard-coded
	// empty in the hook input, so a plugin received typed fields the provider
	// had actually supplied and saw nothing — the same hostile silence current ABI
	// exists to remove, just on the read side instead of the write side.
	id           string
	finishReason string
}

// extractResponse builds mutable references into a decoded response body for
// the given wire format. Unknown formats return no references (pass-through).
//
// raw is the ORIGINAL body bytes before any mutation; it lets the extractors
// capture provider-verbatim args spans instead of the lossy decoded map. It is
// variadic so call sites that only meter usage (no raw bytes on hand) keep
// working: with no raw, extractors fall back to map-derived args exactly as
// before.
func extractResponse(formatName string, body map[string]any, raw ...[]byte) responseRefs {
	var rb []byte
	if len(raw) > 0 {
		rb = raw[0]
	}
	switch formatName {
	case "openai":
		return extractOpenAI(body, rb)
	case "anthropic":
		return extractAnthropic(body, rb)
	case "bedrock":
		return extractBedrock(body, rb)
	case "gemini", "gemini-codeassist":
		return extractGemini(body, rb)
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
// provider in flat form). Returns nil only when the usage object is absent;
// an explicitly present all-zero object remains a report.
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
	return u
}

// objArgsSetter returns a setter for an object-valued args field: it unmarshals
// mutated JSON text back into the parent map at key as a decoded object, so the
// writeback stays in this format's own wire shape (object slots).
func objArgsSetter(parent map[string]any, key string) func(string) error {
	return func(s string) error {
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
func extractOpenAI(body map[string]any, raw []byte) responseRefs {
	usage, _ := body["usage"].(map[string]any)
	refs := responseRefs{
		model: asString(body["model"]),
		id:    asString(body["id"]),
		// Field names live in the openai format package, shared with the
		// streaming reader, so the two cannot drift apart again.
		usage: openaifmt.ReadUsage(usage),
	}

	if output, ok := body["output"].([]any); ok {
		// Items are components of ONE response — keep aggregating all of them.
		refs.hasMessage = len(output) > 0
		extractResponsesOutput(&refs, output, raw)
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
			refs.hasMessage = true // choices[0].message is a non-nil map
			// Content slot ONLY when the content key is present AND a string.
			// Absent or JSON null means the provider sent no writable text
			// slot, and a plugin must not fabricate content presence.
			if v, present := msg["content"]; present && v != nil {
				if s, isStr := v.(string); isStr {
					refs.content = s
					refs.setContent = func(s string) { msg["content"] = s }
				}
			}
			refs.finishReason = asString(choice["finish_reason"])
		}
		// Tool-call slots are recorded for EVERY choice — an unselected
		// alternative must still be restored byte-for-byte after a mutation —
		// but only choice 0's calls are mutable: a second choice is an
		// alternative, not another turn, so mutating it would rewrite a
		// response the provider did not select.
		toolCalls, _ := msg["tool_calls"].([]any)
		for ti, t := range toolCalls {
			tc, _ := t.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			path := []any{"choices", ci, "message", "tool_calls", ti, "function", "arguments"}
			rawArgs, _ := rawJSONSpan(raw, path...)
			call := -1
			if ci == 0 {
				fnRef := fn
				// String slot: the canonical argsJSON is the DECODED inner
				// JSON text (the SDK requires arguments_json to be an object,
				// not a quoted string).
				argsJSON := asString(fn["arguments"])
				if rawArgs != nil {
					var inner string
					if err := json.Unmarshal(rawArgs, &inner); err == nil {
						argsJSON = inner
					}
				}
				refs.toolCalls = append(refs.toolCalls, toolCallRef{
					id:       asString(tc["id"]),
					name:     asString(fn["name"]),
					argsJSON: argsJSON,
					setName:  func(s string) { fnRef["name"] = s },
					setArgs: func(s string) error {
						fnRef["arguments"] = s
						return nil
					},
				})
				call = len(refs.toolCalls) - 1
			}
			refs.rawSlots = append(refs.rawSlots, rawArgSlot{
				path:    path,
				rawArgs: rawArgs,
				objSlot: false,
				call:    call,
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
func extractResponsesOutput(refs *responseRefs, output []any, raw []byte) {
	// Every output_text part, not just the first. A Responses reply routinely
	// carries several, and binding only the first left the rest read-only —
	// so a redaction plugin would report success having rewritten one
	// paragraph of a multi-part answer.
	var textParts []map[string]any

	for oi, item := range output {
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
			path := []any{"output", oi, "arguments"}
			rawArgs, _ := rawJSONSpan(raw, path...)
			// The Responses wire format carries arguments as a JSON STRING;
			// the canonical argsJSON is the DECODED inner JSON text (the SDK
			// requires arguments_json to be an object). The setter writes a
			// string back, so the re-marshaled body stays in this format's own
			// wire shape.
			args := "{}"
			if rawArgs != nil {
				var inner string
				if err := json.Unmarshal(rawArgs, &inner); err == nil {
					args = inner
				}
			} else if v, present := itRef["arguments"]; present && v != nil {
				if s, isStr := v.(string); isStr {
					args = s
				} else if b, err := json.Marshal(v); err == nil {
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
			refs.rawSlots = append(refs.rawSlots, rawArgSlot{
				path:    path,
				rawArgs: rawArgs,
				objSlot: false,
				call:    len(refs.toolCalls) - 1,
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

func extractAnthropic(body map[string]any, raw []byte) responseRefs {
	refs := responseRefs{
		model:        asString(body["model"]),
		id:           asString(body["id"]),
		finishReason: asString(body["stop_reason"]),
		usage:        usageFrom(body, "usage", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"),
	}
	blocks, _ := body["content"].([]any)
	refs.hasMessage = len(blocks) > 0
	for bi, b := range blocks {
		block, _ := b.(map[string]any)
		if block == nil {
			continue
		}
		switch asString(block["type"]) {
		case "text":
			// Content slot = first text block with a present string text key.
			if refs.setContent == nil {
				if s, isStr := block["text"].(string); isStr {
					blockRef := block
					refs.content = s
					refs.setContent = func(s string) { blockRef["text"] = s }
				}
			}
		case "tool_use":
			blockRef := block
			path := []any{"content", bi, "input"}
			rawArgs, _ := rawJSONSpan(raw, path...)
			argsJSON := "{}"
			if rawArgs != nil {
				argsJSON = string(rawArgs) // object slot: verbatim provider bytes
			} else if v, ok := blockRef["input"]; ok && v != nil {
				if b, err := json.Marshal(v); err == nil {
					argsJSON = string(b)
				}
			}
			refs.toolCalls = append(refs.toolCalls, toolCallRef{
				id:       asString(block["id"]),
				name:     asString(block["name"]),
				argsJSON: argsJSON,
				setName:  func(s string) { blockRef["name"] = s },
				setArgs:  objArgsSetter(blockRef, "input"),
			})
			refs.rawSlots = append(refs.rawSlots, rawArgSlot{
				path:    path,
				rawArgs: rawArgs,
				objSlot: true,
				call:    len(refs.toolCalls) - 1,
			})
		}
	}
	return refs
}

// --- bedrock: output.message.content[].{text | toolUse{toolUseId,name,input}} ---

func extractBedrock(body map[string]any, raw []byte) responseRefs {
	refs := responseRefs{
		finishReason: asString(body["stopReason"]),
		usage:        usageFrom(body, "usage", "inputTokens", "outputTokens", "cacheReadInputTokens", "cacheWriteInputTokens"),
	}
	output, _ := body["output"].(map[string]any)
	msg, _ := output["message"].(map[string]any)
	refs.hasMessage = msg != nil
	parts, _ := msg["content"].([]any)
	for pi, p := range parts {
		part, _ := p.(map[string]any)
		if part == nil {
			continue
		}
		// Content slot = first part with a present string text key.
		if s, isStr := part["text"].(string); isStr && refs.setContent == nil {
			partRef := part
			refs.content = s
			refs.setContent = func(s string) { partRef["text"] = s }
		}
		if tu, ok := part["toolUse"].(map[string]any); ok {
			tuRef := tu
			path := []any{"output", "message", "content", pi, "toolUse", "input"}
			rawArgs, _ := rawJSONSpan(raw, path...)
			argsJSON := "{}"
			if rawArgs != nil {
				argsJSON = string(rawArgs) // object slot: verbatim provider bytes
			} else if v, ok := tuRef["input"]; ok && v != nil {
				if b, err := json.Marshal(v); err == nil {
					argsJSON = string(b)
				}
			}
			refs.toolCalls = append(refs.toolCalls, toolCallRef{
				id:       asString(tu["toolUseId"]),
				name:     asString(tu["name"]),
				argsJSON: argsJSON,
				setName:  func(s string) { tuRef["name"] = s },
				setArgs:  objArgsSetter(tuRef, "input"),
			})
			refs.rawSlots = append(refs.rawSlots, rawArgSlot{
				path:    path,
				rawArgs: rawArgs,
				objSlot: true,
				call:    len(refs.toolCalls) - 1,
			})
		}
	}
	return refs
}

// --- gemini: candidates[].content.parts[].{text | functionCall{name,args}} ---

func extractGemini(body map[string]any, raw []byte) responseRefs {
	// Code Assist (Antigravity CLI) wraps the GenerateContentResponse under
	// "response"; unwrap so extraction/writeback target the real fields. Maps
	// are references, so mutating the inner map still reflects in the outer
	// body the caller re-marshals. The args paths gain the "response" prefix
	// so the splice still finds the slot in the wrapped output.
	prefix := []any{}
	if inner, ok := body["response"].(map[string]any); ok {
		body = inner
		prefix = []any{"response"}
	}
	refs := responseRefs{
		model: asString(body["modelVersion"]),
		usage: usageFrom(body, "usageMetadata", "promptTokenCount", "candidatesTokenCount", "cachedContentTokenCount", ""),
	}
	candidates, _ := body["candidates"].([]any)
	// hasMessage: a real candidate with a non-nil content map. An empty
	// candidates list (or one whose first entry has no content) means the
	// provider returned no assistant turn.
	if len(candidates) > 0 {
		if c0, ok := candidates[0].(map[string]any); ok {
			if content, ok := c0["content"].(map[string]any); ok {
				refs.hasMessage = content != nil
			}
		}
	}
	for ci, c := range candidates {
		cand, _ := c.(map[string]any)
		if cand == nil {
			continue
		}
		if ci == 0 {
			refs.finishReason = asString(cand["finishReason"])
		}
		content, _ := cand["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for pi, p := range parts {
			part, _ := p.(map[string]any)
			if part == nil {
				continue
			}
			// Content slot = first part of CANDIDATE 0 with a present string
			// text key. Candidate 0 is the selected response; a later
			// candidate is an alternative, not another turn, so its text must
			// be neither exposed as ResponseMessage.content nor mutated.
			if ci == 0 {
				if s, isStr := part["text"].(string); isStr && refs.setContent == nil {
					partRef := part
					refs.content = s
					refs.setContent = func(s string) { partRef["text"] = s }
				}
			}
			if fc, ok := part["functionCall"].(map[string]any); ok {
				path := make([]any, 0, len(prefix)+7)
				path = append(path, prefix...)
				path = append(path, "candidates", ci, "content", "parts", pi, "functionCall", "args")
				rawArgs, _ := rawJSONSpan(raw, path...)
				// Args slots are recorded for EVERY candidate — an unselected
				// alternative must still be restored byte-for-byte after a
				// mutation — but tool calls and signatures come from candidate
				// 0 ONLY: a second candidate is an alternative response, not
				// another turn, so its calls must never be replayed or mutated.
				call := -1
				if ci == 0 {
					fcRef := fc
					sigPart := part
					argsJSON := "{}"
					if rawArgs != nil {
						argsJSON = string(rawArgs) // object slot: verbatim provider bytes
					} else if v, ok := fcRef["args"]; ok && v != nil {
						if b, err := json.Marshal(v); err == nil {
							argsJSON = string(b)
						}
					}
					refs.toolCalls = append(refs.toolCalls, toolCallRef{
						id:       asString(fc["id"]),
						name:     asString(fc["name"]),
						argsJSON: argsJSON,
						setName:  func(s string) { fcRef["name"] = s },
						setArgs:  objArgsSetter(fcRef, "args"),
						// thoughtSignature sits on the PART, beside functionCall.
						signature:      asString(sigPart["thoughtSignature"]),
						clearSignature: func() { delete(sigPart, "thoughtSignature") },
					})
					call = len(refs.toolCalls) - 1
				}
				refs.rawSlots = append(refs.rawSlots, rawArgSlot{
					path:    path,
					rawArgs: rawArgs,
					objSlot: true,
					call:    call,
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

	// raw body bytes go to the extractors so they can capture provider-verbatim
	// args spans; re-marshaling the decoded map would lose them.
	refs := extractResponse(formatName, body, bodyBytes)
	modified := false

	// Record provider-reported token usage for host metrics and _response.
	rs := reqStateFrom(ctx)
	if rs == nil {
		return nil, fmt.Errorf("json response: request state unavailable")
	}
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
					tc.argsChanged = true
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
	if respChat.ToranaMeta.IsAbsent() {
		respChat.ToranaMeta, _ = engine.ParseOptionalJSONObject([]byte(`{}`))
	}
	// Expose latency/status/usage to response hooks (engine-side only;
	// ToPBChatResponse never serializes ToranaMeta).
	if v, err := json.Marshal(rs.responseMeta()); err == nil {
		respChat.ToranaMeta, _ = respChat.ToranaMeta.SetMember("_response", v)
	}

	// The canonical response preserves what the wire can prove: an assistant
	// message only when the provider response actually contains one (a
	// message/candidate/content block or tool calls), content presence only
	// when the body has a writable text slot, and raw arguments bytes with no
	// map[string]any round-trip between responseRefs and the wire. With no
	// message the hook receives ChatResponse{Message: nil} — never a
	// fabricated empty assistant turn.
	var assistant *engine.ResponseMessage
	if refs.hasMessage {
		assistant = &engine.ResponseMessage{}
		if refs.setContent != nil {
			content := refs.content
			assistant.Content = &content
		}
		for i := range refs.toolCalls {
			tc := &refs.toolCalls[i]
			assistant.ToolCalls = append(assistant.ToolCalls, engine.ResponseToolCall{
				ID:            tc.id,
				Name:          tc.name,
				ArgumentsJSON: []byte(tc.argsJSON),
				Signature:     tc.signature,
			})
		}
	}

	// The non-streaming path is the one where a replacement CAN be applied:
	// the body has not been written yet.
	resp := rs.chatResponse(respChat.Model, refs.id, assistant, refs.finishReason)
	after, err := pl.RunAfterResponse(ctx, reqID, resp, true)
	if err != nil {
		// Propagate. Swallowing it here with `err == nil &&` meant a
		// block-mode plugin that trapped on the response never reached
		// ModifyResponse's refusal path, so the provider's body was forwarded
		// anyway — the same fail-open, one layer further in.
		return bodyBytes, err
	}
	if after != nil && after.Message != nil {
		msg := after.Message
		// Defensive re-verification, last line of defense: the pipeline
		// rejects relative-policy violations per plugin before any
		// replacement is accepted, so these cannot fire on the accepted path —
		// but the apply boundary must fail loudly rather than half-apply, or
		// silently skip as v1 did.
		if (msg.Content != nil) != (refs.setContent != nil) {
			return bodyBytes, fmt.Errorf("response replacement changed content presence")
		}
		if len(msg.ToolCalls) != len(refs.toolCalls) {
			return bodyBytes, fmt.Errorf("response replacement changed tool-call cardinality: %d != %d",
				len(msg.ToolCalls), len(refs.toolCalls))
		}
		if msg.Content != nil && *msg.Content != refs.content && refs.setContent != nil {
			refs.setContent(*msg.Content)
			modified = true
		}
		// Apply tool-call mutations back by position. ID and Signature are
		// host-owned: the guest's values are never read.
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
			if !bytes.Equal(mut.ArgumentsJSON, []byte(tc.argsJSON)) {
				if err := tc.setArgs(string(mut.ArgumentsJSON)); err != nil {
					return bodyBytes, fmt.Errorf("response replacement tool call %d: %w", i, err)
				}
				tc.argsJSON = string(mut.ArgumentsJSON)
				tc.argsChanged = true
				signedContentChanged = true
				modified = true
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

	if !modified {
		// Nothing changed: the provider's bytes are already exactly what
		// should ship. No marshal, no splice — byte-for-byte identity.
		return bodyBytes, nil
	}
	out, err := json.Marshal(body)
	if err != nil {
		return bodyBytes, err
	}
	// Restore provider-verbatim argument bytes. Re-marshaling the decoded map
	// sorts object keys and rounds large integers through float64, so every
	// args slot the pipeline did NOT rewrite must be spliced back byte for
	// byte; rewritten OBJECT slots get the plugin's exact bytes spliced in
	// instead. Rewritten string slots need nothing — the map already holds
	// the new string and re-marshaling it is correct.
	//
	// rawSlots covers EVERY slot on the wire, including unselected
	// choices/candidates, so the restore cannot lose an alternative's bytes to
	// the re-encode. A slot with no raw span (no args key in the original
	// body) that was never changed is skipped BEFORE any lookup: a
	// content-only mutation must not fail because a call never had an args
	// slot. If a plugin genuinely changed such a call's arguments, the setter
	// created the slot in the decoded map and the lookup below is required.
	//
	// All spans are computed against the marshaled output FIRST, then spliced
	// in descending start-offset order, so earlier offsets stay valid no
	// matter where each slot lives.
	type spliceOp struct {
		start, end int
		repl       []byte
	}
	ops := make([]spliceOp, 0, len(refs.rawSlots))
	for _, slot := range refs.rawSlots {
		changed := slot.call >= 0 && refs.toolCalls[slot.call].argsChanged
		if !changed && slot.rawArgs == nil {
			continue // no args slot in the original body; nothing to restore
		}
		start, end, ok := rawJSONSpanAt(out, slot.path...)
		if !ok {
			return bodyBytes, fmt.Errorf("response replacement: args slot not found at path %v", slot.path)
		}
		if changed && !slot.objSlot {
			continue // changed string slot: the map already holds the new string
		}
		var repl []byte
		if changed {
			repl = []byte(refs.toolCalls[slot.call].argsJSON)
		} else {
			repl = slot.rawArgs
		}
		ops = append(ops, spliceOp{start: start, end: end, repl: repl})
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].start > ops[j].start })
	for _, op := range ops {
		out = spliceBytes(out, op.start, op.end, op.repl)
	}
	return out, nil
}

// spliceBytes returns a new slice with doc[start:end] replaced by repl.
// Allocates a fresh slice so the source (which may alias the original body)
// is never written through.
func spliceBytes(doc []byte, start, end int, repl []byte) []byte {
	out := make([]byte, 0, len(doc)-(end-start)+len(repl))
	out = append(out, doc[:start]...)
	out = append(out, repl...)
	out = append(out, doc[end:]...)
	return out
}
