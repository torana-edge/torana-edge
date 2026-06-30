package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// --- Request hook: schema injection -----------------------------------------

func TestCompactorBeforeRequestInjectsAnthropicShape(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"model": "claude",
		"tools": [{
			"name": "Bash",
			"input_schema": {
				"type": "object",
				"properties": {"command": {"type": "string"}},
				"required": ["command"]
			}
		}]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	got, err := c.BeforeRequest(req, body)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}

	var payload map[string]any
	json.Unmarshal(got, &payload)
	tool := payload["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	props := schema["properties"].(map[string]any)

	if _, ok := props["_torana_extraction_intent"]; !ok {
		t.Fatal("intent not injected into properties")
	}
	required := schema["required"].([]any)
	found := false
	for _, r := range required {
		if s, _ := r.(string); s == "_torana_extraction_intent" {
			found = true
		}
	}
	if !found {
		t.Fatal("intent not added to required")
	}
}

func TestCompactorBeforeRequestInjectsOpenAIShape(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"model": "gpt",
		"tools": [{
			"type": "function",
			"function": {
				"name": "execute_command",
				"parameters": {
					"type": "object",
					"properties": {"command": {"type": "string"}},
					"required": ["command"]
				}
			}
		}]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	got, err := c.BeforeRequest(req, body)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}

	var payload map[string]any
	json.Unmarshal(got, &payload)
	fn := payload["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	if _, ok := params["properties"].(map[string]any)["_torana_extraction_intent"]; !ok {
		t.Fatal("intent not injected in OpenAI parameters")
	}
}

func TestCompactorBeforeRequestSkipsNonTerminal(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"tools": [{"name": "get_weather", "input_schema": {"properties": {"city": {"type": "string"}}}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	got, err := c.BeforeRequest(req, body)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}
	if !bytes.Equal(body, got) {
		t.Fatal("non-terminal tool should pass through unchanged")
	}
}

func TestCompactorBeforeRequestIdempotent(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"tools": [{"name": "Bash", "input_schema": {
			"type": "object",
			"properties": {"command": {"type": "string"}},
			"required": ["command"]
		}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	first, _ := c.BeforeRequest(req, body)
	second, _ := c.BeforeRequest(req, first)
	if !bytes.Equal(first, second) {
		t.Fatal("second pass changed the body — not idempotent")
	}
}

func TestCompactorBeforeRequestInjectsSoftPrompt(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"tools": [{
			"name": "Bash",
			"description": "Execute a bash command and return its output.",
			"input_schema": {
				"type": "object",
				"properties": {"command": {"type": "string"}},
				"required": ["command"]
			}
		}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	got, err := c.BeforeRequest(req, body)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}

	var payload map[string]any
	json.Unmarshal(got, &payload)
	tool := payload["tools"].([]any)[0].(map[string]any)

	desc, _ := tool["description"].(string)
	if !strings.Contains(desc, "Before calling this tool") {
		t.Fatal("soft prompt not appended to description")
	}
	if !strings.Contains(desc, "<torana_intent>") {
		t.Fatal("soft prompt missing <torana_intent> tag")
	}
	schema := tool["input_schema"].(map[string]any)
	if _, ok := schema["properties"].(map[string]any)["_torana_extraction_intent"]; !ok {
		t.Fatal("parameter injection missing — both V1 and V2 should apply")
	}
}

func TestSoftPromptIdempotent(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"tools": [{
			"name": "Bash",
			"description": "Execute a bash command.",
			"input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}}
		}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	first, _ := c.BeforeRequest(req, body)
	second, _ := c.BeforeRequest(req, first)
	if !bytes.Equal(first, second) {
		t.Fatal("double injection should be idempotent")
	}
}

func TestSoftPromptSkipsEmptyDescription(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"tools": [{
			"name": "Bash",
			"input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}}
		}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	got, err := c.BeforeRequest(req, body)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}
	var payload map[string]any
	json.Unmarshal(got, &payload)
	tool := payload["tools"].([]any)[0].(map[string]any)
	if desc, ok := tool["description"].(string); ok && strings.Contains(desc, "Before calling this tool") {
		t.Fatal("soft prompt should not be injected when description is missing")
	}
}

func TestSystemPromptInjectionAnthropic(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"system": [{"type": "text", "text": "You are a helpful assistant."}],
		"tools": [{"name": "Bash", "description": "Run cmd.", "input_schema": {
			"type": "object", "properties": {"cmd": {"type": "string"}}
		}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	got, err := c.BeforeRequest(req, body)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}

	var payload map[string]any
	json.Unmarshal(got, &payload)
	sys := payload["system"].([]any)
	last := sys[len(sys)-1].(map[string]any)

	if !strings.Contains(last["text"].(string), "[TORANA SYSTEM INSTRUCTION]") {
		t.Fatal("system prompt not injected")
	}
}

func TestSystemPromptInjectionOpenAI(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "hello"}
		],
		"tools": [{"type": "function", "function": {
			"name": "Bash",
			"parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}
		}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	got, err := c.BeforeRequest(req, body)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}

	var payload map[string]any
	json.Unmarshal(got, &payload)
	msgs := payload["messages"].([]any)
	sys := msgs[0].(map[string]any)

	if !strings.Contains(sys["content"].(string), "[TORANA SYSTEM INSTRUCTION]") {
		t.Fatal("system prompt not injected in OpenAI shape")
	}
}

func TestSystemPromptInjectionIdempotent(t *testing.T) {
	c := NewCompactor()
	body := []byte(`{
		"system": [{"type": "text", "text": "You are helpful."}],
		"tools": [{"name": "Bash", "description": "Run cmd.", "input_schema": {
			"type": "object", "properties": {"cmd": {"type": "string"}}
		}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	first, _ := c.BeforeRequest(req, body)
	second, _ := c.BeforeRequest(req, first)
	if !bytes.Equal(first, second) {
		t.Fatal("system prompt injection should be idempotent")
	}
}

func TestCompactorBeforeRequestEmptyBody(t *testing.T) {
	c := NewCompactor()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	got, err := c.BeforeRequest(req, nil)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}
	if got != nil {
		t.Fatal("nil body should stay nil")
	}
}

// --- Response hook: non-SSE pass-through ------------------------------------

func TestCompactorAfterResponseNonSSE(t *testing.T) {
	c := NewCompactor()
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}}
	original := io.NopCloser(strings.NewReader(`{"ok":true}`))

	wrapped, err := c.AfterResponse(resp, original, nil, nil)
	if err != nil {
		t.Fatalf("AfterResponse: %v", err)
	}
	data, _ := io.ReadAll(wrapped)
	if string(data) != `{"ok":true}` {
		t.Fatal("non-SSE body should pass through unchanged")
	}
}

// --- Response hook: buffered SSE scanning -----------------------------------

func TestBufferedSSEScanFindsV1Intent(t *testing.T) {
	c := NewCompactor()

	// Tool_use with V1 intent parameter.
	sseStream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_001","name":"Bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\",\"_torana_extraction_intent\":\"find the error line\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
	}, "\n")

	resp := &http.Response{
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		StatusCode: http.StatusOK,
	}

	wrapped, err := c.AfterResponse(resp, io.NopCloser(strings.NewReader(sseStream)), nil, nil)
	if err != nil {
		t.Fatalf("AfterResponse: %v", err)
	}
	data, _ := io.ReadAll(wrapped)
	wrapped.Close()

	// Stream should be replayed verbatim.
	if !strings.Contains(string(data), "_torana_extraction_intent") {
		t.Fatal("buffered replay missing original content")
	}
	// Intent should be cached.
	if c.GetIntent("toolu_001") != "find the error line" {
		t.Fatalf("cached intent = %q, want %q", c.GetIntent("toolu_001"), "find the error line")
	}
}

func TestBufferedSSEScanFindsXMLIntent(t *testing.T) {
	c := NewCompactor()

	sseStream := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<torana_intent>find the NPE stack trace</torana_intent>"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_x","name":"Read","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/tmp/log\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
	}, "\n")

	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	wrapped, _ := c.AfterResponse(resp, io.NopCloser(strings.NewReader(sseStream)), nil, nil)
	io.ReadAll(wrapped)
	wrapped.Close()

	if c.GetIntent("toolu_x") != "find the NPE stack trace" {
		t.Fatalf("XML intent = %q", c.GetIntent("toolu_x"))
	}
}

func TestBufferedSSEScanNoIntent(t *testing.T) {
	c := NewCompactor()

	// Tool_use without any intent. This should trigger a retry attempt,
	// but since we pass nil for req/body, retryWithError will return empty.
	sseStream := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"Bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
	}, "\n")

	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	// req=nil, body=nil → retryWithError will detect nil toolInfo and return empty.
	wrapped, _ := c.AfterResponse(resp, io.NopCloser(strings.NewReader(sseStream)), nil, nil)
	data, _ := io.ReadAll(wrapped)
	wrapped.Close()

	// No intent should be cached.
	if c.GetIntent("toolu_bad") != "" {
		t.Fatal("no intent should be cached when none provided")
	}
	// Since req is nil, retry can't execute; returns empty.
	if len(data) != 0 {
		t.Logf("retry fallback returned %d bytes", len(data))
	}
}

// --- Retry message injection -------------------------------------------------

func TestInjectRetryMessages(t *testing.T) {
	body := []byte(`{
		"model": "claude",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "list files"}]},
			{"role": "assistant", "content": [{"type": "text", "text": "I'll run ls."}]}
		]
	}`)

	toolInfo := &toolUseInfo{
		ID:    "toolu_001",
		Name:  "Bash",
		Input: `{"command":"ls -la"}`,
	}

	got, err := injectRetryMessages(body, toolInfo)
	if err != nil {
		t.Fatalf("injectRetryMessages: %v", err)
	}

	var payload map[string]any
	json.Unmarshal(got, &payload)
	msgs := payload["messages"].([]any)

	// Should still have 2 messages (injected into existing, didn't add new).
	if len(msgs) != 2 {
		t.Fatalf("messages count = %d, want 2 (inject into existing)", len(msgs))
	}

	// Last assistant should have tool_use appended.
	assistant := msgs[1].(map[string]any)
	content := assistant["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("assistant content blocks = %d, want 2", len(content))
	}
	toolBlock := content[1].(map[string]any)
	if toolBlock["type"] != "tool_use" {
		t.Fatal("second content block should be tool_use")
	}
	if toolBlock["id"] != "toolu_001" {
		t.Fatalf("tool_use id = %q", toolBlock["id"])
	}

	// Last user message should have tool_result prepended to its content.
	user := msgs[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatal("first message should be user")
	}
	userContent := user["content"].([]any)
	if len(userContent) < 1 {
		t.Fatal("user content should have at least the tool_result")
	}
	tr := userContent[0].(map[string]any)
	if tr["type"] != "tool_result" {
		t.Fatal("first block should be tool_result")
	}
	if tr["is_error"] != true {
		t.Fatal("tool_result should be is_error:true")
	}
	if !strings.Contains(tr["content"].(string), ExtractionIntentKey) {
		t.Fatal("error message should mention the missing parameter")
	}
}

// --- Cache operations -------------------------------------------------------

func TestCompactorCacheOperations(t *testing.T) {
	c := NewCompactor()

	if got := c.GetIntent("missing"); got != "" {
		t.Fatal("missing key should return empty string")
	}
	c.CacheIntent("id1", "find the bug")
	if got := c.GetIntent("id1"); got != "find the bug" {
		t.Fatalf("GetIntent = %q", got)
	}
}

func TestCompactorCacheThreadSafety(t *testing.T) {
	c := NewCompactor()
	done := make(chan struct{})

	go func() {
		for i := 0; i < 1000; i++ {
			c.CacheIntent("key", "value")
			c.GetIntent("key")
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 1000; i++ {
			c.CacheIntent("key", "value")
			c.GetIntent("key")
		}
		done <- struct{}{}
	}()

	<-done
	<-done
}

// --- Adapter tests ----------------------------------------------------------

func TestAdapterSetsHostHeader(t *testing.T) {
	a := NewAdapter(mustParseURL("https://api.deepseek.com"))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Host = "localhost:8080"

	_, err := a.BeforeRequest(req, []byte(`{}`))
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}
	if req.Host != "api.deepseek.com" {
		t.Fatalf("Host = %q", req.Host)
	}
}

func TestAdapterDoesNotTouchBody(t *testing.T) {
	a := NewAdapter(mustParseURL("https://api.openai.com"))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	original := []byte(`{"model":"gpt"}`)

	got, err := a.BeforeRequest(req, original)
	if err != nil {
		t.Fatalf("BeforeRequest: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("adapter should not mutate the body")
	}
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
