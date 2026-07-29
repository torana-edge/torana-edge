package plugin

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
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
// Verification means unmarshalling each plugin's output AND fingerprinting its
// sections, so BenchmarkWriteGrantVerification measures both halves against a
// prototype verifier — the decode alone is under half the real cost.
// BenchmarkRunBeforeRequest measures what it would be added to, and
// BenchmarkBoundaryCrossing isolates one WASM crossing using fixtures that do
// no guest work at all.
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

// BenchmarkVerificationUnmarshal is the decode half of the write-grant check,
// measured alone so the two halves can be told apart.
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

// sectionPrints is a prototype of the write-grant verifier's fingerprints —
// one per grantable section, so a section whose print moved requires the
// matching grant.
//
// This is a benchmark prototype, not the shipping implementation: it exists so
// the cost figure covers the work the verifier actually does, rather than only
// the proto.Unmarshal in front of it. FNV-1a is the cheapest credible choice
// and therefore the fairest lower bound on the hashing itself.
type sectionPrints struct {
	messagesUser      uint64
	messagesAssistant uint64
	messagesSystem    uint64
	messagesTool      uint64
	tools             uint64
	model             uint64
	params            uint64
}

func fingerprintSections(req *pb.ChatRequest) sectionPrints {
	var p sectionPrints
	h := fnv.New64a()

	roleHash := map[string]*uint64{
		"user":      &p.messagesUser,
		"assistant": &p.messagesAssistant,
		"system":    &p.messagesSystem,
		"tool":      &p.messagesTool,
	}
	// One accumulator per role, folded in message order so a reordering is
	// detected as well as an edit.
	acc := map[string]uint64{}
	for _, m := range req.Messages {
		h.Reset()
		_, _ = h.Write([]byte(m.Role))
		_, _ = h.Write([]byte(m.Content))
		_, _ = h.Write(m.ContentPartsJson)
		_, _ = h.Write([]byte(m.Thinking))
		_, _ = h.Write([]byte(m.ToolCallId))
		_, _ = h.Write([]byte(m.ToolName))
		_, _ = h.Write(m.CacheControlJson)
		for _, tc := range m.ToolCalls {
			_, _ = h.Write([]byte(tc.Id))
			_, _ = h.Write([]byte(tc.Name))
			_, _ = h.Write(tc.ArgumentsJson)
			_, _ = h.Write([]byte(tc.Signature))
		}
		acc[m.Role] = acc[m.Role]*31 + h.Sum64()
	}
	for role, dst := range roleHash {
		*dst = acc[role]
	}

	h.Reset()
	for _, t := range req.Tools {
		_, _ = h.Write([]byte(t.Name))
		_, _ = h.Write([]byte(t.Description))
		_, _ = h.Write(t.ParametersJson)
		_, _ = h.Write(t.CacheControlJson)
	}
	p.tools = h.Sum64()

	h.Reset()
	_, _ = h.Write([]byte(req.Model))
	p.model = h.Sum64()

	h.Reset()
	var scratch [8]byte
	if req.MaxTokens != nil {
		binary.LittleEndian.PutUint64(scratch[:], uint64(*req.MaxTokens))
		_, _ = h.Write(scratch[:])
	}
	if req.Temperature != nil {
		binary.LittleEndian.PutUint64(scratch[:], math.Float64bits(*req.Temperature))
		_, _ = h.Write(scratch[:])
	}
	if req.TopP != nil {
		binary.LittleEndian.PutUint64(scratch[:], math.Float64bits(*req.TopP))
		_, _ = h.Write(scratch[:])
	}
	for _, s := range req.StopSequences {
		_, _ = h.Write([]byte(s))
	}
	if req.Stream {
		_, _ = h.Write([]byte{1})
	}
	p.params = h.Sum64()

	return p
}

// BenchmarkWriteGrantVerification is the whole per-plugin check: decode the
// plugin's output, fingerprint every grantable section, and compare against the
// previously accepted prints. This is the number to multiply by the plugin
// count — BenchmarkVerificationUnmarshal alone understates it.
func BenchmarkWriteGrantVerification(b *testing.B) {
	for _, n := range benchSizes {
		req := pbconv.ToPBChatRequest(benchConversation(n))
		raw, err := proto.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
		accepted := fingerprintSections(req)

		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var out pb.ChatRequest
				if err := proto.Unmarshal(raw, &out); err != nil {
					b.Fatal(err)
				}
				got := fingerprintSections(&out)
				if got != accepted {
					b.Fatal("fingerprints must match for an unmodified request")
				}
			}
		})
	}
}

// BenchmarkBoundaryCrossing isolates the cost of one WASM crossing by using
// fixtures that do nothing but return pass-through. The delta between 1, 2 and
// 3 plugins is crossing cost with no guest work mixed in — which the mixed
// pipeline in BenchmarkRunBeforeRequest cannot give, because its fixtures scan
// messages, emit metrics and make host calls.
func BenchmarkBoundaryCrossing(b *testing.B) {
	inert := []string{"test-inert-a", "test-inert-b", "test-inert-c"}
	for _, count := range []int{1, 2, 3} {
		order := inert[:count]
		for _, name := range order {
			requireWASM(b, fixturesDir+"/"+name+"/plugin.wasm")
		}
		pp := newTestPipeline(b, fixturesDir, order)
		for _, n := range []int{1, 100} {
			chat := benchConversation(n)
			b.Run(fmt.Sprintf("plugins=%d/msgs=%d", count, n), func(b *testing.B) {
				b.ReportAllocs()
				ctx := context.Background()
				for i := 0; i < b.N; i++ {
					// These fixtures never mutate, so one request can be
					// reused: nothing to copy, nothing to exclude from the
					// timed region.
					if _, err := pp.RunBeforeRequest(ctx, uint64(i+1), chat); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
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
					// test-mutator appends a marker and skips messages that
					// already carry it, so each iteration needs a fresh copy or
					// it would measure the no-op path after the first. The copy
					// is test scaffolding, not pipeline work, so it is excluded
					// from the timed region — leaving it in inflates the
					// baseline and makes everything compared against it look
					// cheaper than it is.
					b.StopTimer()
					fresh := benchConversationFrom(chat)
					b.StartTimer()

					if _, err := pp.RunBeforeRequest(ctx, uint64(i+1), fresh); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkRunOnStreamChunk is the cost that actually decides whether Torana
// feels fast, and it is not the request path.
//
// run_on_stream_chunk fires once per SSE event. A 1000-token streamed response
// is on the order of 1000 events, so a per-event cost multiplies by three
// orders of magnitude before the user sees the end of the reply — where the
// request hook is paid exactly once. Measure per-event, then multiply.
func BenchmarkRunOnStreamChunk(b *testing.B) {
	requireWASM(b, "../../examples/plugins/test-stream-mutator/plugin.wasm")

	text := "the quick brown fox jumps over the lazy dog"
	events := map[string]engine.StreamEvent{
		"text_delta": {TextDelta: &text},
		// A tool-call fragment: the event stream plugins actually buffer, and
		// the one whose handling is duplicated across intent and
		// schema_translator.
		"tool_delta": {ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"path":"internal/`}},
	}

	for _, count := range []int{1} {
		pp := newTestPipeline(b, "../../examples/plugins", []string{"test-stream-mutator"})
		for name, ev := range events {
			b.Run(fmt.Sprintf("plugins=%d/%s", count, name), func(b *testing.B) {
				b.ReportAllocs()
				ctx := context.Background()
				for i := 0; i < b.N; i++ {
					e := ev
					if _, err := pp.RunOnStreamChunk(ctx, 1, &e); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkStreamedResponse is the per-event cost multiplied out over a
// realistic response length, so the number is in units a user would feel.
func BenchmarkStreamedResponse(b *testing.B) {
	requireWASM(b, "../../examples/plugins/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(b, "../../examples/plugins", []string{"test-stream-mutator"})

	for _, tokens := range []int{100, 1000} {
		b.Run(fmt.Sprintf("tokens=%d", tokens), func(b *testing.B) {
			b.ReportAllocs()
			ctx := context.Background()
			text := "token "
			for i := 0; i < b.N; i++ {
				for t := 0; t < tokens; t++ {
					ev := engine.StreamEvent{TextDelta: &text}
					if _, err := pp.RunOnStreamChunk(ctx, uint64(i+1), &ev); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
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
