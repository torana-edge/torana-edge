package plugin

// Stream-signature enforcement.
//
// This file wires the pure stream verifier (streamverify.go — DONE, consumed
// as-is) into the streaming hook as the enforcement layer, implementing the
// settled atomicity ruling:
//
//   - PRE-COMMIT violations (per-event returned-side discipline: duplicate
//     start at an open index, unknown-index delta/stop, kind-switch
//     violations, events after a terminal condition, unbound signature_delta
//     — the rules validateAcceptedStream applies to the accepted side, run
//     incrementally here) happen BEFORE the violating event is accepted into
//     the output stream, so failure_mode applies: "block" terminates with a
//     typed terminal error; "pass" drops the plugin's event and replays the
//     accepted event for that position.
//   - LATE violations (discovered at a scope close — ContentBlockStop,
//     MessageStop, or end-of-stream — after the block's earlier output has
//     already been forwarded) TERMINATE under BOTH failure modes with the
//     typed terminal error. Nothing is replayed and no rollback is pretended:
//     per-event forwarding is the default. Deferred tool decisions are held in
//     a bounded journal and can recover only while all affected bytes remain
//     unpublished; earlier published content is never rolled back.
//   - ACCEPTED-side defects (validateAcceptedStream / the incremental walker
//     on the host's own events) are HOST defects, never plugin verdicts:
//     they terminate as a typed host terminal regardless of any plugin's
//     failure_mode.
//
// Scope verification runs the policy transaction plus verifyStreamPrefix over
// whole-stream prefixes ending at each scope close, not over the block's
// events alone. Scope slices are
// unsound here: validateAcceptedStream's rules are whole-message rules
// (trailing signature_delta bindings, concurrent tool blocks still open at a
// sibling block's close, index reuse across the message), so a slice can be
// rejected for a defect it does not contain. Prefix checks deliberately defer
// strict missing-stop validation until MessageStop or EndStreamVerified: a
// sibling tool may legitimately still be open at another tool's stop. The
// transaction re-walks the prefix once per scope close — measured in
// BenchmarkStreamEnforcement.
//
// Enforcement is request-scoped: a streamVerifierState lives per reqID in
// PluginPipeline.streamVerify and is dropped by EndRequest, in the same place
// as the streamKinds tracker. RunOnStreamChunkVerified (used by the non-streaming
// JSON replay in jsonresponse.go) is untouched; the streaming path uses
// RunOnStreamChunkVerified + EndStreamVerified.

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"log"
	"regexp"
	"strconv"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// StreamTerminalError is the typed error a terminated stream returns. The
// proxy maps it to a VISIBLY ABNORMAL client outcome (truncated body, no
// finish marker, connection closed without the chunked terminator) rather
// than a clean completion; on the wire the client never sees a
// provider-originated StreamError — termination is a transport-level abort.
type StreamTerminalError struct {
	// Plugin names the plugin whose output violated the signed-stream
	// contract; "host" for accepted-stream defects (adapter faults).
	Plugin string
	// Kind is "plugin" (a plugin violated the contract) or "host" (the
	// accepted stream itself was malformed).
	Kind string
	// Index is the block index the violation is attributed to, or -1 when
	// the violation has no single block (e.g. a signature binding).
	Index int32
	// Scope is the 1-based ordinal of the closed scope the violation was
	// found in; 0 for per-event violations.
	Scope int
	// Err is the underlying verifier/discipline error; its text is the
	// violated invariant.
	Err error
}

const (
	// streamTerminalPlugin marks a plugin contract violation.
	streamTerminalPlugin = "plugin"
	// streamTerminalHost marks an accepted-stream (adapter) defect.
	streamTerminalHost = "host"
)

func (e *StreamTerminalError) Error() string {
	if e == nil {
		return "<nil>"
	}
	head := "stream plugin violation"
	if e.Kind == streamTerminalHost {
		head = "stream host defect"
	}
	if e.Index >= 0 {
		return fmt.Sprintf("%s: %s at block %d (scope %d): %v", head, e.Plugin, e.Index, e.Scope, e.Err)
	}
	return fmt.Sprintf("%s: %s (scope %d): %v", head, e.Plugin, e.Scope, e.Err)
}

// Unwrap exposes the underlying verifier/discipline error.
func (e *StreamTerminalError) Unwrap() error { return e.Err }

// streamDisciplineWalker enforces the per-event stream discipline as an
// INCREMENTAL state machine mirroring validateAcceptedStream's per-event
// rules exactly (same transitions, same error texts). It exists because the
// enforcement must reject a violating event BEFORE it is accepted into the
// output stream; validateAcceptedStream is a whole-slice function. The
// mirror is pinned by TestStreamDisciplineWalkerMirrorsValidateAcceptedStream.
//
// One instance validates the host's accepted events (host-defect domain);
// one instance per stream plugin validates that plugin's RETURNED events
// (plugin-violation domain). end() — the missing-stop check — runs for the
// host and every returned stream at MessageStop and EndStreamVerified; it is
// deliberately not a prefix rule because tool siblings may be concurrent.
type streamDisciplineWalker struct {
	nonTool        *openNonTool
	openTools      map[int32]pbv1.ToolInvocationKind
	seen           map[int32]bool
	sawError       bool
	messageStopped bool
	spanOpen       bool
	sawText        bool
	pos            int
}

func validateStreamToolDeltaFamily(kind pbv1.ToolInvocationKind, delta *pbv1.ToolCallDelta) error {
	if delta == nil {
		return fmt.Errorf("missing tool-call delta")
	}
	switch kind {
	case pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FUNCTION:
		if delta.InputTextDelta != nil {
			return fmt.Errorf("function tool call carries free-form input")
		}
	case pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM:
		if delta.InputTextDelta == nil || delta.ArgumentsDelta != "" {
			return fmt.Errorf("free-form tool call carries the wrong payload family")
		}
	default:
		return fmt.Errorf("tool call has unknown invocation kind %d", kind)
	}
	return nil
}

// walk validates one event and advances the walker state. The returned error
// names the violated per-event rule.
func (w *streamDisciplineWalker) walk(ev *pbv1.StreamEvent) error {
	if w.seen == nil {
		w.seen = make(map[int32]bool)
	}
	if w.openTools == nil {
		w.openTools = make(map[int32]pbv1.ToolInvocationKind)
	}
	pos := w.pos
	w.pos++
	if w.sawError {
		return fmt.Errorf("event after StreamError at position %d", pos)
	}
	if w.messageStopped {
		switch ev.Event.(type) {
		case *pbv1.StreamEvent_Usage, *pbv1.StreamEvent_Error:
		default:
			return fmt.Errorf("event at position %d after MessageStop", pos)
		}
	}
	switch e := ev.Event.(type) {
	case *pbv1.StreamEvent_Error:
		w.sawError = true
	case *pbv1.StreamEvent_Usage:
		// No stream state to enforce for Usage.
	case *pbv1.StreamEvent_MessageStart:
		// Message framing; nothing to enforce before MessageStop.
	case *pbv1.StreamEvent_MessageStop:
		if w.nonTool != nil || len(w.openTools) > 0 {
			return fmt.Errorf("MessageStop at position %d while a content block is still open", pos)
		}
		w.messageStopped = true
	case *pbv1.StreamEvent_ContentBlockStart:
		w.spanOpen = false // any start closes an implicit run
		cbs := e.ContentBlockStart
		if cbs == nil {
			return nil
		}
		idx := cbs.Index
		if w.seen[idx] {
			return fmt.Errorf("content block index %d reused at position %d", idx, pos)
		}
		w.seen[idx] = true
		switch cbs.Block.(type) {
		case *pbv1.ContentBlockStart_Text:
			if w.nonTool != nil {
				return fmt.Errorf("non-tool content block start at position %d while a %s block is open", pos, w.nonTool.kind)
			}
			if len(w.openTools) > 0 {
				return fmt.Errorf("non-tool content block start at position %d while a tool block is open", pos)
			}
			w.nonTool = &openNonTool{index: idx, kind: textBlockOpen}
		case *pbv1.ContentBlockStart_Thinking:
			if w.nonTool != nil {
				return fmt.Errorf("non-tool content block start at position %d while a %s block is open", pos, w.nonTool.kind)
			}
			if len(w.openTools) > 0 {
				return fmt.Errorf("non-tool content block start at position %d while a tool block is open", pos)
			}
			w.nonTool = &openNonTool{index: idx, kind: thinkingBlockOpen}
		case *pbv1.ContentBlockStart_Provider:
			if w.nonTool != nil {
				return fmt.Errorf("non-tool content block start at position %d while a %s block is open", pos, w.nonTool.kind)
			}
			if len(w.openTools) > 0 {
				return fmt.Errorf("non-tool content block start at position %d while a tool block is open", pos)
			}
			w.nonTool = &openNonTool{index: idx, kind: providerBlockOpen}
		case *pbv1.ContentBlockStart_ToolCall:
			if w.nonTool != nil {
				return fmt.Errorf("tool call block start at position %d while a non-tool block is open", pos)
			}
			w.openTools[idx] = cbs.GetToolCall().GetInvocationKind()
		}
	case *pbv1.StreamEvent_ContentBlockStop:
		cbs := e.ContentBlockStop
		if cbs == nil {
			return nil
		}
		idx := cbs.Index
		if _, ok := w.openTools[idx]; ok {
			delete(w.openTools, idx)
			return nil
		}
		if w.nonTool != nil && w.nonTool.index == idx {
			w.nonTool = nil
			w.spanOpen = false
			return nil
		}
		return fmt.Errorf("content block stop at position %d names no open block", pos)
	case *pbv1.StreamEvent_TextDelta:
		if w.nonTool != nil && w.nonTool.kind != textBlockOpen {
			return fmt.Errorf("text delta at position %d inside a %s block", pos, w.nonTool.kind)
		}
		if w.nonTool == nil && len(w.openTools) > 0 {
			return fmt.Errorf("text delta at position %d while a tool block is open", pos)
		}
		w.spanOpen, w.sawText = true, true
	case *pbv1.StreamEvent_ThinkingDelta:
		if w.nonTool != nil && w.nonTool.kind != thinkingBlockOpen {
			return fmt.Errorf("thinking delta at position %d inside a %s block", pos, w.nonTool.kind)
		}
		if w.nonTool == nil && len(w.openTools) > 0 {
			return fmt.Errorf("thinking delta at position %d while a tool block is open", pos)
		}
		w.spanOpen, w.sawText = true, true
	case *pbv1.StreamEvent_SignatureDelta:
		// Mirrors the scope walk: an open span or an open text/thinking block
		// is current (the signature ends the span); a signature inside an
		// open tool/provider block, or one with no text/thinking content
		// anywhere, binds nothing. On the accepted side that is a host
		// defect; on the returned side it is a floating token the plugin
		// could mint — a violation.
		if w.spanOpen || (w.nonTool != nil && (w.nonTool.kind == textBlockOpen || w.nonTool.kind == thinkingBlockOpen)) {
			w.spanOpen = false
			return nil
		}
		if (w.nonTool != nil && w.nonTool.kind == providerBlockOpen) || len(w.openTools) > 0 || !w.sawText {
			return fmt.Errorf("signature_delta has no covered content (does not bind tool-call blocks)")
		}
	case *pbv1.StreamEvent_ToolCallDelta:
		if e.ToolCallDelta == nil {
			return fmt.Errorf("tool call delta at position %d carries no delta", pos)
		}
		kind, ok := w.openTools[e.ToolCallDelta.Index]
		if !ok {
			return fmt.Errorf("tool call delta at position %d names no open tool block (%d)", pos, eventIndex(ev))
		}
		if err := validateStreamToolDeltaFamily(kind, e.ToolCallDelta); err != nil {
			return fmt.Errorf("tool call delta at position %d: %w", pos, err)
		}
	}
	return nil
}

// clone returns an independent validation snapshot. A rejected HookResult
// must not poison the live walker before pass-mode replay: a start can mark an
// index seen before a later overlap check rejects it.
func (w *streamDisciplineWalker) clone() *streamDisciplineWalker {
	out := *w
	if w.nonTool != nil {
		nt := *w.nonTool
		out.nonTool = &nt
	}
	if w.openTools != nil {
		out.openTools = make(map[int32]pbv1.ToolInvocationKind, len(w.openTools))
		for idx, open := range w.openTools {
			out.openTools[idx] = open
		}
	}
	if w.seen != nil {
		out.seen = make(map[int32]bool, len(w.seen))
		for idx, seen := range w.seen {
			out.seen[idx] = seen
		}
	}
	return &out
}

// end applies the end-of-stream rule: a stream that ends with an explicit
// content block still open and no StreamError to abandon it is malformed.
// The accepted walker reports host defects; returned walkers report plugin
// violations at their terminal boundaries.
func (w *streamDisciplineWalker) end() error {
	if !w.sawError && (w.nonTool != nil || len(w.openTools) > 0) {
		return fmt.Errorf("stream ends with an open content block; missing ContentBlockStop (or StreamError)")
	}
	return nil
}

// pluginStreamState is the per-plugin half of the per-request enforcement
// state: the accepted and returned event buffers (scope-verification input)
// and the returned-side discipline walker.
type streamJournal struct {
	originals     []*pbv1.StreamEvent
	outputs       []*pbv1.StreamEvent
	pending       map[int32]bool
	walker        *streamDisciplineWalker
	returnedStart int
	bytes         uint64
}

type pluginStreamState struct {
	// Deferred tool decisions form a transaction. Until it resolves, subsequent
	// outputs are held too, so pass-mode replay preserves original interleaving.
	journal  *streamJournal
	disabled bool // A rolled-back assembler cannot safely resume mid-stream.

	lp *loadedPlugin
	// accepted is every event this plugin saw as input, in call order.
	accepted []*pbv1.StreamEvent
	// returned is every event this plugin produced — after pass-mode replay
	// substitution — in the same call order as accepted.
	returned []*pbv1.StreamEvent
	// scopeStart is the accepted-buffer offset where the current scope began;
	// reset at every scope close. It only decides whether a final policy
	// transaction is pending; returned completeness is checked independently.
	scopeStart int
	// walker enforces the per-event returned-side discipline on this
	// plugin's output.
	walker *streamDisciplineWalker
	// scopeNum is this plugin's verified completed-scope ordinal. It intentionally
	// belongs to the plugin, not the request: upstream plugins may fan out or
	// move boundaries before this plugin observes them, and a plugin may CREATE
	// a complete returned scope before its accepted input closes.
	scopeNum int
	// acceptedCloseCount and returnedCloseCount are cumulative watermarks. A
	// topology writer may suppress a stop now and re-emit it on a later hook
	// call (or do the reverse), so per-call max would count one logical boundary
	// twice. scopeNum is always max of these two watermarks.
	acceptedCloseCount int
	returnedCloseCount int
}

// streamVerifierState is the per-request enforcement state: the host-side
// walker, one pluginStreamState per stream plugin, and the terminal flag.
// Owned by the single streaming goroutine for the request; the map entry is
// guarded by PluginPipeline.mu and dropped by EndRequest.
type streamVerifierState struct {
	host *streamDisciplineWalker
	// plugins is index-aligned with PluginPipeline.streamPlugins.
	plugins  []*pluginStreamState
	terminal *StreamTerminalError
}

func newStreamVerifierState(pp *PluginPipeline) *streamVerifierState {
	vs := &streamVerifierState{host: &streamDisciplineWalker{}}
	vs.plugins = make([]*pluginStreamState, len(pp.streamPlugins))
	for i, lp := range pp.streamPlugins {
		vs.plugins[i] = &pluginStreamState{lp: lp, walker: &streamDisciplineWalker{}}
	}
	return vs
}

// enforces reports whether any loaded plugin participates in stream
// enforcement (has run_on_stream_chunk). With no stream plugin there is
// nothing to police, so the verified path degrades to plain pass-through.
func (vs *streamVerifierState) enforces() bool {
	for _, pvs := range vs.plugins {
		if pvs != nil {
			return true
		}
	}
	return false
}

// terminate records the terminal flag (subsequent calls short-circuit) and
// returns the typed error.
func (vs *streamVerifierState) terminate(kind, plugin string, index int32, scope int, err error) *StreamTerminalError {
	term := &StreamTerminalError{Plugin: plugin, Kind: kind, Index: index, Scope: scope, Err: err}
	vs.terminal = term
	return term
}

// acceptPluginOutput atomically validates one complete HookResult candidate.
// A multi-event EmitEvents action is one replacement, never a sequence of
// independently committed writes. The returned walker is snapshotted first;
// on pass-mode failure every candidate event is discarded and the accepted
// input is replayed exactly once from the original state.
func (vs *streamVerifierState) acceptPluginOutputs(pvs *pluginStreamState, accepted *pbv1.StreamEvent, emitted []*pbv1.StreamEvent) ([]*pbv1.StreamEvent, *StreamTerminalError) {
	if vs.terminal != nil {
		return nil, vs.terminal
	}
	candidate := pvs.walker.clone()
	for _, ev := range emitted {
		if err := candidate.walk(ev); err != nil {
			if pvs.lp.failureMode == "block" {
				return nil, vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, eventIndex(ev), 0, err)
			}
			replay := pvs.walker.clone()
			if rerr := replay.walk(accepted); rerr != nil {
				return nil, vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, eventIndex(accepted), 0, rerr)
			}
			pvs.walker = replay
			return []*pbv1.StreamEvent{accepted}, nil
		}
	}
	pvs.walker = candidate
	return emitted, nil
}

// acceptPluginOutput is the single-event form retained for the focused state
// tests and for callers that have no fan-out. Real HookResult handling uses
// acceptPluginOutputs so the whole EmitEvents action remains atomic.
func (vs *streamVerifierState) acceptPluginOutput(pvs *pluginStreamState, accepted, emitted *pbv1.StreamEvent) (*pbv1.StreamEvent, *StreamTerminalError) {
	got, term := vs.acceptPluginOutputs(pvs, accepted, []*pbv1.StreamEvent{emitted})
	if term != nil {
		return nil, term
	}
	if len(got) != 1 {
		return nil, vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, eventIndex(emitted), 0, errors.New("single stream output produced wrong cardinality"))
	}
	return got[0], nil
}

// isScopeCloseEvent reports whether an accepted-side event closes a scope:
// a content-block stop (tool or non-tool), the message stop, or a terminal
// provider error. A StreamError abandons every open block, so it must also
// close any deferred journal transaction before the event can be released.
func isScopeCloseEvent(ev *pbv1.StreamEvent) bool {
	switch ev.Event.(type) {
	case *pbv1.StreamEvent_ContentBlockStop, *pbv1.StreamEvent_MessageStop, *pbv1.StreamEvent_Error:
		return true
	}
	return false
}

// closeScope is the focused-state-test convenience for one coincident
// accepted/returned close. Production records the actual close counts with
// recordScopeCloses below. The SDK field-policy transaction plus
// verifyStreamPrefix run over the whole-stream prefix ending at this scope. A violation is late
// (earlier output of the block has already been forwarded) and terminates
// under BOTH failure modes. An *acceptedStreamError from the verifier's own
// accepted-side validation is a HOST defect — the plugin's failure_mode never
// applies.
func (vs *streamVerifierState) closeScope(pvs *pluginStreamState) error {
	return vs.checkScope(pvs, pvs.recordScopeCloses(1, 1))
}

// recordScopeCloses advances cumulative close watermarks and returns their
// converged ordinal. It is deliberately cumulative: accepted Stop → suppress,
// then a later returned Stop is one scope, not two; similarly for early output
// followed by a later accepted Stop. A stage still calls checkScope whenever
// either argument is non-zero.
func (pvs *pluginStreamState) recordScopeCloses(accepted, returned int) int {
	pvs.acceptedCloseCount += accepted
	pvs.returnedCloseCount += returned
	pvs.scopeNum = pvs.acceptedCloseCount
	if pvs.returnedCloseCount > pvs.scopeNum {
		pvs.scopeNum = pvs.returnedCloseCount
	}
	return pvs.scopeNum
}

// checkScope verifies a completed transaction at an already-assigned ordinal.
// A single upstream HookResult may fan out several close events before the
// next plugin invocation. That fan-out is intentionally one atomic policy
// transaction, but its diagnostics use the last real ordinal rather than 0.
func (vs *streamVerifierState) checkScope(pvs *pluginStreamState, scope int) error {
	if err := verifyStreamPrefix(pvs.accepted, pvs.returned, pvs.lp.plugin.HasGrant); err != nil {
		var ae *acceptedStreamError
		if errors.As(err, &ae) {
			return vs.terminate(streamTerminalHost, "host", -1, scope, err)
		}
		return vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, toolBlockIndexFrom(err), scope, err)
	}
	// Signature binding has its own normative error classes (dropped/stale/
	// forged/added), so it runs before the broader registry transaction. The
	// field walk still applies to every successful signature transaction.
	if err := verifyStreamPolicy(pvs.accepted, pvs.returned, pvs.lp.plugin.HasGrant); err != nil {
		return vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, toolBlockIndexFrom(err), scope, err)
	}
	pvs.scopeStart = len(pvs.accepted)
	return nil
}

// eventIndex returns the block index an event names, or -1.
func eventIndex(ev *pbv1.StreamEvent) int32 {
	switch e := ev.Event.(type) {
	case *pbv1.StreamEvent_ContentBlockStart:
		if e.ContentBlockStart != nil {
			return e.ContentBlockStart.Index
		}
	case *pbv1.StreamEvent_ContentBlockStop:
		if e.ContentBlockStop != nil {
			return e.ContentBlockStop.Index
		}
	case *pbv1.StreamEvent_ToolCallDelta:
		if e.ToolCallDelta != nil {
			return e.ToolCallDelta.Index
		}
	}
	return -1
}

// toolBlockIndexRe extracts the attributed block index from the verifier's
// tool-scope error texts ("tool block %d signature stale", "invented signed
// tool block %d", "suppressed signed tool block %d", ...).
var toolBlockIndexRe = regexp.MustCompile(`tool block (\d+)`)

func toolBlockIndexFrom(err error) int32 {
	m := toolBlockIndexRe.FindStringSubmatch(err.Error())
	if m == nil {
		return -1
	}
	idx, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return -1
	}
	return int32(idx)
}

// RunOnStreamChunkVerified processes one stream event through the plugin
// pipeline WITH stream-signature enforcement, for the STREAMING path only.
// It is the sole stream dispatch entry and applies the pre-commit and
// scope-close rules above; a violation returns a typed *StreamTerminalError
// (never a plain error) and no event from the violating call is forwarded.
// Once the request's state is terminal, every subsequent call returns the
// terminal error without dispatching to any plugin.
//
// Non-streaming JSON replay also uses this entry point and buffers the result
// until EndStreamVerified succeeds, so it cannot bypass the same contract.
func (pp *PluginPipeline) RunOnStreamChunkVerified(ctx context.Context, reqID uint64, chunk *engine.StreamEvent) ([]engine.StreamEvent, error) {
	pp.Acquire()
	defer pp.Release()

	pp.mu.Lock()
	vs := pp.streamVerify[reqID]
	if vs == nil {
		vs = newStreamVerifierState(pp)
		if vs.enforces() {
			pp.streamVerify[reqID] = vs
		} else {
			vs = nil
		}
	}
	if vs != nil && vs.terminal != nil {
		err := vs.terminal
		pp.mu.Unlock()
		return nil, err
	}
	pp.mu.Unlock()
	return pp.runOnStreamChunk(ctx, reqID, chunk, vs)
}

// EndStreamVerified closes the final scope at end-of-stream: the host-side
// missing-stop check (a host defect) and the end-of-stream scope close for
// every plugin with events still pending since its last close. Called by the
// streaming host once the upstream event channel is exhausted, while the
// serialized response can still be aborted. Like the block-close checks this
// is late, so a violation terminates regardless of failure_mode.
func (pp *PluginPipeline) EndStreamVerified(reqID uint64) error {
	pp.Acquire()
	defer pp.Release()

	pp.mu.Lock()
	defer pp.mu.Unlock()
	vs := pp.streamVerify[reqID]
	if vs == nil {
		return nil
	}
	if vs.terminal != nil {
		return vs.terminal
	}
	// Host side: the accepted stream must not end with an open content block.
	if err := vs.host.end(); err != nil {
		return vs.terminate(streamTerminalHost, "host", -1, 0, err)
	}
	// Returned-side completeness is independent of whether a policy scope is
	// pending: a plugin may have suppressed its final stop, and that must not
	// be laundered as whole-block suppression under ir.stream.write.
	for _, pvs := range vs.plugins {
		if pvs == nil {
			continue
		}
		if err := pvs.walker.end(); err != nil {
			return vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, -1, pvs.scopeNum, err)
		}
	}
	// End-of-stream scope: anything accumulated since the last accepted-side
	// scope close (including a stream that never closed a scope). Each plugin's
	// ordinal follows its own accepted stream, which is the only stable model
	// after upstream fan-out/suppression.
	for _, pvs := range vs.plugins {
		if pvs == nil || pvs.scopeStart >= len(pvs.accepted) {
			continue
		}
		// A terminal trailing transaction has no completed close watermark to
		// pair. Give it a separate final ordinal without mutating either
		// watermark (EndStreamVerified is called once and no later scope can
		// converge with it).
		if err := vs.checkScope(pvs, pvs.scopeNum+1); err != nil {
			return err
		}
	}
	return nil
}

// runOnStreamChunk is the traversal owned by RunOnStreamChunkVerified. vs is
// nil when this pipeline generation has no verification work.
func (pp *PluginPipeline) runOnStreamChunk(ctx context.Context, reqID uint64, chunk *engine.StreamEvent, vs *streamVerifierState) ([]engine.StreamEvent, error) {
	pp.mu.Lock()
	tracker := pp.streamKinds[reqID]
	if tracker == nil {
		tracker = &pbconv.BlockKindTracker{}
		pp.streamKinds[reqID] = tracker
	}
	pp.mu.Unlock()

	hostEvent := pbconv.ToPBStreamEvent(chunk)
	if vs != nil {
		// Pre-commit host-side validation: a malformed ACCEPTED event is a
		// host/adaptor defect and terminates before the event is forwarded.
		if err := vs.host.walk(hostEvent); err != nil {
			return nil, vs.terminate(streamTerminalHost, "host", eventIndex(hostEvent), 0, err)
		}
	}

	current := []*pbv1.StreamEvent{hostEvent}
	for pi, lp := range pp.streamPlugins {
		var pvs *pluginStreamState
		callAcceptedStart := 0
		callReturnedStart := 0
		if vs != nil {
			pvs = vs.plugins[pi]
			callAcceptedStart = len(pvs.accepted)
			callReturnedStart = len(pvs.returned)
		}
		next := make([]*pbv1.StreamEvent, 0, len(current))
		for _, ev := range current {
			if pvs != nil {
				pvs.accepted = append(pvs.accepted, ev)
			}
			if pvs != nil && pvs.journal != nil {
				pvs.journal.originals = append(pvs.journal.originals, ev)
				pvs.journal.bytes += streamEventCost(ev)
			}
			var emitted []*pbv1.StreamEvent
			var callErr error
			if pvs != nil && pvs.disabled {
				emitted = []*pbv1.StreamEvent{ev}
			} else {
				emitted, callErr = pp.invokeStreamEvent(ctx, reqID, lp, ev)
			}
			if callErr == nil && pvs != nil {
				if len(emitted) == 1 && eventArm(ev) == eventArm(emitted[0]) {
					callErr = (streamPolicyDiff{canWrite: lp.plugin.HasGrant}).event(ev, emitted[0], "event")
				}
				if callErr == nil {
					candidate := pvs.walker.clone()
					for _, out := range emitted {
						if callErr = candidate.walk(out); callErr != nil {
							break
						}
					}
					if callErr == nil {
						pvs.walker = candidate
					}
				}
			}
			// Suppressing a tool start defers a whole block decision. It may later be
			// re-emitted or deliberately suppressed at its stop. Do not guess that the
			// absent start was already forwarded when a later guest invocation fails.
			if callErr == nil && pvs != nil && len(emitted) == 0 {
				if start := ev.GetContentBlockStart(); start != nil && start.GetToolCall() != nil {
					if pvs.journal == nil {
						pvs.journal = &streamJournal{originals: []*pbv1.StreamEvent{ev}, pending: map[int32]bool{}, walker: pvs.walker.clone(), returnedStart: len(pvs.returned), bytes: streamEventCost(ev)}
					}
					pvs.journal.pending[start.Index] = true
				}
			}
			if pvs != nil && pvs.journal != nil {
				for _, out := range emitted {
					pvs.journal.bytes += streamEventCost(out)
				}
				if pvs.journal.bytes > lp.plugin.StreamBufferLimit() {
					callErr = fmt.Errorf("deferred stream transaction exceeds its byte limit")
				}
			}
			if callErr != nil {
				log.Printf("[plugin] %s run_on_stream_chunk: %v", lp.manifest.Name, callErr)
				if lp.failureMode == "block" {
					if vs != nil {
						pvs.journal = nil
						return nil, vs.terminate(streamTerminalPlugin, lp.manifest.Name, eventIndex(ev), pvs.scopeNum, callErr)
					}
					return nil, fmt.Errorf("plugin %s blocked stream: %w", lp.manifest.Name, callErr)
				}
				if pvs != nil && pvs.journal != nil {
					replay, replayErr := rollbackStreamJournal(pvs)
					if replayErr != nil {
						return nil, vs.terminate(streamTerminalPlugin, lp.manifest.Name, eventIndex(ev), pvs.scopeNum, replayErr)
					}
					next = append(next, replay...)
					continue
				}
				emitted = []*pbv1.StreamEvent{ev}
				if pvs != nil {
					candidate := pvs.walker.clone()
					if err := candidate.walk(ev); err != nil {
						return nil, vs.terminate(streamTerminalPlugin, lp.manifest.Name, eventIndex(ev), pvs.scopeNum, err)
					}
					pvs.walker = candidate
				}
			}
			if pvs != nil {
				pvs.returned = append(pvs.returned, emitted...)
			}
			if pvs != nil && pvs.journal != nil {
				pvs.journal.outputs = append(pvs.journal.outputs, emitted...)
				if stop := ev.GetContentBlockStop(); stop != nil {
					delete(pvs.journal.pending, stop.Index)
				}
				if ev.GetError() != nil {
					// StreamError is terminal and legally abandons every open
					// content block. Resolve the corresponding deferred decisions;
					// the scope verifier below still runs before journal output is
					// allowed to leave this plugin stage.
					clear(pvs.journal.pending)
				}
			} else {
				next = append(next, emitted...)
			}
		}

		current = next

		// A completed scope on EITHER side is enough to verify the current
		// transaction before next escapes to another plugin or the serializer.
		// Accepted-side closes cover ordinary pass-through/suppression; returned-
		// side closes cover a plugin that creates a complete replacement block
		// from a non-close input. Without the latter, a last plugin could invent
		// a tool or text block and release it before EndStreamVerified catches it.
		// The two counters are deliberately paired: coincident accepted/returned
		// stops represent one scope, while fan-out on either side advances to the
		// last real ordinal in this single atomic HookResult batch.
		if vs != nil && pvs != nil {
			acceptedCloses, returnedCloses := 0, 0
			acceptedMessageStop, returnedMessageStop := false, false
			for i := callAcceptedStart; i < len(pvs.accepted); i++ {
				if isScopeCloseEvent(pvs.accepted[i]) {
					acceptedCloses++
				}
				if _, ok := pvs.accepted[i].Event.(*pbv1.StreamEvent_MessageStop); ok {
					acceptedMessageStop = true
				}
			}
			for i := callReturnedStart; i < len(pvs.returned); i++ {
				if isScopeCloseEvent(pvs.returned[i]) {
					returnedCloses++
				}
				if _, ok := pvs.returned[i].Event.(*pbv1.StreamEvent_MessageStop); ok {
					returnedMessageStop = true
				}
			}
			if acceptedCloses != 0 || returnedCloses != 0 {
				scope := pvs.recountCloses()
				// A pending tool block can still produce a complete replacement. Check
				// its policy transaction only when all deferred decisions have resolved.
				if pvs.journal == nil || len(pvs.journal.pending) == 0 {
					var verifyErr error
					if acceptedMessageStop || returnedMessageStop {
						verifyErr = pvs.walker.end()
					}
					if verifyErr == nil {
						verifyErr = vs.checkScope(pvs, scope)
					}
					if verifyErr != nil {
						if pvs.journal != nil && lp.failureMode == "pass" {
							replay, replayErr := rollbackStreamJournal(pvs)
							if replayErr == nil {
								vs.terminal = nil
								replayErr = vs.checkScope(pvs, pvs.recountCloses())
							}
							if replayErr != nil {
								return nil, vs.terminate(streamTerminalPlugin, lp.manifest.Name, -1, scope, replayErr)
							}
							next = append(next, replay...)
						} else {
							if vs.terminal != nil {
								return nil, vs.terminal
							}
							return nil, vs.terminate(streamTerminalPlugin, lp.manifest.Name, -1, scope, verifyErr)
						}
					}
				}
			}
			if pvs.journal != nil && len(pvs.journal.pending) == 0 {
				next = append(next, pvs.journal.outputs...)
				pvs.journal = nil
			}
			current = next

		}
	}

	out := make([]engine.StreamEvent, 0, len(current))
	for _, ev := range current {
		// Kind-aware conversion: the tracker remembers which content block is
		// ACTUALLY open at each index, so a ContentBlockStop becomes
		// ToolCallEnd or BlockStop to match the block it closes. On the
		// verified path the per-plugin discipline walkers have already
		// accepted every event here, so a conversion error is unreachable —
		// kept as a defensive hard error.
		converted, err := tracker.FromPBStreamEvent(ev)
		if err != nil {
			log.Printf("[plugin] stream topology error: %v", err)
			return nil, fmt.Errorf("plugin stream topology: %w", err)
		}
		out = append(out, *converted)
	}
	return out, nil
}

// invokeStreamEvent only proposes an action; verification and publication are
// owned by the caller's transaction, including every emitted child event.
func (pp *PluginPipeline) invokeStreamEvent(ctx context.Context, reqID uint64, lp *loadedPlugin, ev *pbv1.StreamEvent) ([]*pbv1.StreamEvent, error) {
	input, err := encodeHookInput(reqID, streamPayload{ev: ev})
	if err != nil {
		return nil, err
	}
	var output []byte
	pp.recordInvocation(reqID, lp.manifest.Name)
	if err = lp.plugin.CallRequest(ctx, pbv1.Hook_HOOK_ON_STREAM_CHUNK, reqID, input, &output); err != nil {
		return nil, err
	}
	result, err := decodeHookResult(output, pbv1.Hook_HOOK_ON_STREAM_CHUNK)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return []*pbv1.StreamEvent{ev}, nil
	}
	if result.GetSuppress() != nil {
		return nil, nil
	}
	if emit := result.GetEmitEvents(); emit != nil {
		return emit.Events, nil
	}
	return []*pbv1.StreamEvent{ev}, nil
}

func streamEventCost(ev *pbv1.StreamEvent) uint64 { return uint64(proto.Size(ev)) + 64 }

func rollbackStreamJournal(pvs *pluginStreamState) ([]*pbv1.StreamEvent, error) {
	journal := pvs.journal
	replay := journal.walker.clone()
	for _, ev := range journal.originals {
		if err := replay.walk(ev); err != nil {
			return nil, err
		}
	}
	pvs.walker = replay
	pvs.returned = append(pvs.returned[:journal.returnedStart], journal.originals...)
	// Replaying only this transaction must also restore the full relational
	// policy. A later signature can bind content emitted before the journal;
	// those already-published changes cannot be rolled back safely.
	if err := verifyStreamPrefix(pvs.accepted, pvs.returned, pvs.lp.plugin.HasGrant); err != nil {
		return nil, err
	}
	if err := verifyStreamPolicy(pvs.accepted, pvs.returned, pvs.lp.plugin.HasGrant); err != nil {
		return nil, err
	}
	pvs.journal = nil
	pvs.disabled = true
	pvs.recountCloses()
	return journal.originals, nil
}

func (pvs *pluginStreamState) recountCloses() int {
	pvs.acceptedCloseCount = 0
	pvs.returnedCloseCount = 0
	for _, ev := range pvs.accepted {
		if isScopeCloseEvent(ev) {
			pvs.acceptedCloseCount++
		}
	}
	for _, ev := range pvs.returned {
		if isScopeCloseEvent(ev) {
			pvs.returnedCloseCount++
		}
	}
	pvs.scopeNum = max(pvs.acceptedCloseCount, pvs.returnedCloseCount)
	return pvs.scopeNum
}
