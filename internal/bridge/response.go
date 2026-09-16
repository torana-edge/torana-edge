package bridge

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/torana-edge/torana-edge/internal/engine"
	pbjsontext "github.com/torana-edge/torana-plugin-sdk/pb/v1/jsontext"
)

// TranslateResponse projects one complete, non-streaming inference response
// between explicitly configured API contracts. Model is a fallback used only
// when the upstream omitted its model; a reported upstream model is
// authoritative. Native routes are byte preserving, including when model is
// non-empty.
func TranslateResponse(from, to Protocol, body []byte, model string) ([]byte, error) {
	return TranslateResponseWithRequest(from, to, body, model, nil)
}

// TranslateResponseWithRequest supplies the original client request when the
// destination response contract requires request configuration echoes. The
// request is read only. Native routes remain byte preserving.
func TranslateResponseWithRequest(from, to Protocol, body []byte, model string, clientRequest *engine.ChatRequest) ([]byte, error) {
	if !from.Valid() || !to.Valid() {
		return nil, fmt.Errorf("protocol translation: invalid response protocol")
	}
	if from == to {
		return body, nil
	}

	c, err := parseCompletion(from, body)
	if err != nil {
		return nil, err
	}
	if c.Model == "" {
		c.Model = model
	}
	if c.Model == "" {
		return nil, fmt.Errorf("protocol translation: response model is missing and no fallback was configured")
	}
	if err := ensureCompletionIdentities(c); err != nil {
		return nil, err
	}
	if to == OpenAIResponses {
		envelope, err := openAIResponsesEnvelopeFromClient(clientRequest)
		if err != nil {
			return nil, err
		}
		c.ResponsesEnvelope = envelope
	}
	return marshalCompletion(to, c)
}

// CompleteClientResponseEnvelope fills destination fields that are owned by
// Torana for a host-generated, non-streaming response. It is intentionally
// narrower than TranslateResponse: callers must only pass JSON that Torana
// itself just rendered for the client protocol.
func CompleteClientResponseEnvelope(p Protocol, body []byte, clientRequest *engine.ChatRequest) ([]byte, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("protocol translation: invalid response protocol")
	}
	top, err := decodeResponseObject(body, "host-generated")
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	switch p {
	case OpenAIChat:
		object, err := requiredString(top, "object", "host-generated openai-chat response")
		if err != nil || object != "chat.completion" {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("protocol translation: malformed host-generated openai-chat response")
		}
		if _, err := requiredString(top, "id", "host-generated openai-chat response"); err != nil {
			return nil, err
		}
		if _, err := requiredString(top, "model", "host-generated openai-chat response"); err != nil {
			return nil, err
		}
		if _, err := rawArray(top, "choices", "host-generated openai-chat response", true); err != nil {
			return nil, err
		}
		if _, ok := top["created"]; !ok {
			top["created"] = json.RawMessage(strconv.FormatInt(now, 10))
		} else if _, present, err := optionalInt(top, "created", "host-generated openai-chat response"); err != nil || !present {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("protocol translation: host-generated openai-chat response created must be an integer")
		}
	case OpenAIResponses:
		object, err := requiredString(top, "object", "host-generated openai-responses response")
		if err != nil || object != "response" {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("protocol translation: malformed host-generated openai-responses response")
		}
		if _, err := requiredString(top, "id", "host-generated openai-responses response"); err != nil {
			return nil, err
		}
		if _, err := requiredString(top, "model", "host-generated openai-responses response"); err != nil {
			return nil, err
		}
		status, err := requiredString(top, "status", "host-generated openai-responses response")
		if err != nil || status != "completed" {
			if err != nil {
				return nil, err
			}
			return nil, unsupported("non-completed host-generated response")
		}
		if _, err := rawArray(top, "output", "host-generated openai-responses response", true); err != nil {
			return nil, err
		}
		envelope, err := openAIResponsesEnvelopeFromClient(clientRequest)
		if err != nil {
			return nil, err
		}
		if _, ok := top["created_at"]; !ok {
			top["created_at"] = json.RawMessage(strconv.FormatInt(now, 10))
		} else if _, present, err := optionalInt(top, "created_at", "host-generated openai-responses response"); err != nil || !present {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("protocol translation: host-generated openai-responses response created_at must be an integer")
		}
		if _, ok := top["error"]; !ok {
			top["error"] = json.RawMessage("null")
		}
		if _, ok := top["incomplete_details"]; !ok {
			top["incomplete_details"] = json.RawMessage("null")
		}
		top["parallel_tool_calls"] = json.RawMessage(strconv.FormatBool(envelope.ParallelToolCalls))
		top["tool_choice"] = cloneRaw(envelope.ToolChoice)
		top["tools"] = cloneRaw(envelope.Tools)
		if usageRaw, ok := top["usage"]; ok && !bytes.Equal(bytes.TrimSpace(usageRaw), []byte("null")) {
			usage, err := completeOpenAIResponsesUsage(usageRaw)
			if err != nil {
				return nil, err
			}
			top["usage"] = usage
		}
	case Anthropic, Gemini, GeminiCodeAssist:
		// Their host renderer already emits complete SDK envelopes. The strict
		// validation above still protects this public helper from arbitrary JSON.
		return body, nil
	}
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("protocol translation: could not encode host-generated response")
	}
	if err := pbjsontext.Validate(out); err != nil {
		return nil, fmt.Errorf("protocol translation: could not encode host-generated response")
	}
	return out, nil
}

func completeOpenAIResponsesUsage(raw json.RawMessage) (json.RawMessage, error) {
	usage, err := rawObject(raw, "host-generated openai-responses usage")
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64, 3)
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		value, ok, err := optionalInt(usage, key, "host-generated openai-responses usage")
		if err != nil || !ok {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("protocol translation: host-generated openai-responses usage %s is missing", key)
		}
		counts[key] = value
	}
	total, ok := checkedAdd(counts["input_tokens"], counts["output_tokens"])
	if !ok || total != counts["total_tokens"] {
		return nil, fmt.Errorf("protocol translation: host-generated openai-responses usage total is inconsistent")
	}
	inputDetails := map[string]json.RawMessage{}
	if detailsRaw, ok := usage["input_tokens_details"]; ok && !bytes.Equal(bytes.TrimSpace(detailsRaw), []byte("null")) {
		inputDetails, err = rawObject(detailsRaw, "host-generated openai-responses input usage details")
		if err != nil {
			return nil, err
		}
	}
	if _, ok := inputDetails["cached_tokens"]; !ok {
		inputDetails["cached_tokens"] = json.RawMessage("0")
	}
	if _, ok := inputDetails["cache_write_tokens"]; !ok {
		inputDetails["cache_write_tokens"] = json.RawMessage("0")
	}
	for _, key := range []string{"cached_tokens", "cache_write_tokens"} {
		if _, present, err := optionalInt(inputDetails, key, "host-generated openai-responses input usage details"); err != nil || !present {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("protocol translation: host-generated openai-responses input usage detail is invalid")
		}
	}
	inputRaw, err := json.Marshal(inputDetails)
	if err != nil {
		return nil, fmt.Errorf("protocol translation: could not encode host-generated response usage")
	}
	usage["input_tokens_details"] = inputRaw

	outputDetails := map[string]json.RawMessage{}
	if detailsRaw, ok := usage["output_tokens_details"]; ok && !bytes.Equal(bytes.TrimSpace(detailsRaw), []byte("null")) {
		outputDetails, err = rawObject(detailsRaw, "host-generated openai-responses output usage details")
		if err != nil {
			return nil, err
		}
	}
	if _, ok := outputDetails["reasoning_tokens"]; !ok {
		outputDetails["reasoning_tokens"] = json.RawMessage("0")
	}
	if _, present, err := optionalInt(outputDetails, "reasoning_tokens", "host-generated openai-responses output usage details"); err != nil || !present {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: host-generated openai-responses output usage detail is invalid")
	}
	outputRaw, err := json.Marshal(outputDetails)
	if err != nil {
		return nil, fmt.Errorf("protocol translation: could not encode host-generated response usage")
	}
	usage["output_tokens_details"] = outputRaw
	out, err := json.Marshal(usage)
	if err != nil {
		return nil, fmt.Errorf("protocol translation: could not encode host-generated response usage")
	}
	return out, nil
}

// completion is response-specific rather than engine.ChatResponse: the plugin
// projection intentionally cannot carry Responses item IDs, reasoning arms, or
// the complete ordered provider body.
type completion struct {
	ID                string
	Model             string
	CreatedAt         int64
	Blocks            []completionBlock
	Finish            string // stop, tool_calls, length
	Usage             *completionUsage
	ResponsesEnvelope *openAIResponsesEnvelope
}

type openAIResponsesEnvelope struct {
	ParallelToolCalls bool
	ToolChoice        json.RawMessage
	Tools             json.RawMessage
}

type completionBlock struct {
	Text *completionText
	Tool *completionTool
}

type completionText struct {
	Text   string
	ItemID string // Responses message item ID, when the source has one.
	Status string // Responses item status, when the source has one.
}

type completionTool struct {
	CallID string
	ItemID string // Responses output item ID; never aliases CallID by accident.
	Name   string
	Args   json.RawMessage // strict object; numeric and string lexemes remain raw.
	Status string          // Responses item status, when the source has one.
}

// InputTokens is the whole provider prompt count, including cache reads and
// writes. Anthropic is normalized on input/output because its input_tokens
// explicitly excludes both cache counters; OpenAI and Gemini already include
// cache hits in their input total.
type completionUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

func parseCompletion(p Protocol, body []byte) (*completion, error) {
	switch p {
	case OpenAIChat:
		return parseOpenAIChatResponse(body)
	case OpenAIResponses:
		return parseOpenAIResponsesResponse(body)
	case Anthropic:
		return parseAnthropicResponse(body)
	case Gemini:
		return parseGeminiResponse(body, false)
	case GeminiCodeAssist:
		return parseGeminiResponse(body, true)
	default:
		return nil, fmt.Errorf("protocol translation: invalid response protocol")
	}
}

func marshalCompletion(p Protocol, c *completion) ([]byte, error) {
	if err := validateCompletion(c); err != nil {
		return nil, err
	}
	if c.ID == "" {
		return nil, fmt.Errorf("protocol translation: response id is missing")
	}
	if err := validateDestinationToolIdentities(p, c); err != nil {
		return nil, err
	}
	switch p {
	case OpenAIChat:
		return marshalOpenAIChatResponse(c)
	case OpenAIResponses:
		return marshalOpenAIResponsesResponse(c)
	case Anthropic:
		return marshalAnthropicResponse(c)
	case Gemini:
		return marshalGeminiResponse(c, false)
	case GeminiCodeAssist:
		return marshalGeminiResponse(c, true)
	default:
		return nil, fmt.Errorf("protocol translation: invalid response protocol")
	}
}

func validateCompletion(c *completion) error {
	if c == nil {
		return fmt.Errorf("protocol translation: response is missing")
	}
	switch c.Finish {
	case "stop", "tool_calls", "length":
	default:
		return unsupported("response termination")
	}
	callIDs := make(map[string]struct{})
	itemIDs := make(map[string]struct{})
	for i, b := range c.Blocks {
		if (b.Text == nil) == (b.Tool == nil) {
			return fmt.Errorf("protocol translation: malformed response block at index %d", i)
		}
		if b.Tool != nil {
			if b.Tool.Name == "" {
				return fmt.Errorf("protocol translation: tool call name is missing")
			}
			if !validJSONObject(b.Tool.Args) {
				return fmt.Errorf("protocol translation: tool call arguments must be a JSON object")
			}
			if b.Tool.CallID != "" {
				if _, exists := callIDs[b.Tool.CallID]; exists {
					return fmt.Errorf("protocol translation: duplicate tool call id")
				}
				callIDs[b.Tool.CallID] = struct{}{}
			}
			if b.Tool.ItemID != "" {
				if _, exists := itemIDs[b.Tool.ItemID]; exists {
					return fmt.Errorf("protocol translation: duplicate response output item id")
				}
				itemIDs[b.Tool.ItemID] = struct{}{}
			}
		} else if b.Text.ItemID != "" {
			// Several output_text parts legally share their containing message
			// item ID; only reserve it once.
			itemIDs[b.Text.ItemID] = struct{}{}
		}
	}
	if c.Finish == "tool_calls" {
		for _, b := range c.Blocks {
			if b.Tool != nil {
				return validateUsage(c.Usage)
			}
		}
		return fmt.Errorf("protocol translation: tool-call termination has no tool call")
	}
	return validateUsage(c.Usage)
}

func validateUsage(u *completionUsage) error {
	if u == nil {
		return nil
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 {
		return fmt.Errorf("protocol translation: response usage must be non-negative")
	}
	if _, ok := checkedAdd(u.InputTokens, u.OutputTokens); !ok {
		return fmt.Errorf("protocol translation: response total usage overflows")
	}
	cache, ok := checkedAdd(u.CacheReadTokens, u.CacheWriteTokens)
	if !ok {
		return fmt.Errorf("protocol translation: response cache usage overflows")
	}
	if cache > u.InputTokens {
		return fmt.Errorf("protocol translation: response cache usage exceeds input usage")
	}
	return nil
}

// --- Strict raw JSON helpers ------------------------------------------------

func decodeResponseObject(body []byte, context string) (map[string]json.RawMessage, error) {
	// encoding/json accepts duplicate names and replaces invalid Unicode. Run
	// the SDK's strict recursive JSON-text validator before materializing any
	// map so no nested response member can be normalized before inspection.
	if err := pbjsontext.Validate(body); err != nil {
		return nil, fmt.Errorf("protocol translation: malformed %s JSON response", context)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("protocol translation: malformed %s JSON response", context)
	}
	return obj, nil
}

func rejectUnknown(obj map[string]json.RawMessage, context string, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		set[k] = struct{}{}
	}
	for k := range obj {
		if _, ok := set[k]; !ok {
			// A JSON member name is still caller-controlled data. Report the
			// fixed schema location, never echo the unknown name into logs or a
			// client-facing capability error.
			return unsupported(context + " extension fields")
		}
	}
	return nil
}

func requiredString(obj map[string]json.RawMessage, key, context string) (string, error) {
	raw, ok := obj[key]
	if !ok {
		return "", fmt.Errorf("protocol translation: %s %s is missing", context, key)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("protocol translation: %s %s must be a string", context, key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("protocol translation: %s %s must be a string", context, key)
	}
	return s, nil
}

func optionalString(obj map[string]json.RawMessage, key, context string) (string, bool, error) {
	raw, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, fmt.Errorf("protocol translation: %s %s must be a string", context, key)
	}
	return s, true, nil
}

func rawArray(obj map[string]json.RawMessage, key, context string, required bool) ([]json.RawMessage, error) {
	raw, ok := obj[key]
	if !ok {
		if required {
			return nil, fmt.Errorf("protocol translation: %s %s is missing", context, key)
		}
		return nil, nil
	}
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil || a == nil {
		return nil, fmt.Errorf("protocol translation: %s %s must be an array", context, key)
	}
	return a, nil
}

func rawObject(raw json.RawMessage, context string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("protocol translation: %s must be an object", context)
	}
	return obj, nil
}

func optionalInt(obj map[string]json.RawMessage, key, context string) (int64, bool, error) {
	raw, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, nil
	}
	s := string(bytes.TrimSpace(raw))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("protocol translation: %s %s must be a non-negative integer", context, key)
	}
	return n, true, nil
}

func optionalBool(obj map[string]json.RawMessage, key, context string) (bool, bool, error) {
	raw, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false, fmt.Errorf("protocol translation: %s %s must be a boolean", context, key)
	}
	return b, true, nil
}

func validJSONObject(raw []byte) bool {
	_, err := engine.ParseRequiredJSONObject(raw)
	return err == nil
}

func checkedAdd(a, b int64) (int64, bool) {
	if a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

func checkedAdd3(a, b, c int64) (int64, bool) {
	ab, ok := checkedAdd(a, b)
	if !ok {
		return 0, false
	}
	return checkedAdd(ab, c)
}

func presentMeaningful(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) != 0 && !bytes.Equal(t, []byte("null")) && !bytes.Equal(t, []byte("[]")) && !bytes.Equal(t, []byte("{}")) && !bytes.Equal(t, []byte(`""`))
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

// --- OpenAI Chat Completions ----------------------------------------------

func parseOpenAIChatResponse(body []byte) (*completion, error) {
	top, err := decodeResponseObject(body, "openai-chat")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(top, "openai-chat response", "id", "object", "created", "model", "choices", "usage", "system_fingerprint", "service_tier"); err != nil {
		return nil, err
	}
	// system_fingerprint and service_tier describe how the upstream served the
	// completion. They do not alter its content, termination, or usage, and no
	// destination contract has a portable equivalent.
	if object, ok, err := optionalString(top, "object", "openai-chat response"); err != nil || (ok && object != "chat.completion") {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: openai-chat response object is invalid")
	}
	id, _, err := optionalString(top, "id", "openai-chat response")
	if err != nil {
		return nil, err
	}
	model, _, err := optionalString(top, "model", "openai-chat response")
	if err != nil {
		return nil, err
	}
	createdAt, _, err := optionalInt(top, "created", "openai-chat response")
	if err != nil {
		return nil, err
	}
	choices, err := rawArray(top, "choices", "openai-chat response", true)
	if err != nil {
		return nil, err
	}
	if len(choices) != 1 {
		return nil, unsupported("multiple or missing openai-chat choices")
	}
	choice, err := rawObject(choices[0], "openai-chat choice")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(choice, "openai-chat choice", "index", "message", "finish_reason", "logprobs"); err != nil {
		return nil, err
	}
	if index, ok, err := optionalInt(choice, "index", "openai-chat choice"); err != nil || (ok && index != 0) {
		if err != nil {
			return nil, err
		}
		return nil, unsupported("nonzero openai-chat choice index")
	}
	if raw, ok := choice["logprobs"]; ok && presentMeaningful(raw) {
		return nil, unsupported("openai-chat log probabilities")
	}
	finish, err := requiredString(choice, "finish_reason", "openai-chat choice")
	if err != nil {
		return nil, err
	}
	canonicalFinish, err := openAIChatFinish(finish)
	if err != nil {
		return nil, err
	}
	msgRaw, ok := choice["message"]
	if !ok {
		return nil, fmt.Errorf("protocol translation: openai-chat choice message is missing")
	}
	msg, err := rawObject(msgRaw, "openai-chat message")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(msg, "openai-chat message", "role", "content", "tool_calls", "refusal", "annotations", "audio", "function_call"); err != nil {
		return nil, err
	}
	role, err := requiredString(msg, "role", "openai-chat message")
	if err != nil || role != "assistant" {
		if err != nil {
			return nil, err
		}
		return nil, unsupported("non-assistant openai-chat response role")
	}
	for _, k := range []string{"refusal", "annotations", "audio", "function_call"} {
		if raw, ok := msg[k]; ok && presentMeaningful(raw) {
			return nil, unsupported("openai-chat " + k)
		}
	}
	c := &completion{ID: id, Model: model, CreatedAt: createdAt, Finish: canonicalFinish}
	if raw, ok := msg["content"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, unsupported("openai-chat non-text content")
		}
		c.Blocks = append(c.Blocks, completionBlock{Text: &completionText{Text: text}})
	}
	if _, ok := msg["tool_calls"]; ok {
		calls, err := rawArray(msg, "tool_calls", "openai-chat message", false)
		if err != nil {
			return nil, err
		}
		for _, rawCall := range calls {
			call, err := parseOpenAIChatToolCall(rawCall)
			if err != nil {
				return nil, err
			}
			c.Blocks = append(c.Blocks, completionBlock{Tool: call})
		}
	}
	c.Usage, err = parseOpenAIUsage(top["usage"], false)
	if err != nil {
		return nil, err
	}
	if canonicalFinish == "tool_calls" && !hasTools(c.Blocks) {
		return nil, fmt.Errorf("protocol translation: openai-chat tool-call termination has no tool call")
	}
	if canonicalFinish == "stop" && hasTools(c.Blocks) {
		return nil, fmt.Errorf("protocol translation: openai-chat stop termination conflicts with tool calls")
	}
	return c, validateCompletion(c)
}

func parseOpenAIChatToolCall(raw json.RawMessage) (*completionTool, error) {
	obj, err := rawObject(raw, "openai-chat tool call")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(obj, "openai-chat tool call", "id", "type", "function"); err != nil {
		return nil, err
	}
	id, err := requiredString(obj, "id", "openai-chat tool call")
	if err != nil || id == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: openai-chat tool call id is empty")
	}
	typ, err := requiredString(obj, "type", "openai-chat tool call")
	if err != nil || typ != "function" {
		if err != nil {
			return nil, err
		}
		return nil, unsupported("openai-chat non-function tool call")
	}
	fnRaw, ok := obj["function"]
	if !ok {
		return nil, fmt.Errorf("protocol translation: openai-chat tool function is missing")
	}
	fn, err := rawObject(fnRaw, "openai-chat tool function")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(fn, "openai-chat tool function", "name", "arguments"); err != nil {
		return nil, err
	}
	name, err := requiredString(fn, "name", "openai-chat tool function")
	if err != nil {
		return nil, err
	}
	argText, err := requiredString(fn, "arguments", "openai-chat tool function")
	if err != nil || !validJSONObject([]byte(argText)) {
		return nil, fmt.Errorf("protocol translation: openai-chat tool arguments must contain a JSON object")
	}
	return &completionTool{CallID: id, Name: name, Args: json.RawMessage(argText)}, nil
}

func openAIChatFinish(s string) (string, error) {
	switch s {
	case "stop":
		return "stop", nil
	case "tool_calls":
		return "tool_calls", nil
	case "length":
		return "length", nil
	default:
		return "", unsupported("openai-chat termination reason")
	}
}

// --- OpenAI Responses ------------------------------------------------------

// These documented Response members are request echoes or observations about
// how OpenAI served the request. They do not change the ordered output,
// terminal status/error, model, or usage projected by this bridge. Keeping the
// list explicit preserves fail-closed behavior for future unknown members.
var openAIResponsesObservedFields = []string{
	"background",
	"completed_at",
	"conversation",
	"created_at",
	"instructions",
	"max_output_tokens",
	"max_tool_calls",
	"metadata",
	"parallel_tool_calls",
	"previous_response_id",
	"prompt",
	"prompt_cache_diagnostics",
	"prompt_cache_key",
	"prompt_cache_options",
	"prompt_cache_retention",
	"reasoning",
	"safety_identifier",
	"service_tier",
	"store",
	"temperature",
	"text",
	"tool_choice",
	"tools",
	"top_logprobs",
	"top_p",
	"truncation",
	"user",
}

func parseOpenAIResponsesResponse(body []byte) (*completion, error) {
	top, err := decodeResponseObject(body, "openai-responses")
	if err != nil {
		return nil, err
	}
	allowedTop := []string{"id", "object", "model", "status", "error", "incomplete_details", "moderation", "output", "usage"}
	allowedTop = append(allowedTop, openAIResponsesObservedFields...)
	if err := rejectUnknown(top, "openai-responses response", allowedTop...); err != nil {
		return nil, err
	}
	if object, ok, err := optionalString(top, "object", "openai-responses response"); err != nil || (ok && object != "response") {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: openai-responses response object is invalid")
	}
	id, _, err := optionalString(top, "id", "openai-responses response")
	if err != nil {
		return nil, err
	}
	model, _, err := optionalString(top, "model", "openai-responses response")
	if err != nil {
		return nil, err
	}
	createdAt, _, err := optionalInt(top, "created_at", "openai-responses response")
	if err != nil {
		return nil, err
	}
	status, err := requiredString(top, "status", "openai-responses response")
	if err != nil {
		return nil, err
	}
	if raw, ok := top["error"]; ok && presentMeaningful(raw) {
		return nil, unsupported("failed openai-responses response")
	}
	if raw, ok := top["moderation"]; ok && presentMeaningful(raw) {
		return nil, unsupported("openai-responses moderation results")
	}
	finish := "stop"
	switch status {
	case "completed":
		if raw, ok := top["incomplete_details"]; ok && presentMeaningful(raw) {
			return nil, fmt.Errorf("protocol translation: completed openai-responses response has incomplete details")
		}
	case "incomplete":
		raw, ok := top["incomplete_details"]
		if !ok {
			return nil, fmt.Errorf("protocol translation: incomplete openai-responses response lacks details")
		}
		details, err := rawObject(raw, "openai-responses incomplete details")
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(details, "openai-responses incomplete details", "reason"); err != nil {
			return nil, err
		}
		reason, err := requiredString(details, "reason", "openai-responses incomplete details")
		if err != nil || reason != "max_output_tokens" {
			if err != nil {
				return nil, err
			}
			return nil, unsupported("openai-responses incomplete reason")
		}
		finish = "length"
	default:
		return nil, unsupported("nonterminal or failed openai-responses status")
	}
	output, err := rawArray(top, "output", "openai-responses response", true)
	if err != nil {
		return nil, err
	}
	c := &completion{ID: id, Model: model, CreatedAt: createdAt, Finish: finish}
	for _, rawItem := range output {
		item, err := rawObject(rawItem, "openai-responses output item")
		if err != nil {
			return nil, err
		}
		typ, err := requiredString(item, "type", "openai-responses output item")
		if err != nil {
			return nil, err
		}
		switch typ {
		case "message":
			blocks, err := parseResponsesMessage(item, status)
			if err != nil {
				return nil, err
			}
			c.Blocks = append(c.Blocks, blocks...)
		case "function_call":
			tool, err := parseResponsesFunctionCall(item, status)
			if err != nil {
				return nil, err
			}
			c.Blocks = append(c.Blocks, completionBlock{Tool: tool})
		default:
			return nil, unsupported("openai-responses output item type")
		}
	}
	if c.Finish == "stop" && hasTools(c.Blocks) {
		c.Finish = "tool_calls"
	}
	c.Usage, err = parseOpenAIUsage(top["usage"], true)
	if err != nil {
		return nil, err
	}
	return c, validateCompletion(c)
}

func parseResponsesMessage(item map[string]json.RawMessage, responseStatus string) ([]completionBlock, error) {
	if err := rejectUnknown(item, "openai-responses message", "id", "type", "status", "role", "content"); err != nil {
		return nil, err
	}
	id, _, err := optionalString(item, "id", "openai-responses message")
	if err != nil {
		return nil, err
	}
	role, err := requiredString(item, "role", "openai-responses message")
	if err != nil || role != "assistant" {
		if err != nil {
			return nil, err
		}
		return nil, unsupported("non-assistant openai-responses message role")
	}
	itemStatus, err := validateResponsesItemStatus(item, responseStatus)
	if err != nil {
		return nil, err
	}
	content, err := rawArray(item, "content", "openai-responses message", true)
	if err != nil {
		return nil, err
	}
	blocks := make([]completionBlock, 0, len(content))
	for _, rawPart := range content {
		part, err := rawObject(rawPart, "openai-responses content part")
		if err != nil {
			return nil, err
		}
		typ, err := requiredString(part, "type", "openai-responses content part")
		if err != nil {
			return nil, err
		}
		if typ != "output_text" {
			return nil, unsupported("openai-responses non-text content part")
		}
		if err := rejectUnknown(part, "openai-responses output text", "type", "text", "annotations", "logprobs"); err != nil {
			return nil, err
		}
		for _, k := range []string{"annotations", "logprobs"} {
			if raw, ok := part[k]; ok && presentMeaningful(raw) {
				return nil, unsupported("openai-responses output text " + k)
			}
		}
		text, err := requiredString(part, "text", "openai-responses output text")
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, completionBlock{Text: &completionText{Text: text, ItemID: id, Status: itemStatus}})
	}
	return blocks, nil
}

func parseResponsesFunctionCall(item map[string]json.RawMessage, responseStatus string) (*completionTool, error) {
	if err := rejectUnknown(item, "openai-responses function call", "id", "type", "status", "call_id", "name", "arguments"); err != nil {
		return nil, err
	}
	itemStatus, err := validateResponsesItemStatus(item, responseStatus)
	if err != nil {
		return nil, err
	}
	itemID, err := requiredString(item, "id", "openai-responses function call")
	if err != nil || itemID == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: openai-responses function call id is empty")
	}
	callID, err := requiredString(item, "call_id", "openai-responses function call")
	if err != nil || callID == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: openai-responses function call call_id is empty")
	}
	name, err := requiredString(item, "name", "openai-responses function call")
	if err != nil {
		return nil, err
	}
	args, err := requiredString(item, "arguments", "openai-responses function call")
	if err != nil || !validJSONObject([]byte(args)) {
		return nil, fmt.Errorf("protocol translation: openai-responses function arguments must contain a JSON object")
	}
	return &completionTool{ItemID: itemID, CallID: callID, Name: name, Args: json.RawMessage(args), Status: itemStatus}, nil
}

func validateResponsesItemStatus(item map[string]json.RawMessage, responseStatus string) (string, error) {
	status, ok, err := optionalString(item, "status", "openai-responses output item")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("protocol translation: openai-responses output item status is missing")
	}
	if status != "completed" && !(responseStatus == "incomplete" && status == "incomplete") {
		return "", unsupported("openai-responses output item status")
	}
	return status, nil
}

// --- Anthropic Messages ----------------------------------------------------

func parseAnthropicResponse(body []byte) (*completion, error) {
	top, err := decodeResponseObject(body, "anthropic")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(top, "anthropic response", "id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "usage"); err != nil {
		return nil, err
	}
	if typ, ok, err := optionalString(top, "type", "anthropic response"); err != nil || (ok && typ != "message") {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: anthropic response type is invalid")
	}
	id, _, err := optionalString(top, "id", "anthropic response")
	if err != nil {
		return nil, err
	}
	model, _, err := optionalString(top, "model", "anthropic response")
	if err != nil {
		return nil, err
	}
	role, err := requiredString(top, "role", "anthropic response")
	if err != nil || role != "assistant" {
		if err != nil {
			return nil, err
		}
		return nil, unsupported("non-assistant anthropic response role")
	}
	stop, err := requiredString(top, "stop_reason", "anthropic response")
	if err != nil {
		return nil, err
	}
	finish := ""
	switch stop {
	case "end_turn", "stop_sequence":
		finish = "stop"
	case "tool_use":
		finish = "tool_calls"
	case "max_tokens":
		finish = "length"
	default:
		return nil, unsupported("anthropic termination reason")
	}
	if stop == "stop_sequence" {
		if _, ok, err := optionalString(top, "stop_sequence", "anthropic response"); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("protocol translation: anthropic stop sequence is missing")
		}
	} else if raw, ok := top["stop_sequence"]; ok && presentMeaningful(raw) {
		return nil, fmt.Errorf("protocol translation: anthropic stop sequence conflicts with termination")
	}
	content, err := rawArray(top, "content", "anthropic response", true)
	if err != nil {
		return nil, err
	}
	c := &completion{ID: id, Model: model, Finish: finish}
	for _, rawBlock := range content {
		block, err := rawObject(rawBlock, "anthropic content block")
		if err != nil {
			return nil, err
		}
		typ, err := requiredString(block, "type", "anthropic content block")
		if err != nil {
			return nil, err
		}
		switch typ {
		case "text":
			if err := rejectUnknown(block, "anthropic text block", "type", "text", "citations"); err != nil {
				return nil, err
			}
			if raw, ok := block["citations"]; ok && presentMeaningful(raw) {
				return nil, unsupported("anthropic text citations")
			}
			text, err := requiredString(block, "text", "anthropic text block")
			if err != nil {
				return nil, err
			}
			c.Blocks = append(c.Blocks, completionBlock{Text: &completionText{Text: text}})
		case "tool_use":
			if err := rejectUnknown(block, "anthropic tool block", "type", "id", "name", "input", "caller"); err != nil {
				return nil, err
			}
			if raw, ok := block["caller"]; ok && presentMeaningful(raw) {
				return nil, unsupported("anthropic tool caller metadata")
			}
			callID, err := requiredString(block, "id", "anthropic tool block")
			if err != nil || callID == "" {
				return nil, fmt.Errorf("protocol translation: anthropic tool call id is missing")
			}
			name, err := requiredString(block, "name", "anthropic tool block")
			if err != nil {
				return nil, err
			}
			args, ok := block["input"]
			if !ok || !validJSONObject(args) {
				return nil, fmt.Errorf("protocol translation: anthropic tool input must be a JSON object")
			}
			c.Blocks = append(c.Blocks, completionBlock{Tool: &completionTool{CallID: callID, Name: name, Args: cloneRaw(args)}})
		default:
			return nil, unsupported("anthropic content block type")
		}
	}
	c.Usage, err = parseAnthropicUsage(top["usage"])
	if err != nil {
		return nil, err
	}
	return c, validateCompletion(c)
}

// --- Gemini generateContent / Code Assist ---------------------------------

func parseGeminiResponse(body []byte, wrapped bool) (*completion, error) {
	top, err := decodeResponseObject(body, "gemini")
	if err != nil {
		return nil, err
	}
	if wrapped {
		if err := rejectUnknown(top, "gemini-codeassist response", "response"); err != nil {
			return nil, err
		}
		raw, ok := top["response"]
		if !ok {
			return nil, fmt.Errorf("protocol translation: gemini-codeassist response envelope is missing")
		}
		top, err = rawObject(raw, "gemini-codeassist response envelope")
		if err != nil {
			return nil, err
		}
	} else if _, hasEnvelope := top["response"]; hasEnvelope {
		return nil, fmt.Errorf("protocol translation: bare gemini response contains a codeassist envelope")
	}
	if err := rejectUnknown(top, "gemini response", "candidates", "usageMetadata", "modelVersion", "responseId"); err != nil {
		return nil, err
	}
	model, _, err := optionalString(top, "modelVersion", "gemini response")
	if err != nil {
		return nil, err
	}
	id, _, err := optionalString(top, "responseId", "gemini response")
	if err != nil {
		return nil, err
	}
	candidates, err := rawArray(top, "candidates", "gemini response", true)
	if err != nil {
		return nil, err
	}
	if len(candidates) != 1 {
		return nil, unsupported("multiple or missing gemini candidates")
	}
	cand, err := rawObject(candidates[0], "gemini candidate")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(cand, "gemini candidate", "content", "finishReason", "index"); err != nil {
		return nil, err
	}
	if index, ok, err := optionalInt(cand, "index", "gemini candidate"); err != nil || (ok && index != 0) {
		if err != nil {
			return nil, err
		}
		return nil, unsupported("nonzero gemini candidate index")
	}
	reason, err := requiredString(cand, "finishReason", "gemini candidate")
	if err != nil {
		return nil, err
	}
	finish := ""
	switch reason {
	case "STOP":
		finish = "stop"
	case "MAX_TOKENS":
		finish = "length"
	default:
		return nil, unsupported("gemini termination reason")
	}
	contentRaw, ok := cand["content"]
	if !ok {
		return nil, fmt.Errorf("protocol translation: gemini candidate content is missing")
	}
	content, err := rawObject(contentRaw, "gemini candidate content")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(content, "gemini candidate content", "role", "parts"); err != nil {
		return nil, err
	}
	role, err := requiredString(content, "role", "gemini candidate content")
	if err != nil || role != "model" {
		if err != nil {
			return nil, err
		}
		return nil, unsupported("non-model gemini response role")
	}
	parts, err := rawArray(content, "parts", "gemini candidate content", true)
	if err != nil {
		return nil, err
	}
	c := &completion{ID: id, Model: model, Finish: finish}
	for _, rawPart := range parts {
		part, err := rawObject(rawPart, "gemini response part")
		if err != nil {
			return nil, err
		}
		if raw, ok := part["thoughtSignature"]; ok && presentMeaningful(raw) {
			return nil, unsupported("gemini thought signature")
		}
		if raw, ok := part["partMetadata"]; ok && presentMeaningful(raw) {
			return nil, unsupported("gemini part metadata")
		}
		thought, hasThought, err := optionalBool(part, "thought", "gemini response part")
		if err != nil {
			return nil, err
		}
		if hasThought && thought {
			return nil, unsupported("gemini thinking content")
		}
		_, hasText := part["text"]
		_, hasCall := part["functionCall"]
		if hasText == hasCall {
			return nil, unsupported("gemini response part shape")
		}
		if hasText {
			if err := rejectUnknown(part, "gemini text part", "text", "thought", "thoughtSignature", "partMetadata"); err != nil {
				return nil, err
			}
			text, err := requiredString(part, "text", "gemini text part")
			if err != nil {
				return nil, err
			}
			c.Blocks = append(c.Blocks, completionBlock{Text: &completionText{Text: text}})
			continue
		}
		if err := rejectUnknown(part, "gemini function-call part", "functionCall", "thought", "thoughtSignature", "partMetadata"); err != nil {
			return nil, err
		}
		callObj, err := rawObject(part["functionCall"], "gemini function call")
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(callObj, "gemini function call", "id", "name", "args"); err != nil {
			return nil, err
		}
		callID, _, err := optionalString(callObj, "id", "gemini function call")
		if err != nil {
			return nil, err
		}
		name, err := requiredString(callObj, "name", "gemini function call")
		if err != nil {
			return nil, err
		}
		args, ok := callObj["args"]
		if !ok || !validJSONObject(args) {
			return nil, fmt.Errorf("protocol translation: gemini function arguments must be a JSON object")
		}
		c.Blocks = append(c.Blocks, completionBlock{Tool: &completionTool{CallID: callID, Name: name, Args: cloneRaw(args)}})
	}
	if c.Finish == "stop" && hasTools(c.Blocks) {
		c.Finish = "tool_calls"
	}
	c.Usage, err = parseGeminiUsage(top["usageMetadata"])
	if err != nil {
		return nil, err
	}
	return c, validateCompletion(c)
}

// --- Usage -----------------------------------------------------------------

func parseOpenAIUsage(raw json.RawMessage, responses bool) (*completionUsage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	obj, err := rawObject(raw, "openai usage")
	if err != nil {
		return nil, err
	}
	inKey, outKey, detailsKey := "prompt_tokens", "completion_tokens", "prompt_tokens_details"
	if responses {
		inKey, outKey, detailsKey = "input_tokens", "output_tokens", "input_tokens_details"
	}
	allowed := []string{inKey, outKey, "total_tokens", detailsKey}
	if responses {
		allowed = append(allowed, "output_tokens_details")
	} else {
		allowed = append(allowed, "completion_tokens_details", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens")
	}
	if err := rejectUnknown(obj, "openai usage", allowed...); err != nil {
		return nil, err
	}
	in, ok, err := optionalInt(obj, inKey, "openai usage")
	if err != nil || !ok {
		return nil, fmt.Errorf("protocol translation: openai usage input count is missing")
	}
	out, ok, err := optionalInt(obj, outKey, "openai usage")
	if err != nil || !ok {
		return nil, fmt.Errorf("protocol translation: openai usage output count is missing")
	}
	wantTotal, sumOK := checkedAdd(in, out)
	if !sumOK {
		return nil, fmt.Errorf("protocol translation: openai usage total overflows")
	}
	if total, ok, err := optionalInt(obj, "total_tokens", "openai usage"); err != nil || (ok && total != wantTotal) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: openai usage total is inconsistent")
	}
	u := &completionUsage{InputTokens: in, OutputTokens: out}
	if detailsRaw, ok := obj[detailsKey]; ok && !bytes.Equal(bytes.TrimSpace(detailsRaw), []byte("null")) {
		details, err := rawObject(detailsRaw, "openai input token details")
		if err != nil {
			return nil, err
		}
		allowedDetails := []string{"cached_tokens", "cache_write_tokens"}
		if !responses {
			allowedDetails = append(allowedDetails, "audio_tokens", "image_tokens", "text_tokens")
		}
		if err := rejectUnknown(details, "openai input token details", allowedDetails...); err != nil {
			return nil, err
		}
		u.CacheReadTokens, _, err = optionalInt(details, "cached_tokens", "openai input token details")
		if err != nil {
			return nil, err
		}
		u.CacheWriteTokens, _, err = optionalInt(details, "cache_write_tokens", "openai input token details")
		if err != nil {
			return nil, err
		}
		if !responses {
			if err := requireZeroCounts(details, "openai input token modality details", "audio_tokens", "image_tokens", "text_tokens"); err != nil {
				return nil, err
			}
		}
	}
	if !responses {
		hit, hitOK, err := optionalInt(obj, "prompt_cache_hit_tokens", "openai usage")
		if err != nil {
			return nil, err
		}
		miss, missOK, err := optionalInt(obj, "prompt_cache_miss_tokens", "openai usage")
		if err != nil {
			return nil, err
		}
		if hitOK != missOK {
			return nil, fmt.Errorf("protocol translation: openai cache hit and miss counts must be reported together")
		}
		if hitOK {
			cacheTotal, ok := checkedAdd(hit, miss)
			if !ok || cacheTotal != in || hit != u.CacheReadTokens {
				return nil, fmt.Errorf("protocol translation: openai cache hit and miss counts are inconsistent")
			}
		}
	}
	outputDetailsKey := "output_tokens_details"
	outputDetailNames := []string{"reasoning_tokens"}
	if !responses {
		outputDetailsKey = "completion_tokens_details"
		outputDetailNames = []string{"accepted_prediction_tokens", "audio_tokens", "reasoning_tokens", "rejected_prediction_tokens", "text_tokens"}
	}
	if detailsRaw, ok := obj[outputDetailsKey]; ok && !bytes.Equal(bytes.TrimSpace(detailsRaw), []byte("null")) {
		details, err := rawObject(detailsRaw, "openai output token details")
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(details, "openai output token details", outputDetailNames...); err != nil {
			return nil, err
		}
		if err := requireZeroCounts(details, "openai output token details", outputDetailNames...); err != nil {
			return nil, err
		}
	}
	return u, validateUsage(u)
}

func requireZeroCounts(obj map[string]json.RawMessage, context string, keys ...string) error {
	for _, key := range keys {
		value, ok, err := optionalInt(obj, key, context)
		if err != nil {
			return err
		}
		if ok && value != 0 {
			// The canonical completion retains the provider's inclusive total,
			// but cannot faithfully expose this billed/category subcount on every
			// destination contract.
			return unsupported(context)
		}
	}
	return nil
}

func parseAnthropicUsage(raw json.RawMessage) (*completionUsage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	obj, err := rawObject(raw, "anthropic usage")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(obj, "anthropic usage", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"); err != nil {
		return nil, err
	}
	in, ok, err := optionalInt(obj, "input_tokens", "anthropic usage")
	if err != nil || !ok {
		return nil, fmt.Errorf("protocol translation: anthropic usage input count is missing")
	}
	out, ok, err := optionalInt(obj, "output_tokens", "anthropic usage")
	if err != nil || !ok {
		return nil, fmt.Errorf("protocol translation: anthropic usage output count is missing")
	}
	read, _, err := optionalInt(obj, "cache_read_input_tokens", "anthropic usage")
	if err != nil {
		return nil, err
	}
	write, _, err := optionalInt(obj, "cache_creation_input_tokens", "anthropic usage")
	if err != nil {
		return nil, err
	}
	totalInput, ok := checkedAdd3(in, read, write)
	if !ok {
		return nil, fmt.Errorf("protocol translation: anthropic input usage overflows")
	}
	u := &completionUsage{InputTokens: totalInput, OutputTokens: out, CacheReadTokens: read, CacheWriteTokens: write}
	return u, validateUsage(u)
}

func parseGeminiUsage(raw json.RawMessage) (*completionUsage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	obj, err := rawObject(raw, "gemini usage metadata")
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(obj, "gemini usage metadata", "promptTokenCount", "candidatesTokenCount", "totalTokenCount", "cachedContentTokenCount"); err != nil {
		return nil, err
	}
	in, ok, err := optionalInt(obj, "promptTokenCount", "gemini usage metadata")
	if err != nil || !ok {
		return nil, fmt.Errorf("protocol translation: gemini usage input count is missing")
	}
	out, ok, err := optionalInt(obj, "candidatesTokenCount", "gemini usage metadata")
	if err != nil || !ok {
		return nil, fmt.Errorf("protocol translation: gemini usage output count is missing")
	}
	wantTotal, sumOK := checkedAdd(in, out)
	if !sumOK {
		return nil, fmt.Errorf("protocol translation: gemini usage total overflows")
	}
	if total, ok, err := optionalInt(obj, "totalTokenCount", "gemini usage metadata"); err != nil || (ok && total != wantTotal) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("protocol translation: gemini usage total is inconsistent")
	}
	read, _, err := optionalInt(obj, "cachedContentTokenCount", "gemini usage metadata")
	if err != nil {
		return nil, err
	}
	u := &completionUsage{InputTokens: in, OutputTokens: out, CacheReadTokens: read}
	return u, validateUsage(u)
}

// --- Marshal ---------------------------------------------------------------

func openAIResponsesEnvelopeFromClient(chat *engine.ChatRequest) (*openAIResponsesEnvelope, error) {
	if chat == nil || chat.OpenAIVariant != engine.OpenAIResponses {
		return nil, unsupported("OpenAI Responses request context for required response fields")
	}
	raw, err := MarshalRequest(OpenAIResponses, chat)
	if err != nil {
		return nil, unsupported("OpenAI Responses request context for required response fields")
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil || request == nil {
		return nil, unsupported("OpenAI Responses request context for required response fields")
	}
	envelope := &openAIResponsesEnvelope{
		ParallelToolCalls: true,
		ToolChoice:        json.RawMessage(`"auto"`),
		Tools:             json.RawMessage(`[]`),
	}
	if parallelRaw, ok := request["parallel_tool_calls"]; ok {
		if json.Unmarshal(parallelRaw, &envelope.ParallelToolCalls) != nil {
			return nil, unsupported("OpenAI Responses parallel tool response field")
		}
	}
	if choiceRaw, ok := request["tool_choice"]; ok {
		envelope.ToolChoice = cloneRaw(choiceRaw)
	}
	if toolsRaw, ok := request["tools"]; ok {
		var tools []json.RawMessage
		if json.Unmarshal(toolsRaw, &tools) != nil || tools == nil {
			return nil, unsupported("OpenAI Responses tools response field")
		}
		envelope.Tools = cloneRaw(toolsRaw)
	}
	return envelope, nil
}

func marshalOpenAIChatResponse(c *completion) ([]byte, error) {
	var text *string
	var calls []any
	seenTool := false
	for i, b := range c.Blocks {
		if b.Text != nil {
			if text != nil || seenTool {
				return nil, unsupported("ordered response blocks in openai-chat")
			}
			t := b.Text.Text
			text = &t
			continue
		}
		seenTool = true
		id := ensureCallID(b.Tool.CallID, i)
		calls = append(calls, struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: b.Tool.Name, Arguments: string(b.Tool.Args)}})
	}
	message := map[string]any{"role": "assistant", "content": text}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": chatFinish(c.Finish)}
	top := map[string]any{"id": c.ID, "object": "chat.completion", "created": c.CreatedAt, "model": c.Model, "choices": []any{choice}}
	if c.Usage != nil {
		if c.Usage.CacheWriteTokens != 0 {
			return nil, unsupported("cache write token accounting in openai-chat responses")
		}
		u := map[string]any{"prompt_tokens": c.Usage.InputTokens, "completion_tokens": c.Usage.OutputTokens, "total_tokens": c.Usage.InputTokens + c.Usage.OutputTokens}
		if c.Usage.CacheReadTokens != 0 {
			u["prompt_tokens_details"] = map[string]any{"cached_tokens": c.Usage.CacheReadTokens}
		}
		top["usage"] = u
	}
	return json.Marshal(top)
}

func marshalOpenAIResponsesResponse(c *completion) ([]byte, error) {
	if c.ResponsesEnvelope == nil {
		return nil, unsupported("OpenAI Responses request context for required response fields")
	}
	status := "completed"
	if c.Finish == "length" {
		status = "incomplete"
	}
	output := make([]any, 0, len(c.Blocks))
	seenItemIDs := make(map[string]struct{})
	for i := 0; i < len(c.Blocks); {
		b := c.Blocks[i]
		if b.Text != nil {
			itemID := b.Text.ItemID
			if itemID == "" {
				itemID = completionGeneratedItemID("msg", c.ID, i)
			}
			if _, exists := seenItemIDs[itemID]; exists {
				return nil, fmt.Errorf("protocol translation: duplicate response output item id")
			}
			seenItemIDs[itemID] = struct{}{}
			itemStatus := responseItemStatus(b.Text.Status, status)
			content := []any{map[string]any{"type": "output_text", "text": b.Text.Text, "annotations": []any{}}}
			j := i + 1
			// parseResponsesMessage flattens a message's ordered content parts.
			// Reassemble consecutive parts carrying the same source item ID so
			// one provider item never becomes duplicate destination item IDs.
			for b.Text.ItemID != "" && j < len(c.Blocks) {
				next := c.Blocks[j]
				if next.Text == nil || next.Text.ItemID != b.Text.ItemID || responseItemStatus(next.Text.Status, status) != itemStatus {
					break
				}
				content = append(content, map[string]any{"type": "output_text", "text": next.Text.Text, "annotations": []any{}})
				j++
			}
			output = append(output, map[string]any{
				"id": itemID, "type": "message", "status": itemStatus, "role": "assistant",
				"content": content,
			})
			i = j
			continue
		}
		callID := ensureCallID(b.Tool.CallID, i)
		itemID := b.Tool.ItemID
		if itemID == "" || itemID == callID {
			itemID = completionGeneratedItemID("fc", c.ID, i)
		}
		if _, exists := seenItemIDs[itemID]; exists {
			return nil, fmt.Errorf("protocol translation: duplicate response output item id")
		}
		seenItemIDs[itemID] = struct{}{}
		itemStatus := responseItemStatus(b.Tool.Status, status)
		output = append(output, map[string]any{
			"id": itemID, "type": "function_call", "status": itemStatus,
			"call_id": callID, "name": b.Tool.Name, "arguments": string(b.Tool.Args),
		})
		i++
	}
	top := map[string]any{
		"id": c.ID, "object": "response", "created_at": c.CreatedAt,
		"model": c.Model, "status": status, "error": nil, "incomplete_details": nil, "output": output,
		"parallel_tool_calls": c.ResponsesEnvelope.ParallelToolCalls,
		"tool_choice":         c.ResponsesEnvelope.ToolChoice,
		"tools":               c.ResponsesEnvelope.Tools,
	}
	if status == "incomplete" {
		top["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if c.Usage != nil {
		// The Responses schema requires cache_write_tokens inside
		// input_tokens_details even when no cache write was billed.
		details := map[string]any{"cached_tokens": c.Usage.CacheReadTokens, "cache_write_tokens": c.Usage.CacheWriteTokens}
		u := map[string]any{"input_tokens": c.Usage.InputTokens, "output_tokens": c.Usage.OutputTokens, "total_tokens": c.Usage.InputTokens + c.Usage.OutputTokens}
		u["input_tokens_details"] = details
		u["output_tokens_details"] = map[string]any{"reasoning_tokens": 0}
		top["usage"] = u
	}
	return json.Marshal(top)
}

func marshalAnthropicResponse(c *completion) ([]byte, error) {
	if c.Usage == nil {
		return nil, unsupported("missing usage required by Anthropic responses")
	}
	content := make([]any, 0, len(c.Blocks))
	for i, b := range c.Blocks {
		if b.Text != nil {
			content = append(content, map[string]any{"type": "text", "text": b.Text.Text})
		} else {
			content = append(content, map[string]any{"type": "tool_use", "id": ensureCallID(b.Tool.CallID, i), "name": b.Tool.Name, "input": json.RawMessage(b.Tool.Args)})
		}
	}
	stop := "end_turn"
	switch c.Finish {
	case "tool_calls":
		stop = "tool_use"
	case "length":
		stop = "max_tokens"
	}
	top := map[string]any{"id": c.ID, "type": "message", "role": "assistant", "model": c.Model, "content": content, "stop_reason": stop, "stop_sequence": nil}
	uncached := c.Usage.InputTokens - c.Usage.CacheReadTokens - c.Usage.CacheWriteTokens
	top["usage"] = map[string]any{
		"input_tokens": uncached, "output_tokens": c.Usage.OutputTokens,
		"cache_read_input_tokens":     c.Usage.CacheReadTokens,
		"cache_creation_input_tokens": c.Usage.CacheWriteTokens,
	}
	return json.Marshal(top)
}

func marshalGeminiResponse(c *completion, wrapped bool) ([]byte, error) {
	if c.Usage != nil && c.Usage.CacheWriteTokens != 0 {
		return nil, unsupported("cache write token accounting in gemini responses")
	}
	parts := make([]any, 0, len(c.Blocks))
	for i, b := range c.Blocks {
		if b.Text != nil {
			parts = append(parts, map[string]any{"text": b.Text.Text})
		} else {
			parts = append(parts, map[string]any{"functionCall": map[string]any{
				"id": ensureCallID(b.Tool.CallID, i), "name": b.Tool.Name, "args": json.RawMessage(b.Tool.Args),
			}})
		}
	}
	reason := "STOP"
	if c.Finish == "length" {
		reason = "MAX_TOKENS"
	}
	inner := map[string]any{
		"modelVersion": c.Model,
		"candidates": []any{map[string]any{
			"index": 0, "content": map[string]any{"role": "model", "parts": parts}, "finishReason": reason,
		}},
	}
	inner["responseId"] = c.ID
	if c.Usage != nil {
		inner["usageMetadata"] = map[string]any{
			"promptTokenCount": c.Usage.InputTokens, "candidatesTokenCount": c.Usage.OutputTokens,
			"totalTokenCount":         c.Usage.InputTokens + c.Usage.OutputTokens,
			"cachedContentTokenCount": c.Usage.CacheReadTokens,
		}
	}
	if wrapped {
		return json.Marshal(map[string]any{"response": inner})
	}
	return json.Marshal(inner)
}

func hasTools(blocks []completionBlock) bool {
	for _, b := range blocks {
		if b.Tool != nil {
			return true
		}
	}
	return false
}

func ensureCompletionIdentities(c *completion) error {
	if c.ID == "" {
		id, err := newCompletionID()
		if err != nil {
			return fmt.Errorf("protocol translation: could not generate response identity")
		}
		c.ID = id
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().Unix()
	}
	used := make(map[string]struct{})
	for _, b := range c.Blocks {
		if b.Tool != nil && b.Tool.CallID != "" {
			used[b.Tool.CallID] = struct{}{}
		}
	}
	for i, b := range c.Blocks {
		if b.Tool == nil || b.Tool.CallID != "" {
			continue
		}
		id := completionGeneratedCallID(c.ID, i)
		if _, exists := used[id]; exists {
			return unsupported("generated tool-call identity collision")
		}
		b.Tool.CallID = id
		used[id] = struct{}{}
	}
	return nil
}

func newCompletionID() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("resp_torana_%x", entropy[:]), nil
}

// completionGeneratedCallID intentionally matches streaming's response-ID /
// block-index derivation. It is stable for a provider response and cannot
// repeat merely because Gemini omitted call IDs on separate turns.
func completionGeneratedCallID(responseID string, index int) string {
	sum := sha256.Sum256([]byte(responseID))
	return fmt.Sprintf("call_torana_%x_%d", sum[:6], index)
}

func completionGeneratedItemID(kind, responseID string, index int) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + responseID))
	return fmt.Sprintf("%s_torana_%x_%d", kind, sum[:6], index)
}

func validateDestinationToolIdentities(p Protocol, c *completion) error {
	for _, b := range c.Blocks {
		if b.Tool == nil {
			continue
		}
		if err := validateToolIdentity(p, b.Tool.Name, b.Tool.CallID); err != nil {
			return err
		}
	}
	return nil
}

// validateToolIdentity enforces the destination wire grammar without changing
// caller/provider identities. An empty id validates a definition name only.
// The 64-byte name bound is the portable common subset of supported contracts.
func validateToolIdentity(p Protocol, name, id string) error {
	if !portableToolName(name) {
		return unsupported(string(p) + " tool-call name grammar")
	}
	if (p == Gemini || p == GeminiCodeAssist) && !asciiLetterOrUnderscore(name[0]) {
		return unsupported(string(p) + " tool-call name grammar")
	}
	if id == "" {
		return nil
	}
	if p == Anthropic {
		if !portableToolID(id) {
			return unsupported("anthropic tool-call id grammar")
		}
		return nil
	}
	if !portableOpaqueID(id) {
		return unsupported(string(p) + " tool-call id grammar")
	}
	return nil
}

func portableToolName(name string) bool {
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func asciiLetterOrUnderscore(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func portableToolID(id string) bool {
	if id == "" || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func portableOpaqueID(id string) bool {
	if id == "" || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func ensureCallID(id string, index int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("call_%d", index)
}

func chatFinish(finish string) string {
	if finish == "tool_calls" {
		return "tool_calls"
	}
	return finish
}

func responseItemStatus(itemStatus, responseStatus string) string {
	if itemStatus == "completed" || itemStatus == "incomplete" {
		return itemStatus
	}
	return responseStatus
}
