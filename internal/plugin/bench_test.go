package plugin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

// These benchmarks exist to answer one question with numbers instead of
// intuition: what would it cost to verify, between every pair of plugins, that
// a plugin only changed what its grants allow?
//
// Today RunBeforeRequest converts once, marshals once, and then chains raw
// bytes from plugin to plugin without ever looking inside (discovery.go:942-967).
// Verification means unmarshalling each plugin's output to fingerprint it, so
// the added cost is N unmarshals per request. BenchmarkVerificationUnmarshal
// measures that directly; BenchmarkRunBeforeRequest measures what it would be
// added to. The ratio between them is the decision.
//
// Run:
//
//	make testdata
//	go test ./internal/plugin -run '^$' -bench . -benchmem

// benchConversation builds a request shaped like real coding-agent traffic:
// a system prompt, alternating user/assistant turns, and tool calls whose
// results carry the bulk of the bytes. Tool results dominate real payloads,
// which is why they dominate here.
func benchConversation(messages int) *engine.ChatRequest {
	temp := 0.0
	maxTok := 4096
	req := &engine.ChatRequest{
		Model:       "claude-sonnet-4",
		Temperature: &temp,
		MaxTokens:   &maxTok,
		Messages: []engine.Message{{
			Role:    engine.RoleSystem,
			Content: strings.Repeat("You are a coding assistant. ", 40),
		}},
		Tools: []engine.ToolDef{
			{
				Name:        "read_file",
				Description: "Read a file from the workspace",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
			{
				Name:        "run_command",
				Description: "Run a shell command",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
					"required":   []any{"cmd"},
				},
			},
		},
	}

	// ~2KB of tool output per result, which is conservative next to a real
	// file read or test run.
	toolOutput := strings.Repeat("package main\n\nfunc handler() error { return nil }\n", 40)

	for i := 0; i < messages; i++ {
		switch i % 3 {
		case 0:
			req.Messages = append(req.Messages, engine.Message{
				Role:    engine.RoleUser,
				Content: fmt.Sprintf("Turn %d: please investigate the failing test in the parser.", i),
			})
		case 1:
			req.Messages = append(req.Messages, engine.Message{
				Role: engine.RoleAssistant,
				ToolCalls: []engine.ToolCall{{
					ID:        fmt.Sprintf("call_%d", i),
					Name:      "read_file",
					Arguments: map[string]any{"path": fmt.Sprintf("internal/parser/parse_%d.go", i)},
				}},
			})
		default:
			req.Messages = append(req.Messages, engine.Message{
				Role:       engine.RoleTool,
				ToolCallID: fmt.Sprintf("call_%d", i-1),
				ToolName:   "read_file",
				Content:    toolOutput,
			})
		}
	}
	return req
}

var benchSizes = []int{1, 5, 20, 100}

// benchPlugins are fixtures with a run_before_request hook that are inert for
// neutral content: the blocker triggers only on "blockme" and the responder
// only on "respondme", so neither fires here. test-mutator is the one that
// actually rewrites the request, which is the expensive case.
var benchPlugins = []string{
	"test-mutator",
	"test-observer",
	"test-metrics",
	"test-blocker-nogrant",
	"test-responder-nogrant",
}

// BenchmarkPbconvToPB isolates the engine→protobuf conversion, which is the
// single most expensive step already in the path today: it runs json.Marshal
// per message (ContentParts, CacheControl), per tool call (Arguments) and per
// tool (Parameters) — pbconv.go:33-84. Any design that re-converts per plugin
// pays this again, which is why the plan projects from the built proto instead.
func BenchmarkPbconvToPB(b *testing.B) {
	for _, n := range benchSizes {
		chat := benchConversation(n)
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = pbconv.ToPBChatRequest(chat)
			}
		})
	}
}

// BenchmarkPbconvFromPB is the return leg, run once per request today.
func BenchmarkPbconvFromPB(b *testing.B) {
	for _, n := range benchSizes {
		pbReq := pbconv.ToPBChatRequest(benchConversation(n))
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = pbconv.FromPBChatRequest(pbReq)
			}
		})
	}
}

// BenchmarkProtoMarshal is the once-per-request encode at discovery.go:943.
func BenchmarkProtoMarshal(b *testing.B) {
	for _, n := range benchSizes {
		pbReq := pbconv.ToPBChatRequest(benchConversation(n))
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := proto.Marshal(pbReq); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkVerificationUnmarshal is the cost the write-grant design adds:
// one proto.Unmarshal of a plugin's output, per plugin, so the host can
// fingerprint its sections and compare them against the previously accepted
// request. Multiply this by the plugin count and compare against
// BenchmarkRunBeforeRequest for the same size.
func BenchmarkVerificationUnmarshal(b *testing.B) {
	for _, n := range benchSizes {
		raw, err := proto.Marshal(pbconv.ToPBChatRequest(benchConversation(n)))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var out pb.ChatRequest
				if err := proto.Unmarshal(raw, &out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRunBeforeRequest is the end-to-end baseline: the whole hook
// dispatch including the WASM boundary crossings, for 1, 3 and 5 plugins.
// Verification cost is only meaningful as a fraction of this.
func BenchmarkRunBeforeRequest(b *testing.B) {
	for _, count := range []int{1, 3, 5} {
		order := benchPlugins[:count]
		for _, name := range order {
			requireWASM(b, "../../examples/plugins/"+name+"/plugin.wasm")
		}
		pp := newTestPipeline(b, "../../examples/plugins", order)
		for _, n := range benchSizes {
			chat := benchConversation(n)
			b.Run(fmt.Sprintf("plugins=%d/msgs=%d", count, n), func(b *testing.B) {
				b.ReportAllocs()
				ctx := context.Background()
				for i := 0; i < b.N; i++ {
					// A fresh copy each iteration: test-mutator appends a
					// marker and skips messages that already carry it, so a
					// shared request would measure the no-op path after the
					// first iteration.
					if _, err := pp.RunBeforeRequest(ctx, uint64(i+1), benchConversationFrom(chat)); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// benchConversationFrom deep-copies the parts a plugin can mutate. Cheaper
// than rebuilding the conversation, and excluded from the measurement above
// only in the sense that it is the same work every iteration.
func benchConversationFrom(src *engine.ChatRequest) *engine.ChatRequest {
	out := *src
	out.Messages = make([]engine.Message, len(src.Messages))
	copy(out.Messages, src.Messages)
	out.Tools = make([]engine.ToolDef, len(src.Tools))
	copy(out.Tools, src.Tools)
	return &out
}
