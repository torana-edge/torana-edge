package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// anthropicPIIEnv stands up the REAL server with the REAL v2 pii bundle and an
// Anthropic-format provider. The upstream counts hits and returns a valid
// Anthropic response; a transport wrapper captures each outgoing request body
// (read and restored) before delegating to the real transport.
func anthropicPIIEnv(t *testing.T, piiCfg string) (
	post func(body string) (int, []byte),
	hits *int32,
	captured func() []string,
) {
	t.Helper()
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "pii")

	var hitCount int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn"}`))
	}))
	t.Cleanup(upstream.Close)

	providers := provider.Config{
		Providers: map[string]provider.Provider{
			"ant": {URL: upstream.URL, Format: "anthropic"},
		},
		Plugins: provider.PluginsConfig{
			Dir:             bundles,
			Order:           []string{"pii"},
			Config:          map[string]json.RawMessage{"pii": json.RawMessage(piiCfg)},
			AllowUnapproved: true,
		},
	}
	srv, err := New(Config{Port: "8080", Providers: providers})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	var (
		mu     sync.Mutex
		bodies []string
		capErr error
		origRT = srv.proxy.Transport
	)
	srv.proxy.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			// The transport goroutine cannot t.Fatal; record the failure and
			// assert it from the test goroutine.
			mu.Lock()
			capErr = err
			mu.Unlock()
			req.Body = io.NopCloser(bytes.NewReader(nil))
			return origRT.RoundTrip(req)
		}
		req.Body = io.NopCloser(bytes.NewReader(b))
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		return origRT.RoundTrip(req)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 30 * time.Second}
	post = func(body string) (int, []byte) {
		req, _ := http.NewRequest("POST", base+"/provider/ant/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	captured = func() []string {
		mu.Lock()
		defer mu.Unlock()
		if capErr != nil {
			t.Fatalf("capture ReadAll failed: %v", capErr)
		}
		return append([]string(nil), bodies...)
	}
	return post, &hitCount, captured
}

// anthropicToolResultConvo builds the EXACT Claude Code / Anthropic shape the
// v1 PII plugin mishandled: an assistant turn with a real tool_use block,
// immediately followed by a user turn whose tool_result content is an ARRAY
// of typed text blocks (v1 mapped this to a tool message with empty scalar
// Content and skipped it).
func anthropicToolResultConvo(toolResultContent any) string {
	return anthropicToolResultConvoWithSystem(
		[]map[string]any{
			{"type": "text", "text": "You are a coding agent. Read files and report their contents."},
		},
		toolResultContent)
}

// anthropicToolResultConvoWithSystem is anthropicToolResultConvo with an
// explicit system value, so the STRING system form (the historical parse
// bypass) can be exercised: a string system is a VALID Anthropic form that
// v1's adapter rejected, silently skipping every before-request plugin.
func anthropicToolResultConvoWithSystem(system any, toolResultContent any) string {
	b, _ := json.Marshal(map[string]any{
		"model":  "claude-3-5-sonnet",
		"system": system,
		"tools": []map[string]any{
			{
				"name":        "read_file",
				"description": "Read a file from the repository",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
		},
		"messages": []map[string]any{
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_1", "name": "read_file", "input": map[string]any{"path": "server.go"}},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "tu_1", "content": toolResultContent},
				},
			},
		},
	})
	return string(b)
}

func strptr(s string) *string { return &s }

// capturedBlock mirrors one Anthropic content block with the polymorphic
// fields preserved as raw JSON where nested.
type capturedBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// capturedMessage mirrors one Anthropic message.
type capturedMessage struct {
	Role    string          `json:"role"`
	Content []capturedBlock `json:"content"`
}

// capturedAnthropicRequest is the strict structural shape of the outgoing
// Anthropic request.
type capturedAnthropicRequest struct {
	Model     string            `json:"model"`
	MaxTokens *int              `json:"max_tokens,omitempty"`
	System    []capturedBlock   `json:"system"`
	Tools     []capturedToolDef `json:"tools"`
	Messages  []capturedMessage `json:"messages"`
}

type capturedToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// decodeCaptured decodes the captured request STRICTLY: unknown members and
// trailing JSON are rejected, so a topology change cannot hide behind extra
// fields or a second document. The trailing check is the normative
// second-decode/io.EOF test, not the array/object More() API.
func decodeCaptured(body string) (capturedAnthropicRequest, error) {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var cap capturedAnthropicRequest
	if err := dec.Decode(&cap); err != nil {
		return cap, err
	}
	// The normative trailing check: a real second Decode that must return
	// io.EOF. A nil error means a second JSON value was present.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return cap, fmt.Errorf("trailing JSON after the captured request: %v", err)
	}
	return cap, nil
}

// mustDecodeCaptured is the test-asserting wrapper for decodeCaptured.
func mustDecodeCaptured(t *testing.T, body string) capturedAnthropicRequest {
	t.Helper()
	cap, err := decodeCaptured(body)
	if err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	return cap
}

// decodeNested decodes ONE raw nested JSON value with the same strict
// contract as the top level: unknown members rejected, and a second decode
// that must return io.EOF. RawMessage members must never fall back to
// permissive json.Unmarshal.
func decodeNested(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing JSON in nested member: %v", err)
	}
	return nil
}

// checkCleanTopology pins the EXACT outgoing topology of the clean twin:
// one system text block, the read_file tool definition with its schema, an
// assistant message with exactly the expected tool_use (id, name, input),
// and a user message with exactly one tool_result bound to tu_1 whose nested
// content is exactly two typed text blocks in order with the exact strings.
// It returns an error so weakened-form rows can assert failure without a
// t.Fatal-based helper.
func checkCleanTopology(cap capturedAnthropicRequest) error {
	if len(cap.System) != 1 {
		return fmt.Errorf("system blocks = %d, want 1: %+v", len(cap.System), cap.System)
	}
	if err := validateBlockArm(&cap.System[0]); err != nil {
		return fmt.Errorf("system block: %v", err)
	}
	if cap.System[0].Text == nil || *cap.System[0].Text != "You are a coding agent. Read files and report their contents." {
		return fmt.Errorf("system block = %+v", cap.System[0])
	}

	if len(cap.Tools) != 1 || cap.Tools[0].Name != "read_file" || cap.Tools[0].Description != "Read a file from the repository" {
		return fmt.Errorf("tools = %+v", cap.Tools)
	}
	// The input schema is exact: type object, exactly the path property whose
	// own schema is exactly {type:"string"}, exactly the required list.
	var pathSchema struct {
		Type string `json:"type"`
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := decodeNested(cap.Tools[0].InputSchema, &schema); err != nil {
		return fmt.Errorf("tool input_schema: %v", err)
	}
	if schema.Type != "object" || len(schema.Properties) != 1 || len(schema.Required) != 1 || schema.Required[0] != "path" {
		return fmt.Errorf("tool input_schema = %+v", schema)
	}
	pathRaw, ok := schema.Properties["path"]
	if !ok {
		return fmt.Errorf("tool input_schema missing the path property: %+v", schema)
	}
	if err := decodeNested(pathRaw, &pathSchema); err != nil {
		return fmt.Errorf("path property schema: %v", err)
	}
	if pathSchema.Type != "string" {
		return fmt.Errorf("path property schema = %+v, want exactly {type:string}", pathSchema)
	}

	if len(cap.Messages) != 2 {
		return fmt.Errorf("messages = %d, want 2", len(cap.Messages))
	}
	assistant := cap.Messages[0]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 {
		return fmt.Errorf("assistant message = %+v", assistant)
	}
	tu := assistant.Content[0]
	if err := validateBlockArm(&tu); err != nil {
		return fmt.Errorf("tool_use block: %v", err)
	}
	if tu.Type != "tool_use" || tu.ID != "tu_1" || tu.Name != "read_file" {
		return fmt.Errorf("tool_use block = %+v", tu)
	}
	var input struct {
		Path string `json:"path"`
	}
	if err := decodeNested(tu.Input, &input); err != nil {
		return fmt.Errorf("tool_use input: %v", err)
	}
	if input.Path != "server.go" {
		return fmt.Errorf("tool_use input = %+v", input)
	}

	user := cap.Messages[1]
	if user.Role != "user" || len(user.Content) != 1 {
		return fmt.Errorf("user message = %+v", user)
	}
	tr := user.Content[0]
	if err := validateBlockArm(&tr); err != nil {
		return fmt.Errorf("tool_result block: %v", err)
	}
	if tr.Type != "tool_result" || tr.ToolUseID != "tu_1" {
		return fmt.Errorf("tool_result block = %+v", tr)
	}
	var resultBlocks []capturedBlock
	if err := decodeNested(tr.Content, &resultBlocks); err != nil {
		return fmt.Errorf("tool_result content: %v", err)
	}
	if len(resultBlocks) != 2 {
		return fmt.Errorf("tool_result content blocks = %d, want 2: %+v", len(resultBlocks), resultBlocks)
	}
	wantText := []string{"ok", "no secrets here"}
	for i, want := range wantText {
		if err := validateBlockArm(&resultBlocks[i]); err != nil {
			return fmt.Errorf("tool_result content block %d: %v", i, err)
		}
		if resultBlocks[i].Type != "text" || resultBlocks[i].Text == nil || *resultBlocks[i].Text != want {
			return fmt.Errorf("tool_result content block %d = %+v, want text %q", i, resultBlocks[i], want)
		}
	}
	return nil
}

// assertCleanTopology is the thin test-asserting wrapper for the structural
// check.
func assertCleanTopology(t *testing.T, cap capturedAnthropicRequest) {
	t.Helper()
	if err := checkCleanTopology(cap); err != nil {
		t.Fatalf("%v", err)
	}
}

// assertCapturedHeader pins the canonical request header: the requested model
// and the adapter's deterministic max_tokens default.
func assertCapturedHeader(t *testing.T, cap capturedAnthropicRequest) {
	t.Helper()
	if cap.Model != "claude-3-5-sonnet" {
		t.Fatalf("model = %q, want claude-3-5-sonnet", cap.Model)
	}
	if cap.MaxTokens == nil || *cap.MaxTokens != 4096 {
		t.Fatalf("max_tokens = %v, want the adapter default 4096", cap.MaxTokens)
	}
}

// validateBlockArm rejects every union field that is known globally but
// ILLEGAL for the block's selected type: a text block may carry only type and
// text, a tool_use only type/id/name/input, a tool_result only
// type/tool_use_id/content. DisallowUnknownFields alone cannot catch these
// because every union field is a known member of the shared struct.
func validateBlockArm(block *capturedBlock) error {
	switch block.Type {
	case "text":
		if block.ID != "" || block.Name != "" || block.ToolUseID != "" || len(block.Input) != 0 || len(block.Content) != 0 {
			return fmt.Errorf("text block carries a union field illegal for its arm: %+v", block)
		}
		if block.Text == nil {
			return fmt.Errorf("text block missing text: %+v", block)
		}
	case "tool_use":
		if block.Text != nil || block.ToolUseID != "" || len(block.Content) != 0 {
			return fmt.Errorf("tool_use block carries a union field illegal for its arm: %+v", block)
		}
		if block.ID == "" || block.Name == "" || len(block.Input) == 0 {
			return fmt.Errorf("tool_use block missing required fields: %+v", block)
		}
	case "tool_result":
		if block.Text != nil || block.ID != "" || block.Name != "" || len(block.Input) != 0 {
			return fmt.Errorf("tool_result block carries a union field illegal for its arm: %+v", block)
		}
		if block.ToolUseID == "" || len(block.Content) == 0 {
			return fmt.Errorf("tool_result block missing required fields: %+v", block)
		}
	default:
		return fmt.Errorf("unknown block type %q", block.Type)
	}
	return nil
}

// TestCapturedDecoderStrictness — the weakened forms the permissive decoder
// would accept must FAIL the strict structural decoder: a trailing second
// JSON value and a nested extra member.
func TestCapturedDecoderStrictness(t *testing.T) {
	base := `{"model":"m","max_tokens":1,"system":[{"type":"text","text":"s"}],"tools":[],"messages":[]}`
	if _, err := decodeCaptured(base + ` {}`); err == nil {
		t.Fatal("trailing second JSON value accepted by the top-level decoder")
	}
	withExtra := `{"model":"m","max_tokens":1,"system":[{"type":"text","text":"s","extra":1}],"tools":[],"messages":[]}`
	if _, err := decodeCaptured(withExtra); err == nil {
		t.Fatal("nested extra member accepted by the top-level decoder")
	}
	// Nested raw members are decoded with the same strictness: an extra
	// member inside a tool_result text block is rejected.
	blocks := json.RawMessage(`[{"type":"text","text":"ok","extra":true}]`)
	var out []capturedBlock
	if err := decodeNested(blocks, &out); err == nil {
		t.Fatal("nested extra member accepted by decodeNested")
	}
	// And a trailing second value inside a nested member.
	trailing := json.RawMessage(`[{"type":"text","text":"ok"}] []`)
	if err := decodeNested(trailing, &out); err == nil {
		t.Fatal("trailing JSON accepted inside a nested member")
	}
}

// TestCapturedBlockArmExactness — table-driven negative matrix: each row
// carries one forbidden KNOWN-UNION field for its selected arm. Strict JSON
// decoding succeeds (the field is a known member), but the arm validator must
// reject it. The validator returns an error; this test asserts on it directly
// without any t.Fatal-based helper.
func TestCapturedBlockArmExactness(t *testing.T) {
	rows := []struct {
		name string
		raw  string
	}{
		// text arm: every non-text union field is illegal.
		{"text with id", `{"type":"text","text":"s","id":"x"}`},
		{"text with name", `{"type":"text","text":"s","name":"n"}`},
		{"text with input", `{"type":"text","text":"s","input":{}}`},
		{"text with tool_use_id", `{"type":"text","text":"s","tool_use_id":"t"}`},
		{"text with content", `{"type":"text","text":"s","content":[]}`},
		// tool_use arm.
		{"tool_use with text", `{"type":"tool_use","id":"i","name":"n","input":{},"text":"t"}`},
		{"tool_use with tool_use_id", `{"type":"tool_use","id":"i","name":"n","input":{},"tool_use_id":"t"}`},
		{"tool_use with content", `{"type":"tool_use","id":"i","name":"n","input":{},"content":[]}`},
		// tool_result arm.
		{"tool_result with text", `{"type":"tool_result","tool_use_id":"t","content":[],"text":"x"}`},
		{"tool_result with id", `{"type":"tool_result","tool_use_id":"t","content":[],"id":"i"}`},
		{"tool_result with name", `{"type":"tool_result","tool_use_id":"t","content":[],"name":"n"}`},
		{"tool_result with input", `{"type":"tool_result","tool_use_id":"t","content":[],"input":{}}`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var block capturedBlock
			if err := decodeNested(json.RawMessage(row.raw), &block); err != nil {
				t.Fatalf("strict JSON decode should succeed (the field is a known union member): %v", err)
			}
			if err := validateBlockArm(&block); err == nil {
				t.Fatalf("illegal union field accepted: %+v", block)
			}
		})
	}
}

// TestPIIAnthropicToolResultArrayBlockStringSystem — the SAME blocked shape
// with the system sent as the VALID STRING form, the exact historical parse
// bypass. The adapter must accept the string (canonicalizing it to one text
// block — the captured clean twin therefore still carries the canonical ARRAY
// form and passes the exact-topology check), the pipeline must run, and the
// blocked twin must get the byte-exact 422 with zero additional upstream
// calls, exactly like the array-form test.
func TestPIIAnthropicToolResultArrayBlockStringSystem(t *testing.T) {
	post, hits, captured := anthropicPIIEnv(t, `{"tools":["*"],"on_error":"block"}`)

	const sysText = "You are a coding agent. Read files and report their contents."

	// 1. Clean twin with a STRING system: reaches upstream exactly once; the
	// captured request has the canonicalized array system with the same text.
	cleanBody := anthropicToolResultConvoWithSystem(sysText, []map[string]any{
		{"type": "text", "text": "ok"},
		{"type": "text", "text": "no secrets here"},
	})
	status, body := post(cleanBody)
	if status != http.StatusOK {
		t.Fatalf("clean status = %d, want 200; body=%s", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("clean upstream hits = %d, want 1", n)
	}
	got := captured()
	if len(got) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(got))
	}
	cap := mustDecodeCaptured(t, got[0])
	assertCapturedHeader(t, cap)
	assertCleanTopology(t, cap)

	// 2. Blocked twin with a STRING system and the PII-bearing array-valued
	// tool_result: the pipeline runs (the string parses now) and the refusal
	// is the byte-exact 422; the upstream is never called again.
	blockedBody := anthropicToolResultConvoWithSystem(sysText, []map[string]any{
		{"type": "text", "text": ""},
		{"type": "text", "text": "contact: someone@example.com"},
	})
	status, body = post(blockedBody)
	if status != 422 {
		t.Fatalf("blocked status = %d, want 422; body=%s", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream hits = %d after the blocked request, want 1 (never called again)", n)
	}
	const wantMessage = "Blocked: PII detected in `read_file` output and NOT sent upstream. Found: email (line 2). Do not resend this content; reformulate to exclude or redact these values before returning the tool result."
	wantBody := renderProviderError("anthropic", 422, "pii_detected", wantMessage)
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("block body is not byte-exactly the renderer envelope:\n  got  %s\n  want %s", body, wantBody)
	}
}

// TestPIIAnthropicToolResultArrayBlock — the structured-tool-result regression:
// PII inside an ARRAY-valued Anthropic tool_result is extracted (the empty
// first element makes the email land on line 2), the request is blocked with
// the EXACT provider-shaped, value-free 422, and upstream is never reached.
// The clean twin uses the same topology and reaches upstream exactly once;
// the blocked request leaves the cumulative count unchanged (one loaded
// environment for both).
func TestPIIAnthropicToolResultArrayBlock(t *testing.T) {
	post, hits, captured := anthropicPIIEnv(t, `{"tools":["*"],"on_error":"block"}`)

	// 1. Clean twin: same topology, clean array-valued tool-result content.
	cleanBody := anthropicToolResultConvo([]map[string]any{
		{"type": "text", "text": "ok"},
		{"type": "text", "text": "no secrets here"},
	})
	status, body := post(cleanBody)
	if status != http.StatusOK {
		t.Fatalf("clean status = %d, want 200; body=%s", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("clean upstream hits = %d, want 1", n)
	}
	// The outgoing Anthropic request kept the EXACT topology: decode it
	// strictly (no substring bag) and assert every structural position.
	got := captured()
	if len(got) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(got))
	}
	cap := mustDecodeCaptured(t, got[0])
	assertCapturedHeader(t, cap)
	assertCleanTopology(t, cap)

	// Live weakened-form rows on the REAL captured bytes: a known-union field
	// injected into the wrong arm must fail the structural check even though
	// it is a valid JSON member of the shared struct. Each row re-decodes the
	// immutable body so no row shares a slice backing array with another
	// (spoofed := cap would let an earlier mutation leak into later rows),
	// and each row requires an error NAMING its intended arm so it cannot
	// pass because some unrelated mutation happened to fail the check.
	liveSpoofs := []struct {
		name    string
		mut     func(*capturedAnthropicRequest)
		wantErr string
	}{
		{
			name: "cross-arm text on a tool_use",
			mut: func(c *capturedAnthropicRequest) {
				c.Messages[0].Content[0].Text = strptr("spoof")
			},
			wantErr: "tool_use block",
		},
		{
			name: "cross-arm text on a tool_result",
			mut: func(c *capturedAnthropicRequest) {
				c.Messages[1].Content[0].Text = strptr("spoof")
			},
			wantErr: "tool_result block",
		},
		{
			name: "cross-arm tool_use_id on a system text block",
			mut: func(c *capturedAnthropicRequest) {
				c.System[0].ToolUseID = "tu_9"
			},
			wantErr: "text block",
		},
	}
	for _, row := range liveSpoofs {
		t.Run(row.name, func(t *testing.T) {
			spoofed := mustDecodeCaptured(t, got[0])
			row.mut(&spoofed)
			err := checkCleanTopology(spoofed)
			if err == nil || !strings.Contains(err.Error(), row.wantErr) {
				t.Fatalf("spoof %s: err = %v, want an error naming %q", row.name, err, row.wantErr)
			}
		})
	}

	// 2. Blocked twin: the email sits in the SECOND text element; the empty
	// first element keeps the line numbering at line 2.
	blockedBody := anthropicToolResultConvo([]map[string]any{
		{"type": "text", "text": ""},
		{"type": "text", "text": "contact: someone@example.com"},
	})
	status, body = post(blockedBody)
	if status != 422 {
		t.Fatalf("blocked status = %d, want 422; body=%s", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream hits = %d after the blocked request, want the cumulative count unchanged (1)", n)
	}

	// The refusal is BYTE-EXACTLY the deterministic Anthropic envelope
	// renderProviderError produces: no extra members at either level are
	// tolerated, and the raw email is absent from the complete bytes.
	const wantMessage = "Blocked: PII detected in `read_file` output and NOT sent upstream. Found: email (line 2). Do not resend this content; reformulate to exclude or redact these values before returning the tool result."
	wantBody := renderProviderError("anthropic", 422, "pii_detected", wantMessage)
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("block body is not byte-exactly the renderer envelope:\n  got  %s\n  want %s", body, wantBody)
	}
	if strings.Contains(string(body), "someone@example.com") {
		t.Fatalf("the raw PII value leaked into the block response: %s", body)
	}
}
