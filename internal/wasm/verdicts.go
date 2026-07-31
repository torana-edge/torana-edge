package wasm

import (
	"fmt"
	"sync"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Plugin verdicts for one request.
//
// Every verdict is attributed to the plugin that issued it. v1 could not do
// this: verdicts arrived as anonymous keys in the returned request's
// ToranaMeta, so an operator seeing a blocked request could not tell which
// plugin blocked it.
type RequestVerdicts struct {
	mu sync.Mutex

	block    *BlockVerdict
	respond  *RespondVerdict
	route    *RouteVerdict
	identity *IdentityVerdict
}

// BlockVerdict rejects the request outright. It short-circuits the pipeline:
// downstream plugins never see a request that has already been refused.
type BlockVerdict struct {
	Plugin  string
	Status  int32
	Code    string
	Message string
}

// RespondVerdict serves a canned completion without calling upstream.
type RespondVerdict struct {
	Plugin  string
	Content string
}

// RouteVerdict overrides the provider and/or model for this request.
type RouteVerdict struct {
	Plugin   string
	Provider string
	Model    string
}

// IdentityVerdict overrides the rate-limit identity.
type IdentityVerdict struct {
	Plugin   string
	Identity string
}

func (v *RequestVerdicts) setBlock(plugin string, a *pbv2.BlockRequestArgs) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// First block wins. A later plugin cannot soften or restate another's
	// rejection, and attribution stays with whoever actually refused it.
	if v.block != nil {
		return
	}
	v.block = &BlockVerdict{Plugin: plugin, Status: a.Status, Code: a.Code, Message: a.Message}
}

func (v *RequestVerdicts) setRespond(plugin, content string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.respond != nil {
		return
	}
	v.respond = &RespondVerdict{Plugin: plugin, Content: content}
}

func (v *RequestVerdicts) setRoute(plugin, provider, model string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.route = &RouteVerdict{Plugin: plugin, Provider: provider, Model: model}
}

func (v *RequestVerdicts) setIdentity(plugin, identity string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.identity = &IdentityVerdict{Plugin: plugin, Identity: identity}
}

// Block returns the recorded block verdict, or nil.
func (v *RequestVerdicts) Block() *BlockVerdict {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.block
}

// Respond returns the recorded respond verdict, or nil.
func (v *RequestVerdicts) Respond() *RespondVerdict {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.respond
}

// Route returns the recorded route verdict, or nil.
func (v *RequestVerdicts) Route() *RouteVerdict {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.route
}

// Identity returns the recorded identity verdict, or nil.
func (v *RequestVerdicts) Identity() *IdentityVerdict {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.identity
}

// discardFrom drops the non-block verdicts issued by plugin.
//
// Trap semantics, settled in the plan: a BLOCK recorded before a trap
// SURVIVES — a security verdict fails closed, and a plugin that decided to
// refuse a request and then crashed still refused it. A respond or route from
// a plugin that then trapped is DISCARDED: a half-built synthetic response, or
// a reroute chosen by code that crashed immediately afterwards, is not
// trustworthy enough to act on.
func (v *RequestVerdicts) discardFrom(plugin string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.respond != nil && v.respond.Plugin == plugin {
		v.respond = nil
	}
	if v.route != nil && v.route.Plugin == plugin {
		v.route = nil
	}
	if v.identity != nil && v.identity.Plugin == plugin {
		v.identity = nil
	}
}

// VerdictsFor returns the verdicts recorded for reqID, or nil when none were.
func (r *Runtime) VerdictsFor(reqID uint64) *RequestVerdicts {
	r.verdictMu.RLock()
	defer r.verdictMu.RUnlock()
	return r.verdicts[reqID]
}

// DiscardTrappedVerdicts applies trap semantics for one plugin's failed call.
func (r *Runtime) DiscardTrappedVerdicts(reqID uint64, plugin string) {
	r.VerdictsFor(reqID).discardFrom(plugin)
}

// verdictsBucket returns the bucket for reqID, creating it on first use.
func (r *Runtime) verdictsBucket(reqID uint64) *RequestVerdicts {
	r.verdictMu.Lock()
	defer r.verdictMu.Unlock()
	if r.verdicts == nil {
		r.verdicts = make(map[uint64]*RequestVerdicts)
	}
	v, ok := r.verdicts[reqID]
	if !ok {
		v = &RequestVerdicts{}
		r.verdicts[reqID] = v
	}
	return v
}

// metaAppend atomically appends fragment to a request-scoped buffer and
// returns the buffer's state.
//
// Atomic because the alternative — the guest doing meta_get then meta_set — is
// two round trips with a lost update between them. Under concurrency that
// silently drops a fragment, and the corrupted tool call surfaces much later
// as invalid JSON reaching the agent.
//
// An empty fragment is the read path: it returns the buffer without creating
// the key, so a fail-open reader cannot resurrect a buffer that was never
// written.
func (r *Runtime) metaAppend(reqID uint64, key string, fragment []byte) (string, bool, error) {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	bucket, ok := r.meta[reqID]
	if !ok {
		if len(fragment) == 0 {
			return "", false, nil
		}
		bucket = make(map[string][]byte)
		r.meta[reqID] = bucket
	}
	existing, present := bucket[key]
	if len(fragment) == 0 {
		return string(existing), present, nil
	}
	if err := r.checkMetaBudget(bucket, key, len(existing)+len(fragment), len(fragment)); err != nil {
		return string(existing), present, err
	}
	// append keeps the slice's spare capacity in the map, so successive
	// fragments amortise. Building a new exact-size slice per call, or
	// concatenating strings, copied the whole buffer every time — O(total x
	// fragments) on exactly the hot path this exists to serve.
	bucket[key] = append(existing, fragment...)
	// Converting to string here would copy the whole buffer on every fragment
	// too. The ack path does not need the contents, and the read path
	// (empty fragment) converts once.
	return "", true, nil
}

// Host-side budgets for request-scoped metadata.
//
// This storage lives in the HOST, so it is not covered by the guest's 64 MiB
// WASM memory cap: an approved but buggy or adversarial plugin could otherwise
// grow host memory until the request ended. The limits are per key and per
// request, and exceeding either is a classified refusal rather than a silent
// truncation — a truncated tool call is worse than a refused one, because the
// agent will try to execute it.
const (
	maxMetaValueBytes   = 4 << 20  // 4 MiB per key
	maxMetaRequestBytes = 16 << 20 // 16 MiB across one request
)

// checkMetaBudget reports whether a write would exceed either budget.
//
// finalLen is the value's size AFTER the write; delta is the change to the
// request total. They are separate because a replacement is not growth: an
// earlier version passed delta for both, so replacing a 3.5 MiB value with a
// 7 MiB one checked only the 3.5 MiB increase and accepted a value well over
// the per-key limit.
//
// Caller holds metaMu.
func (r *Runtime) checkMetaBudget(bucket map[string][]byte, key string, finalLen, delta int) error {
	if finalLen > maxMetaValueBytes {
		return fmt.Errorf("metadata key would reach %d bytes, over the %d byte per-key limit",
			finalLen, maxMetaValueBytes)
	}
	total := delta
	for k, v := range bucket {
		total += len(k) + len(v)
	}
	if total > maxMetaRequestBytes {
		return fmt.Errorf("request metadata would reach %d bytes, over the %d byte limit",
			total, maxMetaRequestBytes)
	}
	return nil
}

// metaSetBounded writes a value subject to the same budgets.
func (r *Runtime) metaSetBounded(reqID uint64, key, value string) error {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	bucket, ok := r.meta[reqID]
	if !ok {
		bucket = make(map[string][]byte)
		r.meta[reqID] = bucket
	}
	// finalLen is the whole value; only the DELTA counts against the request
	// total, because replacing a key frees what it held.
	if err := r.checkMetaBudget(bucket, key, len(value), len(value)-len(bucket[key])); err != nil {
		return err
	}
	bucket[key] = []byte(value)
	return nil
}

// RecordRouteVerdictForTest records a route verdict the way the dispatcher
// does, so tests in other internal packages can set up a route-only plugin
// without compiling a guest.
func (r *Runtime) RecordRouteVerdictForTest(reqID uint64, plugin, provider, model string) {
	r.verdictsBucket(reqID).setRoute(plugin, provider, model)
}

// RecordBlockVerdictForTest and RecordRespondVerdictForTest record verdicts the
// way the dispatcher does, so tests in other internal packages can set up trap
// semantics without compiling a guest for every permutation.
func (r *Runtime) RecordBlockVerdictForTest(reqID uint64, plugin string, status int32, code, message string) {
	r.verdictsBucket(reqID).setBlock(plugin, &pbv2.BlockRequestArgs{
		Status: status, Code: code, Message: message,
	})
}

func (r *Runtime) RecordRespondVerdictForTest(reqID uint64, plugin, content string) {
	r.verdictsBucket(reqID).setRespond(plugin, content)
}
