package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// drainStream collects every event a ParseStream channel emits.
func drainStream(t *testing.T, ch <-chan engine.StreamEvent) []engine.StreamEvent {
	t.Helper()
	var out []engine.StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// TestStreamParseConcurrentToolCallsInterleaved pins index-aware parsing: two
// interleaved tool calls (wire contentBlockIndexes 0 and 1) with NON-ASCENDING
// stops must produce engine events carrying their OWN indexes. The pre-fix
// parser hard-coded every tool event to index 0 and silently dropped the
// second stop (the single inToolUse bool collapsed both calls onto one scope).
func TestStreamParseConcurrentToolCallsInterleaved(t *testing.T) {
	input := `{"contentBlockStart":{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"tu_0","name":"list_dir"}}}}
{"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tu_1","name":"read_file"}}}}
{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"p\":\"/x\"}"}}}}
{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"f\":\"y\"}"}}}}
{"contentBlockStop":{"contentBlockIndex":1}}
{"contentBlockStop":{"contentBlockIndex":0}}
`
	events := drainStream(t, (&Stream{}).ParseStream(io.NopCloser(strings.NewReader(input))))

	want := []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "tu_0", Name: "list_dir"}},
		{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "tu_1", Name: "read_file"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"p":"/x"}`}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"f":"y"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
	}
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d\ngot:  %+v\nwant: %+v", len(events), len(want), events, want)
	}
	for i := range want {
		if !reflect.DeepEqual(events[i], want[i]) {
			t.Errorf("event %d = %+v, want %+v — every tool event must carry the wire contentBlockIndex", i, events[i], want[i])
		}
	}
}

// TestStreamSerializeConcurrentToolCallsInterleaved pins index-aware
// serialization: concurrent tool-call events with NON-ASCENDING stops must
// write their engine indexes into the wire ContentBlockIndex, and the wire
// must round-trip back to the SAME events. The pre-fix serializer hard-coded
// index 0 for every start/delta/stop, collapsing parallel calls onto one wire
// block.
func TestStreamSerializeConcurrentToolCallsInterleaved(t *testing.T) {
	events := []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "tu_0", Name: "list_dir"}},
		{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "tu_1", Name: "read_file"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"p":"/x"}`}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"f":"y"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
	}
	evtCh := make(chan engine.StreamEvent, len(events))
	for _, e := range events {
		evtCh <- e
	}
	close(evtCh)

	var buf bytes.Buffer
	if err := (&Stream{}).SerializeStream(context.Background(), &buf, evtCh); err != nil {
		t.Fatalf("serialize: %v", err)
	}

	// Every tool frame must carry the engine index it was emitted for, in
	// order: start,start,delta,delta,stop,stop at 0,1,0,1,1,0.
	var got []int
	var toolIDs []string
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var se bedrockStreamEvent
		if err := json.Unmarshal([]byte(line), &se); err != nil {
			t.Fatalf("bad wire line: %v (%q)", err, line)
		}
		switch {
		case se.ContentBlockStart != nil && se.ContentBlockStart.Start.ToolUse != nil:
			got = append(got, se.ContentBlockStart.ContentBlockIndex)
			toolIDs = append(toolIDs, se.ContentBlockStart.Start.ToolUse.ToolUseID)
		case se.ContentBlockDelta != nil && se.ContentBlockDelta.Delta.ToolUse != nil:
			got = append(got, se.ContentBlockDelta.ContentBlockIndex)
		case se.ContentBlockStop != nil:
			got = append(got, se.ContentBlockStop.ContentBlockIndex)
		}
	}
	wantIdx := []int{0, 1, 0, 1, 1, 0}
	if !reflect.DeepEqual(got, wantIdx) {
		t.Fatalf("wire contentBlockIndexes = %v, want %v — concurrent calls must keep their own indexes through non-ascending stops", got, wantIdx)
	}
	if len(toolIDs) != 2 || toolIDs[0] != "tu_0" || toolIDs[1] != "tu_1" {
		t.Fatalf("tool starts = %v, want [tu_0 tu_1]", toolIDs)
	}

	// Round-trip: the serialized wire must reparse to the SAME events.
	reparsed := drainStream(t, (&Stream{}).ParseStream(io.NopCloser(bytes.NewReader(buf.Bytes()))))
	if len(reparsed) != len(events) {
		t.Fatalf("round-trip: %d events, want %d: %+v", len(reparsed), len(events), reparsed)
	}
	for i := range events {
		if !reflect.DeepEqual(reparsed[i], events[i]) {
			t.Errorf("round-trip event %d = %+v, want %+v", i, reparsed[i], events[i])
		}
	}
}

// TestStreamParseToolDeltaForUnknownIndexErrors: a toolUse delta naming an
// index that never opened as a tool block is an EXPLICIT error event — not a
// silent attach to whatever tool state is around (the pre-fix parser flipped
// inToolUse on any toolUse delta regardless of index).
func TestStreamParseToolDeltaForUnknownIndexErrors(t *testing.T) {
	events := drainStream(t, (&Stream{}).ParseStream(io.NopCloser(strings.NewReader(
		`{"contentBlockDelta":{"contentBlockIndex":2,"delta":{"toolUse":{"input":"{}"}}}}`+"\n"))))
	if len(events) != 1 || events[0].Error == nil {
		t.Fatalf("expected exactly one error event, got %+v", events)
	}
	if events[0].Error.Message != "bedrock: tool call delta for unknown index 2" {
		t.Errorf("error message = %q, want %q", events[0].Error.Message, "bedrock: tool call delta for unknown index 2")
	}
}

// TestStreamSerializeToolCallStateErrors: index-bound tool state rejects
// malformed IR explicitly — a duplicate start, or a delta/end for an index
// that never started — never silently collapsing parallel scopes.
func TestStreamSerializeToolCallStateErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []engine.StreamEvent
		want   string
	}{
		{
			name: "duplicate start",
			events: []engine.StreamEvent{
				{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "a", Name: "a"}},
				{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "b", Name: "b"}},
			},
			want: "bedrock: duplicate tool call start at index 0",
		},
		{
			name: "delta for unknown index",
			events: []engine.StreamEvent{
				{ToolCallDelta: &engine.ToolCallDelta{Index: 2, ArgumentsDelta: `{}`}},
			},
			want: "bedrock: tool call delta for unknown index 2",
		},
		{
			name: "end for unknown index",
			events: []engine.StreamEvent{
				{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
			},
			want: "bedrock: tool call end for unknown index 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evCh := make(chan engine.StreamEvent, len(tc.events))
			for _, ev := range tc.events {
				evCh <- ev
			}
			close(evCh)
			var buf bytes.Buffer
			err := (&Stream{}).SerializeStream(context.Background(), &buf, evCh)
			if err == nil {
				t.Fatal("malformed tool-call stream did not error")
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}
