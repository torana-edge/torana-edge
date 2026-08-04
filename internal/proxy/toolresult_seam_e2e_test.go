package proxy

// Staged real-hook proof for the SDK tool-result seam (SDK main 995c0bd,
// merged): a REAL WASM guest drives the SDK helper through the REAL server.
// The helper fixture requests ONLY ir.tool_results.write — every accepted
// row below proves the position-keyed text change is authorized by the NEW
// grant alone (the openai tool-role row, the gemini user-role rows), and
// the grant-less fixture proves the authorization is real.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// toolResultBody is an openai tool-role request carrying a tool-result with
// marker topology + metadata + a result signature, plus a sibling plain
// result in a second tool message.
func toolResultBody() string {
	return `{"model":"m","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"before","name":"read"},
		{"role":"tool","tool_call_id":"c2","content":"sibling","name":"write"}
	]}`
}

// TestToolResultSeamRealHookE2E — the real hook rows. The OpenAI rows prove
// AUTHORIZATION/ROLE coverage only (the openai wire carries no signatures,
// markers, or metadata): a tool-role mutation is accepted under ONLY
// ir.messages.write.tool and reaches the wire; the byte-identical call is a
// pass; the grant-less fixture is refused (502). The PROVENANCE claims are
// proven by the gemini rows below (which carry both partMetadata and
// thoughtSignature) and by the verifier unit.
func TestToolResultSeamRealHookE2E(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-toolresult-helper/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-toolresult-helper-nogrant/plugin.wasm")

	var lastBody string
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	newServer := func(order []string) (*Server, string) {
		srv, err := New(Config{Port: "0", Providers: provider.Config{
			Providers: map[string]provider.Provider{"p": {URL: upstream.URL, Format: "openai"}},
			Limits:    provider.Limits{Concurrency: 8},
			Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: order, AllowUnapproved: true},
		}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Shutdown(t.Context()) })
		return srv, "http://" + ln.Addr().String()
	}
	post := func(base, model string) (int, []byte) {
		body := strings.Replace(toolResultBody(), `"model":"m"`, `"model":"`+model+`"`, 1)
		client := &http.Client{Timeout: 30 * time.Second}
		req, _ := http.NewRequest("POST", base+"/provider/p/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	t.Run("change accepted with only the tool grant", func(t *testing.T) {
		_, base := newServer([]string{"test-toolresult-helper"})
		status, _ := post(base, "change")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		var wire struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
				Content    string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(lastBody), &wire); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		found := false
		for _, m := range wire.Messages {
			if m.Role == "tool" && m.ToolCallID == "c1" {
				found = true
				if m.Content != "changed-by-helper" {
					t.Fatalf("changed text did not reach upstream: %q", m.Content)
				}
			}
			if m.Role == "tool" && m.ToolCallID == "c2" && m.Content != "sibling" {
				t.Fatalf("sibling result disturbed: %q", m.Content)
			}
		}
		if !found {
			t.Fatal("the designated result did not reach upstream")
		}
	})

	t.Run("noop passes with tokens preserved", func(t *testing.T) {
		_, base := newServer([]string{"test-toolresult-helper"})
		status, _ := post(base, "noop")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if !strings.Contains(lastBody, `"content":"before"`) {
			t.Fatalf("the no-op must leave the original text: %s", lastBody)
		}
	})

	t.Run("nogrant refused", func(t *testing.T) {
		_, base := newServer([]string{"test-toolresult-helper-nogrant"})
		status, _ := post(base, "change")
		if status != 502 {
			t.Fatalf("status = %d, want 502 (the grant is the authorization)", status)
		}
	})

	// The ordinary user-role case (gemini, code-assist envelope): the tool
	// result carries BOTH partMetadata and thoughtSignature. The final
	// provider request is decoded STRUCTURALLY (strict typed decode, not
	// substring presence):
	//
	//   - change-json: the changed text is the EXACT response bytes, the
	//     metadata is byte-exact in the same Part, the thoughtSignature is
	//     ABSENT (the helper cleared the stale token), and the sibling
	//     part/topology is unchanged;
	//   - noop: the EXACT original text, metadata, and thoughtSignature are
	//     retained in the same Part (tokens preserved).
	type geminiPartT struct {
		// PRESENCE-SENSITIVE text arm: nil = absent; an explicitly emitted
		// empty text member would be a non-nil pointer to "".
		Text             *string `json:"text"`
		FunctionResponse *struct {
			Name     string          `json:"name"`
			Response json.RawMessage `json:"response"`
			ID       string          `json:"id"`
		} `json:"functionResponse"`
		PartMetadata     json.RawMessage `json:"partMetadata"`
		ThoughtSignature *string         `json:"thoughtSignature"`
	}
	type geminiWireT struct {
		Model   string `json:"model"`
		Request struct {
			Contents []struct {
				Role  string        `json:"role"`
				Parts []geminiPartT `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	geminiWire := func(t *testing.T, model string) geminiWireT {
		t.Helper()
		srv, err := New(Config{Port: "0", Providers: provider.Config{
			Providers: map[string]provider.Provider{"p": {URL: upstream.URL, Format: "gemini"}},
			Limits:    provider.Limits{Concurrency: 8},
			Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-toolresult-helper"}, AllowUnapproved: true},
		}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Shutdown(t.Context()) })

		body := `{"model":"` + model + `","request":{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"before"},"id":"c1"},"partMetadata":{"custom":{"b":1,"a":2}},"thoughtSignature":"sig-token"}]},{"role":"user","parts":[{"text":"sibling-text","partMetadata":{"s":"1"}}]}]}}`
		client := &http.Client{Timeout: 30 * time.Second}
		req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/provider/p/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var wire geminiWireT
		// STRICT structural decode: unknown fields fail, no trailing data.
		dec := json.NewDecoder(strings.NewReader(lastBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wire); err != nil {
			t.Fatalf("gemini wire not structurally decodable: %v (%s)", err, lastBody)
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			t.Fatalf("gemini wire has trailing data: %v", err)
		}
		return wire
	}

	// assertGeminiShape is the SHARED cardinality/topology pin: exactly TWO
	// contents rows (the designated functionResponse row and the sibling
	// text row), ONE part each; both rows index only AFTER the shape is
	// pinned.
	assertGeminiShape := func(t *testing.T, wire geminiWireT) (geminiPartT, geminiPartT) {
		t.Helper()
		rows := wire.Request.Contents
		if len(rows) != 2 {
			t.Fatalf("contents rows = %d, want 2 (designated row + sibling row)", len(rows))
		}
		for i, r := range rows {
			if r.Role != "user" {
				t.Fatalf("row %d role = %q, want user", i, r.Role)
			}
		}
		if len(rows[0].Parts) != 1 || len(rows[1].Parts) != 1 {
			t.Fatalf("parts per row = %d/%d, want 1/1", len(rows[0].Parts), len(rows[1].Parts))
		}
		return rows[0].Parts[0], rows[1].Parts[0]
	}
	checkSibling := func(t *testing.T, sibling geminiPartT) {
		t.Helper()
		if sibling.FunctionResponse != nil {
			t.Fatal("the sibling part lost its text identity")
		}
		if sibling.Text == nil || *sibling.Text != "sibling-text" {
			t.Fatalf("sibling text = %v, want PRESENT sibling-text", sibling.Text)
		}
		if sibling.ThoughtSignature != nil {
			t.Fatalf("the sibling part carries a signature: %q", *sibling.ThoughtSignature)
		}
		if string(sibling.PartMetadata) != `{"s":"1"}` {
			t.Fatalf("sibling partMetadata not byte-exact: %s", sibling.PartMetadata)
		}
	}

	t.Run("gemini change: text changed, signature ABSENT, metadata exact, sibling untouched", func(t *testing.T) {
		wire := geminiWire(t, "change-json")
		part, sibling := assertGeminiShape(t, wire)
		checkSibling(t, sibling)
		if part.FunctionResponse == nil || part.FunctionResponse.ID != "c1" {
			t.Fatalf("the designated part is gone: %+v", part)
		}
		if part.Text != nil {
			t.Fatalf("the designated part must have NO text arm: %q", *part.Text)
		}
		if string(part.FunctionResponse.Response) != `{"output":"changed"}` {
			t.Fatalf("response = %s, want the exact changed bytes", part.FunctionResponse.Response)
		}
		if string(part.PartMetadata) != `{"custom":{"b":1,"a":2}}` {
			t.Fatalf("partMetadata not byte-exact: %s", part.PartMetadata)
		}
		// Presence-sensitive: the member is ABSENT (nil), not an empty value.
		if part.ThoughtSignature != nil {
			t.Fatalf("a stale thoughtSignature reached the wire: %q", *part.ThoughtSignature)
		}
	})

	t.Run("gemini noop: original text, metadata, and signature PRESENT, sibling untouched", func(t *testing.T) {
		wire := geminiWire(t, "noop")
		part, sibling := assertGeminiShape(t, wire)
		checkSibling(t, sibling)
		if part.Text != nil {
			t.Fatalf("the designated part must have NO text arm: %q", *part.Text)
		}
		if string(part.FunctionResponse.Response) != `{"output":"before"}` {
			t.Fatalf("response = %s, want the exact original bytes", part.FunctionResponse.Response)
		}
		if string(part.PartMetadata) != `{"custom":{"b":1,"a":2}}` {
			t.Fatalf("partMetadata not byte-exact: %s", part.PartMetadata)
		}
		// Presence-sensitive: the member is PRESENT with the exact value.
		if part.ThoughtSignature == nil || *part.ThoughtSignature != "sig-token" {
			t.Fatalf("thoughtSignature not preserved: %v", part.ThoughtSignature)
		}
	})
}
