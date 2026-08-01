package plugin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
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

// BenchmarkWriteGrantVerification is the whole per-plugin check under both
// enforcement-safe methods: decode the plugin's output, then either compare it
// exactly against the previously accepted request, or fingerprint its sections
// and compare digests. Multiply the result by the plugin count.
//
// See writegrant.go for the fingerprint and writegrant_prototype_test.go for
// the exact-comparison oracle and the mutation suite proving both methods
// proving each detects every change a plugin could make. An earlier prototype
// here was faster and unsafe: it missed a cross-role reorder outright.
func BenchmarkWriteGrantVerification(b *testing.B) {
	for _, n := range benchSizes {
		req := pbconv.ToPBChatRequest(benchConversation(n))
		raw, err := proto.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}

		b.Run(fmt.Sprintf("exact/msgs=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var out pb.ChatRequest
				if err := proto.Unmarshal(raw, &out); err != nil {
					b.Fatal(err)
				}
				if compareSections(req, &out).any() {
					b.Fatal("unmodified request must compare equal")
				}
			}
		})

		accepted := fingerprintRequestSections(req)
		b.Run(fmt.Sprintf("fingerprint/msgs=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var out pb.ChatRequest
				if err := proto.Unmarshal(raw, &out); err != nil {
					b.Fatal(err)
				}
				if !accepted.equal(fingerprintRequestSections(&out)) {
					b.Fatal("unmodified request must fingerprint equal")
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

// BenchmarkRunOnStreamChunk measures the per-event cost of the streaming hook,
// which is where the pipeline's time actually goes: run_on_stream_chunk fires
// once per SSE event, while the request hook is paid once per request.
//
// Each iteration runs a COMPLETE request under one request ID and ends it.
// Stream plugins keep request-scoped state — test-fragment-buffer buffers
// tool-call fragments until ToolCallEnd — so a benchmark that reuses one ID
// forever measures unbounded buffer growth, and one that invents a fresh ID per
// event without calling EndRequest leaks a buffer per event. Neither is a
// per-event cost.
func BenchmarkRunOnStreamChunk(b *testing.B) {
	streamPlugins := []string{"test-stream-mutator", "test-fragment-buffer"}

	text := "the quick brown fox jumps over the lazy dog"
	// sequences are complete, well-formed event streams: whatever a plugin
	// opens, it gets to close.
	sequences := map[string]func() []engine.StreamEvent{
		"text_delta": func() []engine.StreamEvent {
			t := text
			return []engine.StreamEvent{{TextDelta: &t}}
		},
		// A tool call assembled from fragments, then closed — the pattern
		// intent and schema_translator both implement.
		"tool_call": func() []engine.StreamEvent {
			return []engine.StreamEvent{
				{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_1", Name: "read_file"}},
				{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"path":"internal/`}},
				{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `parser/parse.go"}`}},
				{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
			}
		},
	}

	for _, count := range []int{1, 2} {
		order := streamPlugins[:count]
		for _, name := range order {
			requireWASM(b, fixturesDir+"/"+name+"/plugin.wasm")
		}
		pp := newTestPipeline(b, fixturesDir, order)

		for name, build := range sequences {
			seq := build()
			b.Run(fmt.Sprintf("plugins=%d/%s", count, name), func(b *testing.B) {
				ctx := context.Background()
				// Warm before timing. The first runs are measurably slower than
				// the steady state; the cause is not established here, so the
				// benchmark removes the effect rather than explaining it.
				for w := 0; w < 20; w++ {
					runSequence(b, pp, ctx, uint64(w+1), seq)
				}
				b.ResetTimer()
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					runSequence(b, pp, ctx, uint64(i+1), seq)
				}
				// Report per EVENT, so the two sequences are comparable.
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(seq)), "ns/event")
			})
		}
	}
}

// runSequence plays one complete request and releases its request-scoped state.
func runSequence(b *testing.B, pp *PluginPipeline, ctx context.Context, reqID uint64, seq []engine.StreamEvent) {
	b.Helper()
	for _, ev := range seq {
		e := ev
		if _, err := pp.RunOnStreamChunk(ctx, reqID, &e); err != nil {
			b.Fatal(err)
		}
	}
	pp.EndRequest(reqID)
}

// BenchmarkStreamedResponse walks a whole response's worth of events, so the
// per-event figure can be checked against a sustained run rather than trusted
// on its own.
//
// The parameter is EVENTS, not tokens. How many SSE events a provider emits per
// token is a provider behaviour this benchmark does not measure and must not
// assume: providers coalesce deltas, and content-block and message boundaries
// add events of their own. Converting events to tokens is the reader's job,
// with their own provider's traffic in hand.
func BenchmarkStreamedResponse(b *testing.B) {
	requireWASM(b, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(b, fixturesDir, []string{"test-stream-mutator"})

	for _, events := range []int{100, 1000} {
		b.Run(fmt.Sprintf("events=%d", events), func(b *testing.B) {
			b.ReportAllocs()
			ctx := context.Background()
			text := "token "
			for i := 0; i < b.N; i++ {
				reqID := uint64(i + 1)
				for t := 0; t < events; t++ {
					ev := engine.StreamEvent{TextDelta: &text}
					if _, err := pp.RunOnStreamChunk(ctx, reqID, &ev); err != nil {
						b.Fatal(err)
					}
				}
				// One response, then release its request-scoped state — the
				// host does this at the end of every request.
				pp.EndRequest(reqID)
			}
		})
	}
}

// runVerifiedSequence plays one complete request through the enforcement
// entry point (per-event verified calls + end-of-stream scope close) and
// releases its request-scoped state.
func runVerifiedSequence(b *testing.B, pp *PluginPipeline, ctx context.Context, reqID uint64, seq []engine.StreamEvent) {
	b.Helper()
	for _, ev := range seq {
		e := ev
		if _, err := pp.RunOnStreamChunkVerified(ctx, reqID, &e); err != nil {
			b.Fatal(err)
		}
	}
	if err := pp.EndStreamVerified(reqID); err != nil {
		b.Fatal(err)
	}
	pp.EndRequest(reqID)
}

// BenchmarkStreamEnforcement measures the exact production path of the
// stream policy/signature enforcement (B2 part 2b): the per-event state
// bookkeeping, recursive SDK field-policy transaction, scope verifier, and
// WASM boundary cadence. The cases deliberately cover the semantic shapes the
// registry promises to support: pass-through, one-for-one rewrite, fragment
// assembly, suppression, and fan-out — not just a verifier-only happy path.
//
// Each iteration is one COMPLETE request under one request ID (the walkers
// and buffers are request-scoped, so reusing an ID would measure unbounded
// growth). ReportMetric scales by event count so the sizes are comparable.
func BenchmarkStreamEnforcement(b *testing.B) {
	cases := []struct {
		name   string
		plugin string
		seq    []engine.StreamEvent
	}{
		{
			name:   "pass-through",
			plugin: "test-stream-mutator",
			seq: []engine.StreamEvent{
				{TextDelta: strPtr("ordinary text")}, {FinishReason: "stop"},
			},
		},
		{
			name:   "rewrite",
			plugin: "test-stream-mutator",
			seq: []engine.StreamEvent{
				{TextDelta: strPtr("the secret plan")}, {FinishReason: "stop"},
			},
		},
		{
			name:   "assembly",
			plugin: "test-fragment-buffer",
			seq: append([]engine.StreamEvent{
				{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_1", Name: "read"}},
				{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"path":"`}},
				{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `x"}`}},
				{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
			}, engine.StreamEvent{FinishReason: "tool_calls"}),
		},
		{
			name:   "suppress",
			plugin: "test-fragment-buffer",
			seq: append([]engine.StreamEvent{
				{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_1", Name: "read"}},
				{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{`}},
				{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
			}, engine.StreamEvent{FinishReason: "tool_calls"}),
		},
		{
			name:   "fan-out",
			plugin: "test-stream-fanout",
			seq: []engine.StreamEvent{
				{TextDelta: strPtr("ab")}, {FinishReason: "stop"},
			},
		},
	}

	for _, tc := range cases {
		requireWASM(b, fixturesDir+"/"+tc.plugin+"/plugin.wasm")
		pp := newTestPipeline(b, fixturesDir, []string{tc.plugin})
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			ctx := context.Background()
			for w := 0; w < 20; w++ {
				runVerifiedSequence(b, pp, ctx, uint64(w+1), tc.seq)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runVerifiedSequence(b, pp, ctx, uint64(i+1), tc.seq)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(tc.seq)), "ns/event")
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
