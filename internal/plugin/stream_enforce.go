package plugin

// Stream-signature enforcement (Migration B part 2b).
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
//     per-event forwarding is the implementation default (no whole-block
//     buffering), so a violation found at block close is late by construction.
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
// as the streamKinds tracker. RunOnStreamChunk (used by the non-streaming
// JSON replay in jsonresponse.go) is untouched; the streaming path uses
// RunOnStreamChunkVerified + EndStreamVerified.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
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
	openTools      map[int32]bool
	seen           map[int32]bool
	sawError       bool
	messageStopped bool
	spanOpen       bool
	sawText        bool
	pos            int
}

// walk validates one event and advances the walker state. The returned error
// names the violated per-event rule.
func (w *streamDisciplineWalker) walk(ev *pbv2.StreamEvent) error {
	if w.seen == nil {
		w.seen = make(map[int32]bool)
	}
	if w.openTools == nil {
		w.openTools = make(map[int32]bool)
	}
	pos := w.pos
	w.pos++
	if w.sawError {
		return fmt.Errorf("event after StreamError at position %d", pos)
	}
	if w.messageStopped {
		switch ev.Event.(type) {
		case *pbv2.StreamEvent_Usage, *pbv2.StreamEvent_Error:
		default:
			return fmt.Errorf("event at position %d after MessageStop", pos)
		}
	}
	switch e := ev.Event.(type) {
	case *pbv2.StreamEvent_Error:
		w.sawError = true
	case *pbv2.StreamEvent_Usage:
		// No stream state to enforce for Usage.
	case *pbv2.StreamEvent_MessageStart:
		// Message framing; nothing to enforce before MessageStop.
	case *pbv2.StreamEvent_MessageStop:
		if w.nonTool != nil || len(w.openTools) > 0 {
			return fmt.Errorf("MessageStop at position %d while a content block is still open", pos)
		}
		w.messageStopped = true
	case *pbv2.StreamEvent_ContentBlockStart:
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
		case *pbv2.ContentBlockStart_Text:
			if w.nonTool != nil {
				return fmt.Errorf("non-tool content block start at position %d while a %s block is open", pos, w.nonTool.kind)
			}
			if len(w.openTools) > 0 {
				return fmt.Errorf("non-tool content block start at position %d while a tool block is open", pos)
			}
			w.nonTool = &openNonTool{index: idx, kind: textBlockOpen}
		case *pbv2.ContentBlockStart_Thinking:
			if w.nonTool != nil {
				return fmt.Errorf("non-tool content block start at position %d while a %s block is open", pos, w.nonTool.kind)
			}
			if len(w.openTools) > 0 {
				return fmt.Errorf("non-tool content block start at position %d while a tool block is open", pos)
			}
			w.nonTool = &openNonTool{index: idx, kind: thinkingBlockOpen}
		case *pbv2.ContentBlockStart_Provider:
			if w.nonTool != nil {
				return fmt.Errorf("non-tool content block start at position %d while a %s block is open", pos, w.nonTool.kind)
			}
			if len(w.openTools) > 0 {
				return fmt.Errorf("non-tool content block start at position %d while a tool block is open", pos)
			}
			w.nonTool = &openNonTool{index: idx, kind: providerBlockOpen}
		case *pbv2.ContentBlockStart_ToolCall:
			if w.nonTool != nil {
				return fmt.Errorf("tool call block start at position %d while a non-tool block is open", pos)
			}
			w.openTools[idx] = true
		}
	case *pbv2.StreamEvent_ContentBlockStop:
		cbs := e.ContentBlockStop
		if cbs == nil {
			return nil
		}
		idx := cbs.Index
		if w.openTools[idx] {
			delete(w.openTools, idx)
			return nil
		}
		if w.nonTool != nil && w.nonTool.index == idx {
			w.nonTool = nil
			w.spanOpen = false
			return nil
		}
		return fmt.Errorf("content block stop at position %d names no open block", pos)
	case *pbv2.StreamEvent_TextDelta:
		if w.nonTool != nil && w.nonTool.kind != textBlockOpen {
			return fmt.Errorf("text delta at position %d inside a %s block", pos, w.nonTool.kind)
		}
		if w.nonTool == nil && len(w.openTools) > 0 {
			return fmt.Errorf("text delta at position %d while a tool block is open", pos)
		}
		w.spanOpen, w.sawText = true, true
	case *pbv2.StreamEvent_ThinkingDelta:
		if w.nonTool != nil && w.nonTool.kind != thinkingBlockOpen {
			return fmt.Errorf("thinking delta at position %d inside a %s block", pos, w.nonTool.kind)
		}
		if w.nonTool == nil && len(w.openTools) > 0 {
			return fmt.Errorf("thinking delta at position %d while a tool block is open", pos)
		}
		w.spanOpen, w.sawText = true, true
	case *pbv2.StreamEvent_SignatureDelta:
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
	case *pbv2.StreamEvent_ToolCallDelta:
		if e.ToolCallDelta == nil || !w.openTools[e.ToolCallDelta.Index] {
			return fmt.Errorf("tool call delta at position %d names no open tool block (%d)", pos, eventIndex(ev))
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
		out.openTools = make(map[int32]bool, len(w.openTools))
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
type pluginStreamState struct {
	lp *loadedPlugin
	// accepted is every event this plugin saw as input, in call order.
	accepted []*pbv2.StreamEvent
	// returned is every event this plugin produced — after pass-mode replay
	// substitution — in the same call order as accepted.
	returned []*pbv2.StreamEvent
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
	// plugins is index-aligned with PluginPipeline.plugins; nil for plugins
	// without run_on_stream_chunk.
	plugins  []*pluginStreamState
	terminal *StreamTerminalError
}

func newStreamVerifierState(pp *PluginPipeline) *streamVerifierState {
	vs := &streamVerifierState{host: &streamDisciplineWalker{}}
	vs.plugins = make([]*pluginStreamState, len(pp.plugins))
	for i, lp := range pp.plugins {
		if hasHook(lp.manifest, "run_on_stream_chunk") {
			vs.plugins[i] = &pluginStreamState{lp: lp, walker: &streamDisciplineWalker{}}
		}
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
func (vs *streamVerifierState) acceptPluginOutputs(pvs *pluginStreamState, accepted *pbv2.StreamEvent, emitted []*pbv2.StreamEvent) ([]*pbv2.StreamEvent, *StreamTerminalError) {
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
			return []*pbv2.StreamEvent{accepted}, nil
		}
	}
	pvs.walker = candidate
	return emitted, nil
}

// acceptPluginOutput is the single-event form retained for the focused state
// tests and for callers that have no fan-out. Real HookResult handling uses
// acceptPluginOutputs so the whole EmitEvents action remains atomic.
func (vs *streamVerifierState) acceptPluginOutput(pvs *pluginStreamState, accepted, emitted *pbv2.StreamEvent) (*pbv2.StreamEvent, *StreamTerminalError) {
	got, term := vs.acceptPluginOutputs(pvs, accepted, []*pbv2.StreamEvent{emitted})
	if term != nil {
		return nil, term
	}
	if len(got) != 1 {
		return nil, vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, eventIndex(emitted), 0, errors.New("single stream output produced wrong cardinality"))
	}
	return got[0], nil
}

// acceptPassThrough commits an accepted event through the same snapshotting
// path as a HookResult. This keeps encode/decode/trap pass-mode recovery from
// having a different (and potentially state-poisoning) transition path.
func (vs *streamVerifierState) acceptPassThrough(pvs *pluginStreamState, ev *pbv2.StreamEvent) (*pbv2.StreamEvent, *StreamTerminalError) {
	got, term := vs.acceptPluginOutputs(pvs, ev, []*pbv2.StreamEvent{ev})
	if term != nil {
		return nil, term
	}
	if len(got) != 1 { // unreachable; defensive against future refactors.
		return nil, vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, eventIndex(ev), 0, errors.New("stream pass-through produced no event"))
	}
	return got[0], nil
}

// isScopeCloseEvent reports whether an accepted-side event closes a scope:
// a content-block stop (tool or non-tool) or the message stop.
func isScopeCloseEvent(ev *pbv2.StreamEvent) bool {
	switch ev.Event.(type) {
	case *pbv2.StreamEvent_ContentBlockStop, *pbv2.StreamEvent_MessageStop:
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
func eventIndex(ev *pbv2.StreamEvent) int32 {
	switch e := ev.Event.(type) {
	case *pbv2.StreamEvent_ContentBlockStart:
		if e.ContentBlockStart != nil {
			return e.ContentBlockStart.Index
		}
	case *pbv2.StreamEvent_ContentBlockStop:
		if e.ContentBlockStop != nil {
			return e.ContentBlockStop.Index
		}
	case *pbv2.StreamEvent_ToolCallDelta:
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
// It behaves like RunOnStreamChunk and additionally applies the pre-commit
// and scope-close rules above; a violation returns a typed *StreamTerminalError
// (never a plain error) and no event from the violating call is forwarded.
// Once the request's state is terminal, every subsequent call returns the
// terminal error without dispatching to any plugin.
//
// The non-streaming JSON replay (jsonresponse.go) keeps calling
// RunOnStreamChunk — enforcement belongs to the live streaming path, where
// the host cannot otherwise clear a stale token before it escapes.
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

// runOnStreamChunk is the shared traversal behind both entry points. vs is
// nil on the legacy (non-enforced) path; non-nil on the verified path.
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

	current := []*pbv2.StreamEvent{hostEvent}
	for pi, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_on_stream_chunk") {
			continue
		}
		var pvs *pluginStreamState
		callAcceptedStart := 0
		callReturnedStart := 0
		if vs != nil {
			pvs = vs.plugins[pi]
			callAcceptedStart = len(pvs.accepted)
			callReturnedStart = len(pvs.returned)
			pvs.accepted = append(pvs.accepted, current...)
		}
		next := make([]*pbv2.StreamEvent, 0, len(current))
		for _, ev := range current {
			evBytes, err := encodeHookInput(reqID, streamPayload{ev: ev})
			if err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk encode: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after encode failure: %w", lp.manifest.Name, err)
				}
				if vs != nil && pvs != nil {
					accepted, term := vs.acceptPassThrough(pvs, ev)
					if term != nil {
						return nil, term
					}
					next = append(next, accepted)
					pvs.returned = append(pvs.returned, accepted)
				} else {
					next = append(next, ev)
				}
				continue
			}
			var outBytes []byte
			pp.recordInvocation(reqID, lp.manifest.Name)
			if err := lp.plugin.CallRequest(ctx, pbv2.Hook_HOOK_ON_STREAM_CHUNK, reqID, evBytes, &outBytes); err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after failure: %w", lp.manifest.Name, err)
				}
				if vs != nil && pvs != nil {
					accepted, term := vs.acceptPassThrough(pvs, ev)
					if term != nil {
						return nil, term
					}
					next = append(next, accepted)
					pvs.returned = append(pvs.returned, accepted)
				} else {
					next = append(next, ev)
				}
				continue
			}
			res, err := decodeHookResult(outBytes, pbv2.Hook_HOOK_ON_STREAM_CHUNK)
			if err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk: invalid result: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after invalid output: %w", lp.manifest.Name, err)
				}
				if vs != nil && pvs != nil {
					accepted, term := vs.acceptPassThrough(pvs, ev)
					if term != nil {
						return nil, term
					}
					next = append(next, accepted)
					pvs.returned = append(pvs.returned, accepted)
				} else {
					next = append(next, ev)
				}
				continue
			}
			if res == nil {
				// Pass-through: the event is the plugin's own output, so it
				// must satisfy the returned-side discipline too.
				if vs != nil && pvs != nil {
					accepted, term := vs.acceptPassThrough(pvs, ev)
					if term != nil {
						return nil, term
					}
					next = append(next, accepted)
					pvs.returned = append(pvs.returned, accepted)
				} else {
					next = append(next, ev)
				}
				continue
			}
			if res.GetSuppress() != nil {
				// Deliberately emit nothing; distinct from pass-through.
				continue
			}
			if emit := res.GetEmitEvents(); emit != nil {
				// Validation already refused an empty or malformed list, so
				// this is a real replacement or fan-out. Validate and commit
				// the ENTIRE action atomically: a later bad child cannot leave
				// an earlier child forwarded under failure_mode=pass.
				if vs != nil && pvs != nil {
					accepted, term := vs.acceptPluginOutputs(pvs, ev, emit.Events)
					if term != nil {
						return nil, term
					}
					next = append(next, accepted...)
					pvs.returned = append(pvs.returned, accepted...)
				} else {
					next = append(next, emit.Events...)
				}
				continue
			}
			// Fallback pass-through (a result with no action): same discipline
			// as the explicit pass-through.
			if vs != nil && pvs != nil {
				accepted, term := vs.acceptPassThrough(pvs, ev)
				if term != nil {
					return nil, term
				}
				next = append(next, accepted)
				pvs.returned = append(pvs.returned, accepted)
			} else {
				next = append(next, ev)
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
				if _, ok := pvs.accepted[i].Event.(*pbv2.StreamEvent_MessageStop); ok {
					acceptedMessageStop = true
				}
			}
			for i := callReturnedStart; i < len(pvs.returned); i++ {
				if isScopeCloseEvent(pvs.returned[i]) {
					returnedCloses++
				}
				if _, ok := pvs.returned[i].Event.(*pbv2.StreamEvent_MessageStop); ok {
					returnedMessageStop = true
				}
			}
			if acceptedCloses != 0 || returnedCloses != 0 {
				// All accepted events in this HookResult are one atomic policy
				// transaction. Cumulative paired watermarks make delayed/early
				// matching stops converge while a multi-scope fan-out reports its
				// last real boundary.
				scope := pvs.recordScopeCloses(acceptedCloses, returnedCloses)
				if acceptedMessageStop || returnedMessageStop {
					if err := pvs.walker.end(); err != nil {
						return nil, vs.terminate(streamTerminalPlugin, pvs.lp.manifest.Name, -1, scope, err)
					}
				}
				if err := vs.checkScope(pvs, scope); err != nil {
					return nil, err
				}
			}
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
