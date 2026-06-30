package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

var TerminalToolNames = map[string]bool{
	"Bash":            true,
	"Read":            true,
	"execute_command": true,
	"read_file":       true,
	"view_file":       true,
}

const ExtractionIntentKey = "_torana_extraction_intent"
const xmlTagOpen = "<torana_intent>"
const xmlTagClose = "</torana_intent>"

const extractionIntentDescription = "CRITICAL: Explicitly specify the exact error string, code block, " +
	"or data points you are looking to find in the output of this command to satisfy the user request."

const softPromptSuffix = "\n\n[SYSTEM] Before calling this tool, you MUST declare your extraction " +
	"intent in your text response using a <torana_intent>...</torana_intent> XML block. " +
	"In it, describe exactly what specific information, error, file path, or data you expect " +
	"to find in the tool's output. The proxy uses this to optimize context windows. " +
	"If you omit this block, the tool output may be truncated and your task may fail. " +
	"Example: <torana_intent>find the stack trace for NullPointerException</torana_intent>"

const systemPromptInstruction = "\n\n[TORANA SYSTEM INSTRUCTION] Before calling any terminal tool " +
	"(Bash, Read, execute_command, read_file, view_file), you MUST declare your extraction intent " +
	"in your text response BEFORE the tool call, using <torana_intent>...</torana_intent> XML tags. " +
	"Describe exactly what specific information, error message, code block, file path, or data you " +
	"are looking for in the tool's output. The proxy intercepts this to compact large tool results " +
	"for context window optimization. Example: <torana_intent>find the NullPointerException " +
	"stack trace in the build output</torana_intent>"

// ---------------------------------------------------------------------------
// Compactor module
// ---------------------------------------------------------------------------

// Compactor implements RequestHook and ResponseHook. The thread-safe cache
// maps tool_use_id → intent string.
type Compactor struct {
	mu    sync.RWMutex
	cache map[string]string
}

func NewCompactor() *Compactor {
	return &Compactor{cache: make(map[string]string)}
}

func (c *Compactor) Name() string { return "compactor" }

func (c *Compactor) CacheIntent(toolUseID, intent string) {
	c.mu.Lock()
	c.cache[toolUseID] = intent
	c.mu.Unlock()
}

func (c *Compactor) GetIntent(toolUseID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache[toolUseID]
}

// ---------------------------------------------------------------------------
// Request hook – V1/V2/V3 prompts
// ---------------------------------------------------------------------------

func (c *Compactor) BeforeRequest(req *http.Request, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	newBody, mutated, err := injectExtractionIntent(body)
	if err != nil {
		return body, fmt.Errorf("compactor inject: %w", err)
	}
	if mutated {
		log.Printf("  [compactor] injected intent prompts [%d → %d bytes]", len(body), len(newBody))
	}
	return newBody, nil
}

// ---------------------------------------------------------------------------
// Response hook – buffer, scan, retry
// ---------------------------------------------------------------------------

func (c *Compactor) AfterResponse(resp *http.Response, body io.ReadCloser,
	originalReq *http.Request, originalBody []byte) (io.ReadCloser, error) {

	if !isEventStream(resp) {
		return body, nil
	}

	log.Printf("  [compactor] buffering SSE stream for intent scan")
	raw, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return nil, fmt.Errorf("compactor: read SSE: %w", err)
	}

	hasIntent, toolInfo := scanBufferedSSE(raw, c)
	if hasIntent {
		log.Printf("  [compactor] intent found — replaying buffered stream (%d bytes)", len(raw))
		return io.NopCloser(bytes.NewReader(raw)), nil
	}

	// No intent. Synchronous retry.
	log.Printf("  [compactor] no intent — initiating sync retry")
	return c.retryWithError(originalReq, originalBody, toolInfo)
}

// ---------------------------------------------------------------------------
// SSE buffered scanner
// ---------------------------------------------------------------------------

// toolUseInfo captures the data needed to reconstruct a tool_use for retry.
type toolUseInfo struct {
	ID    string
	Name  string
	Input string // JSON object string
}

// scanBufferedSSE parses a complete SSE byte slice looking for intent.
// Returns whether any intent was found, plus the first failed terminal
// tool_use for retry purposes.
func scanBufferedSSE(data []byte, c *Compactor) (bool, *toolUseInfo) {
	var (
		textBuf       strings.Builder
		pendingIntent string
		inToolUse     bool
		toolID        string
		toolName      string
		inputBuf      strings.Builder
		firstFailed   *toolUseInfo
	)

	events := bytes.Split(data, []byte("\n\n"))
	for _, event := range events {
		eventStr := string(event)
		var dataLine string
		for _, line := range strings.Split(eventStr, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		if dataLine == "" {
			continue
		}

		var ev map[string]any
		if err := json.Unmarshal([]byte(dataLine), &ev); err != nil {
			continue
		}
		typ, _ := ev["type"].(string)

		switch typ {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			cbType, _ := cb["type"].(string)

			if cbType == "text" {
				textBuf.Reset()
			}
			if cbType == "tool_use" {
				inToolUse = true
				toolID, _ = cb["id"].(string)
				toolName, _ = cb["name"].(string)
				inputBuf.Reset()
				log.Printf("  [compactor]   tool_use: name=%s id=%s terminal=%v",
					toolName, toolID, TerminalToolNames[toolName])

				if pendingIntent != "" {
					c.CacheIntent(toolID, pendingIntent)
					log.Printf("  [compactor] cached xml-intent for %s: %q", toolID, truncate(pendingIntent, 80))
					pendingIntent = ""
				}
			}

		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			if text, ok := delta["text"].(string); ok {
				textBuf.WriteString(text)
				if pi := scanTextBuf(textBuf.String()); pi != "" {
					pendingIntent = pi
				}
			} else if td, ok := delta["text_delta"].(map[string]any); ok {
				if text, ok := td["text"].(string); ok {
					textBuf.WriteString(text)
					if pi := scanTextBuf(textBuf.String()); pi != "" {
						pendingIntent = pi
					}
				}
			}
			if inToolUse {
				if partial, ok := delta["partial_json"].(string); ok {
					inputBuf.WriteString(partial)
				}
			}

		case "content_block_stop":
			if pi := scanTextBuf(textBuf.String()); pi != "" {
				pendingIntent = pi
			}
			if inToolUse {
				inToolUse = false
				// Check V1 intent from assembled input.
				if c.GetIntent(toolID) == "" {
					var input map[string]any
					if err := json.Unmarshal([]byte(inputBuf.String()), &input); err == nil {
						if v, ok := input[ExtractionIntentKey].(string); ok && v != "" {
							c.CacheIntent(toolID, v)
							log.Printf("  [compactor] cached v1-intent for %s: %q", toolID, truncate(v, 80))
						}
					}
				}
				// Record first failed terminal tool for retry.
				notCached := c.GetIntent(toolID) == ""
				isTerminal := TerminalToolNames[toolName]
				if notCached && isTerminal && firstFailed == nil {
					firstFailed = &toolUseInfo{
						ID: toolID, Name: toolName, Input: inputBuf.String(),
					}
					log.Printf("  [compactor]   recording firstFailed: %s/%s", toolName, toolID)
				}
			}
		}
	}

	// Determine overall result: was any intent cached for any tool?
	hasIntent := pendingIntent != "" || c.GetIntent(toolID) != "" // Last tool had intent
	return hasIntent, firstFailed
}

func scanTextBuf(text string) string {
	start := strings.Index(text, xmlTagOpen)
	if start < 0 {
		return ""
	}
	start += len(xmlTagOpen)
	end := strings.Index(text[start:], xmlTagClose)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

// ---------------------------------------------------------------------------
// Synchronous retry
// ---------------------------------------------------------------------------

// retryWithError builds a new request with the failed tool_use + error
// tool_result appended to messages, sends it upstream, and returns the
// new response body.
func (c *Compactor) retryWithError(req *http.Request, originalBody []byte,
	toolInfo *toolUseInfo) (io.ReadCloser, error) {

	if toolInfo == nil || req == nil || originalBody == nil {
		// No terminal tool to retry, or no original request available.
		log.Printf("  [compactor] cannot retry — no tool (%v) or no req (%v)",
			toolInfo == nil, req == nil)
		return io.NopCloser(strings.NewReader("")), nil
	}

	// Mutate the request body.
	newBody, err := injectRetryMessages(originalBody, toolInfo)
	if err != nil {
		return nil, fmt.Errorf("compactor retry inject: %w", err)
	}
	log.Printf("  [compactor] retry body [%d → %d bytes]", len(originalBody), len(newBody))

	// Build the retry request.
	upstreamURL := req.URL.String()
	retryReq, err := http.NewRequest(http.MethodPost, upstreamURL, bytes.NewReader(newBody))
	if err != nil {
		return nil, fmt.Errorf("compactor retry new-request: %w", err)
	}

	// Copy relevant headers.
	for k, vs := range req.Header {
		for _, v := range vs {
			retryReq.Header.Add(k, v)
		}
	}
	retryReq.ContentLength = int64(len(newBody))

	// Execute.
	client := &http.Client{}
	retryResp, err := client.Do(retryReq)
	if err != nil {
		return nil, fmt.Errorf("compactor retry do: %w", err)
	}

	log.Printf("  [compactor] retry response: %d %s", retryResp.StatusCode, retryResp.Status)
	if retryResp.StatusCode >= 400 {
		body, _ := io.ReadAll(retryResp.Body)
		retryResp.Body.Close()
		log.Printf("  [compactor] retry error body: %s", truncate(string(body), 500))
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	return retryResp.Body, nil
}

// injectRetryMessages appends the failed tool_use to the last assistant
// message and injects an error tool_result into the last user message.
// Uses existing messages (doesn't insert new ones) to keep the Anthropic
// alternating-role invariant intact.
func injectRetryMessages(body []byte, toolInfo *toolUseInfo) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, fmt.Errorf("parse body: %w", err)
	}

	messages, _ := payload["messages"].([]any)

	// Find last assistant message and append the fake tool_use.
	var lastAssistant map[string]any
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "assistant" {
			lastAssistant = msg
			break
		}
	}
	if lastAssistant == nil {
		return body, fmt.Errorf("no assistant message found")
	}

	content, _ := lastAssistant["content"].([]any)
	lastAssistant["content"] = append(content, map[string]any{
		"type":  "tool_use",
		"id":    toolInfo.ID,
		"name":  toolInfo.Name,
		"input": json.RawMessage(toolInfo.Input),
	})

	// Find the last user message and prepend the error tool_result.
	errorContent := fmt.Sprintf(
		"SYSTEM ERROR: You MUST include the \"%s\" parameter in your tool call "+
			"JSON. Please retry the exact same tool call but include \"%s\" with a "+
			"description of what specific data you are looking for in the output.",
		ExtractionIntentKey, ExtractionIntentKey,
	)
	toolResultBlock := map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolInfo.ID,
		"content":     errorContent,
		"is_error":    true,
	}

	var lastUser map[string]any
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			lastUser = msg
			break
		}
	}
	if lastUser == nil {
		return body, fmt.Errorf("no user message found")
	}

	userContent, _ := lastUser["content"].([]any)
	// Prepend so the fake tool_result appears first.
	lastUser["content"] = append([]any{toolResultBlock}, userContent...)

	payload["messages"] = messages

	result, err := json.Marshal(payload)
	if err != nil {
		return body, fmt.Errorf("marshal retry body: %w", err)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// JSON injection (V1/V2/V3)
// ---------------------------------------------------------------------------

func injectExtractionIntent(body []byte) ([]byte, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false, nil
	}
	tools, _ := payload["tools"].([]any)
	mutated := false

	if injectSystemPrompt(payload) {
		mutated = true
	}
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name := resolveToolName(tool)
		if !TerminalToolNames[name] {
			continue
		}
		if injectSoftPrompt(tool) {
			mutated = true
		}
		if schema, ok := tool["input_schema"].(map[string]any); ok {
			if injectIntoProperties(schema) {
				mutated = true
			}
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			if params, ok := fn["parameters"].(map[string]any); ok {
				if injectIntoProperties(params) {
					mutated = true
				}
			}
		}
	}
	if !mutated {
		return body, false, nil
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return body, false, fmt.Errorf("marshal mutated payload: %w", err)
	}
	return result, true, nil
}

func injectIntoProperties(schema map[string]any) bool {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		props = make(map[string]any)
		schema["properties"] = props
	}
	if _, exists := props[ExtractionIntentKey]; exists {
		return false
	}
	props[ExtractionIntentKey] = map[string]any{
		"type":        "string",
		"description": extractionIntentDescription,
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		if s, ok := r.(string); ok && s == ExtractionIntentKey {
			return false
		}
	}
	schema["required"] = append(required, ExtractionIntentKey)
	return true
}

func injectSoftPrompt(tool map[string]any) bool {
	desc, ok := tool["description"].(string)
	if !ok || desc == "" {
		return false
	}
	if strings.Contains(desc, "<torana_intent>") {
		return false
	}
	tool["description"] = desc + softPromptSuffix
	return true
}

func injectSystemPrompt(payload map[string]any) bool {
	if sysArr, ok := payload["system"].([]any); ok && len(sysArr) > 0 {
		if last, ok := sysArr[len(sysArr)-1].(map[string]any); ok {
			if text, ok := last["text"].(string); ok {
				if strings.Contains(text, "[TORANA SYSTEM INSTRUCTION]") {
					return false
				}
				last["text"] = text + systemPromptInstruction
				return true
			}
		}
	}
	if msgs, ok := payload["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := msg["role"].(string); role == "system" {
				if content, ok := msg["content"].(string); ok {
					if strings.Contains(content, "[TORANA SYSTEM INSTRUCTION]") {
						return false
					}
					msg["content"] = content + systemPromptInstruction
					return true
				}
			}
		}
	}
	return false
}

func resolveToolName(tool map[string]any) string {
	if n, ok := tool["name"].(string); ok && n != "" {
		return n
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok {
			return n
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// SSE helpers
// ---------------------------------------------------------------------------

func isEventStream(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
