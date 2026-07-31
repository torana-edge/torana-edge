package wasm

import (
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
