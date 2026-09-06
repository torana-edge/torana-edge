package format_test

// The fidelity contract, one case per fact a caller can send.
//
// Each entry is a real provider wire shape. Where a case documents a bug this
// tree actually shipped, the comment says so — those are the ones that reached
// a provider as an empty string with no error to anybody.

const marker = "IMPORTANT_PAYLOAD_a7f3"

var fidelityCases = []fidelityCase{
	// ---------------------------------------------------------------------
	// OpenAI Chat Completions
	// ---------------------------------------------------------------------
	{
		// The tool-role branch replaced the whole block list and read only the
		// SCALAR content, so an array-form tool result reached the provider as
		// content:"". The model was told the tool returned nothing.
		name:   "tool result with array content",
		format: "openai",
		body: `{"model":"m","messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"` + marker + `"}]}
		]}`,
		survives: []string{marker},
	},
	{
		name:   "tool result with multi-part array content",
		format: "openai",
		body: `{"model":"m","messages":[
			{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"` + marker + `"},{"type":"text","text":"SECOND_PART"}]}
		]}`,
		survives: []string{marker, "SECOND_PART"},
	},
	{
		name:     "tool result with scalar content",
		format:   "openai",
		body:     `{"model":"m","messages":[{"role":"tool","tool_call_id":"c1","content":"` + marker + `"}]}`,
		survives: []string{marker},
	},
	{
		// OpenAI puts `strict` inside `function`. It was read and written one
		// level up, so a caller's strict schema silently stopped being strict:
		// dropped on the way in, and emitted where the provider ignores it.
		name:   "tool definition strict flag",
		format: "openai",
		body: `{"model":"m","messages":[{"role":"user","content":"u"}],
			"tools":[{"type":"function","function":{"name":"f","description":"d",
			"parameters":{"type":"object","properties":{}},"strict":true}}]}`,
		paths: map[string]any{
			"tools[0].function.name":   "f",
			"tools[0].function.strict": true,
		},
		absentPaths: []string{"tools[0].strict"},
	},
	{
		name:   "tool definition without strict stays without it",
		format: "openai",
		body: `{"model":"m","messages":[{"role":"user","content":"u"}],
			"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
		absentPaths: []string{"tools[0].strict", "tools[0].function.strict"},
	},
	{
		// The chat wire carries a tool name alongside tool_call_id. The IR has
		// had a home for it (RequestToolResultBlock.tool_name) all along; this
		// adapter simply never populated or emitted it.
		name:     "tool result name",
		format:   "openai",
		body:     `{"model":"m","messages":[{"role":"tool","tool_call_id":"c1","name":"my_tool","content":"ok"}]}`,
		survives: []string{"my_tool"},
	},
	{
		// Reasoning content was built into a block and then thrown away by the
		// tool-role branch replacing the block list wholesale.
		name:     "tool result alongside reasoning content",
		format:   "openai",
		body:     `{"model":"m","messages":[{"role":"tool","tool_call_id":"c1","content":"` + marker + `","reasoning_content":"BECAUSE_REASON"}]}`,
		survives: []string{marker, "BECAUSE_REASON"},
	},
	{
		// A valid OpenAI shape that produced zero blocks and then tripped the
		// SDK's "at least one block" rule as a 400 the caller could not act on.
		name:   "assistant message with no content",
		format: "openai",
		body:   `{"model":"m","messages":[{"role":"assistant"}]}`,
		paths:  map[string]any{"messages[0].role": "assistant"},
	},
	{
		name:   "user message with empty content array",
		format: "openai",
		body:   `{"model":"m","messages":[{"role":"user","content":[]}]}`,
		paths:  map[string]any{"messages[0].role": "user"},
	},
	{
		// A stop sequence the caller set must not be quietly rewritten. A
		// non-string element used to be dropped on the floor.
		name:   "stop sequences with a non-string element are refused",
		format: "openai",
		body: `{"model":"m","messages":[{"role":"user","content":"u"}],
			"stop":["A",5,"B"]}`,
		wantUnmarshalErr: true,
	},
	{
		name:   "stop sequences as a scalar string",
		format: "openai",
		body:   `{"model":"m","messages":[{"role":"user","content":"u"}],"stop":"END"}`,
		paths:  map[string]any{"stop[0]": "END"},
	},
	{
		name:   "stop sequences as an array of strings",
		format: "openai",
		body:   `{"model":"m","messages":[{"role":"user","content":"u"}],"stop":["A","B"]}`,
		paths:  map[string]any{"stop[0]": "A", "stop[1]": "B"},
	},
	{
		// Substituting a model sends a different, differently-priced request
		// than the caller asked for and misattributes every downstream cost and
		// metric. Refusing would be wrong too: docs/LOCAL_MODELS.md documents
		// single-model endpoints (Ollama, vLLM) that accept a request without
		// one. Forward exactly what the caller sent.
		name:        "absent model is forwarded absent, not invented",
		format:      "openai",
		body:        `{"messages":[{"role":"user","content":"u"}]}`,
		absentPaths: []string{"model"},
	},
	{
		name:     "unknown top-level fields pass through",
		format:   "openai",
		body:     `{"model":"m","messages":[{"role":"user","content":"u"}],"seed":42,"parallel_tool_calls":false}`,
		paths:    map[string]any{"seed": 42, "parallel_tool_calls": false},
		survives: []string{"seed"},
	},
	{
		name:     "image content part survives",
		format:   "openai",
		body:     `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		survives: []string{"https://x/y.png", "image_url"},
	},
	{
		// Position IS content: an image the caller placed before its caption
		// means something different after it. Text and unknown parts used to
		// accumulate in separate buckets concatenated text-first, so every
		// mixed content array came back reordered.
		name:   "content array keeps caller order across part kinds",
		format: "openai",
		body: `{"model":"m","messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"https://x/one.png"}},
			{"type":"text","text":"A"},
			{"type":"image_url","image_url":{"url":"https://x/two.png"}},
			{"type":"text","text":"B"}
		]}]}`,
		paths: map[string]any{
			"messages[0].content[0].type":          "image_url",
			"messages[0].content[0].image_url.url": "https://x/one.png",
			"messages[0].content[1].type":          "text",
			"messages[0].content[1].text":          "A",
			"messages[0].content[2].type":          "image_url",
			"messages[0].content[2].image_url.url": "https://x/two.png",
			"messages[0].content[3].type":          "text",
			"messages[0].content[3].text":          "B",
		},
	},
	{
		// The same reordering on the tool-result path: a screenshot followed
		// by the text explaining it arrived at the model explanation-first.
		name:   "tool result keeps caller order across part kinds",
		format: "openai",
		body: `{"model":"m","messages":[{"role":"tool","tool_call_id":"c1","content":[
			{"type":"image_url","image_url":{"url":"https://x/shot.png"}},
			{"type":"text","text":"after"}
		]}]}`,
		paths: map[string]any{
			"messages[0].content[0].type":          "image_url",
			"messages[0].content[0].image_url.url": "https://x/shot.png",
			"messages[0].content[1].type":          "text",
			"messages[0].content[1].text":          "after",
		},
	},
	{
		// The scalar-string shortcut applies only when the ordered projection
		// is exactly ONE text part. A lone non-text part stays an array — it
		// has no scalar spelling.
		name:   "lone non-text part stays an array",
		format: "openai",
		body:   `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		paths: map[string]any{
			"messages[0].content[0].type": "image_url",
		},
	},
	{
		name:     "large tool-call integer arguments keep their lexeme",
		format:   "openai",
		body:     `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"n\":12345678901234567890}"}}]}]}`,
		survives: []string{"12345678901234567890"},
	},

	// ---------------------------------------------------------------------
	// OpenAI Responses
	// ---------------------------------------------------------------------
	{
		// An input the adapter cannot represent was silently replaced with
		// null, destroying the conversation rather than refusing the request.
		name:             "unrepresentable input is refused, not nulled",
		format:           "openai",
		body:             `{"model":"m","input":42}`,
		wantUnmarshalErr: true,
	},
	{
		name:     "string input survives",
		format:   "openai",
		body:     `{"model":"m","input":"` + marker + `"}`,
		survives: []string{marker},
	},

	// ---------------------------------------------------------------------
	// Anthropic
	// ---------------------------------------------------------------------
	{
		// marshalUnknownBlock rebuilt the block from the typed contentBlock,
		// which has no source field — so every image and document reached the
		// provider as a bare {"type":"image"}, its attachment gone, with no
		// error to anybody.
		name:   "image block keeps its source",
		format: "anthropic",
		body: `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + marker + `"}},
			{"type":"text","text":"describe it"}]}]}`,
		survives: []string{marker, "image/png", "base64"},
	},
	{
		name:   "document block keeps its source",
		format: "anthropic",
		body: `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
			{"type":"document","source":{"type":"text","media_type":"text/plain","data":"` + marker + `"}}]}]}`,
		survives: []string{marker, "text/plain"},
	},
	{
		// A cache breakpoint closes the prefix at the block it follows, which
		// may be an unmodelled arm. Emitting those verbatim must not lose the
		// marker, or the provider cache is disabled for the whole prefix.
		name:   "cache_control after an image block survives",
		format: "anthropic",
		body: `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + marker + `"},"cache_control":{"type":"ephemeral"}}]}]}`,
		survives: []string{marker, "ephemeral"},
	},
	{
		name:   "absent model is forwarded absent, not invented",
		format: "anthropic",
		body: `{"max_tokens":10,"messages":[{"role":"user","content":[
			{"type":"text","text":"u"}]}]}`,
		absentPaths: []string{"model"},
	},
	{
		// Anthropic requires max_tokens; that is the provider's rule to
		// enforce. Inventing 4096 caps a response the caller never capped.
		name:        "absent max_tokens is not invented",
		format:      "anthropic",
		body:        `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u"}]}]}`,
		absentPaths: []string{"max_tokens"},
	},
	{
		// Anthropic tool_result has no name member, and this adapter's own arm
		// table rejects one on the way in — so emitting it produced a body the
		// parser would refuse and the provider would reject.
		name:   "tool result carries no name member",
		format: "anthropic",
		body: `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`,
		absentPaths: []string{"messages[0].content[0].name"},
	},
	{
		name:     "thinking block round-trips with its signature",
		format:   "anthropic",
		body:     `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + marker + `","signature":"SIG"}]}]}`,
		survives: []string{marker, "SIG"},
	},
	{
		name:     "cache_control on a tool definition survives",
		format:   "anthropic",
		body:     `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"u"}],"tools":[{"name":"f","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]}`,
		survives: []string{"ephemeral"},
	},

	// ---------------------------------------------------------------------
	// Gemini — no known losses; these pin the behaviour the others regressed.
	// ---------------------------------------------------------------------
	{
		name:     "inline image data survives",
		format:   "gemini",
		body:     `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + marker + `"}},{"text":"describe"}]}]}`,
		survives: []string{marker, "image/png"},
	},
	{
		name:     "function response survives",
		format:   "gemini",
		body:     `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"ls","response":{"out":"` + marker + `"}}}]}]}`,
		survives: []string{marker, "ls"},
	},
}
