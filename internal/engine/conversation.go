package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"

	"google.golang.org/protobuf/proto"

	plugin_sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Conversation and cache-prefix identity.
//
// Torana needs to recognise two different things across turns: which
// conversation a request belongs to, and which cached prefix it would hit. Both
// are derived here from the canonical IR, so they behave identically no matter
// which wire format carried the request. A provider that vends chat completions
// but prices and caches like Anthropic gets the same identity treatment as
// Anthropic itself, because by the time these functions see it, the wire shape
// is gone.
//
// # Why not session IDs from the client
//
// Harnesses and observability proxies have conventions for this — Claude Code
// emits a session-ID header, Helicone and Langfuse define their own, and the
// Code Assist envelope carries an inner sessionId. All were rejected as inputs.
// Torana cannot know what harness someone runs, a feature that works only behind
// one of them is not a platform feature, and header names are a compatibility
// treadmill. Identity must hold with zero cooperation from the client, so it is
// derived from content the client cannot avoid sending.
//
// # The two keys are not interchangeable
//
// ConversationID is the durable, human-meaningful label: the thing a user picks
// out of a list and asks to keep warm. It is derived from the conversation's
// roots, so it does not move as turns accumulate.
//
// CachePrefixKey identifies the actual cache entry a warming ping must target.
// Provider caches are keyed by prefix bytes and model, so this key changes
// whenever either does — including when something rewrites history. That is
// correct rather than unfortunate: a rewritten prefix means the old cache entry
// is dead, and warming should follow the live one.
//
// Callers doing cache work need both. Collapsing them either way breaks: the
// label alone will happily warm a prefix that no longer exists, and the cache
// key alone cannot be written into a config file because it moves.
//
// keyBytes is the truncated SHA-256 width. 48 bits stays short enough to type
// into a config file by hand while keeping collisions negligible at the scale of
// one developer's machine.
const keyBytes = 6

// Domain tags keep the two derivations in separate spaces, so a conversation
// label can never be mistaken for a cache key or collide with one.
const (
	domainConversation = "torana/conversation/v1"
	domainCachePrefix  = "torana/cache-prefix/v1"
)

// ConversationID returns a stable short label for the conversation this request
// belongs to, derived from the parts that do not change turn to turn: the system
// prompt and the first user message. It returns "" for a request with neither,
// which callers must read as "unidentifiable" rather than lumping under a shared
// empty key.
//
// Model and tool definitions are deliberately excluded. A harness that switches
// sonnet → opus for one turn, or adds a tool mid-session, is still in the same
// conversation as far as anyone reading a list is concerned.
//
// Two conversations that open with the same system prompt and the same first
// user message share a label. This is a real collision and it is left in place:
// such requests also share a cached prefix, so for every cache purpose they are
// the same thing.
func ConversationID(c *ChatRequest) string {
	if c == nil || len(c.Messages) == 0 {
		return ""
	}
	h := sha256.New()
	writeHashField(h, domainConversation)

	stable := 0
	for _, m := range c.Messages {
		switch m.Role {
		case RoleSystem:
			// Every leading system message counts: harnesses split the system
			// prompt across several, and all of them are stable.
			writeHashField(h, "system")
			writeHashField(h, messageText(m))
			stable++
		case RoleUser:
			// The first user message pins the conversation; nothing after it is
			// stable enough to include.
			writeHashField(h, "user")
			writeHashField(h, messageText(m))
			return shortHex(h)
		}
	}
	if stable == 0 {
		return ""
	}
	return shortHex(h)
}

// CachePrefixKey returns a key identifying the provider-side cache entry
// this request would hit, or "" when there is nothing cacheable.
//
// The key operation takes an ALREADY-VALIDATED PB request (the checked
// Engine->PB boundary produces it) and is SELF-GATED by the SDK's own
// full-domain validator: an out-of-domain request (invalid tool
// definitions, top-level scalars, raw fields, or messages) yields the
// fail-safe "" before anything is hashed. There is no partial,
// hand-maintained validation here — the SDK's ValidateReplacement is the
// single table, and "" is reserved for nil and for out-of-domain requests
// (the SDK domain permits zero messages, so a tools-only request with a
// tool marker keys the tool prefix).
//
// The ordered-prefix reference model: the provider-visible serialization
// order is TOOLS FIRST, then messages (outer blocks, then nested
// tool-result content). The prefix closes at the LAST marker in that order,
// at its exact carrier; a message marker supersedes an earlier tool marker;
// with no marker the whole request is the automatic-cache prefix. Only the
// prefix projection is hashed — the marker message is truncated at the
// exact block/nested position (pure PB construction, never mutating or
// aliasing the input) and hashed with the SDK's shared body fingerprint.
// TopologyFacts carries the host-only reconstruction facts folded into
// the cache key (raw-JSON checkpoint section 4): the wire reconstruction
// depends on the Code Assist variant, the OpenAI variant, and the
// Responses input layout, so a key that ignored them could alias two
// different provider wires. The plugin mirror cannot reproduce these
// host-only facts (S1 obligation: cache_tier_selector reconciliation).
type TopologyFacts struct {
	CodeAssist           bool
	OpenAIVariant        OpenAIVariant
	ResponsesInputLayout OptionalJSONArray
}

// CachePrefixKey is the PB-only form with no topology facts (mirror
// stable; host call sites pass the facts via CachePrefixKeyTopology).
func CachePrefixKey(pbReq *pb.ChatRequest) string {
	return CachePrefixKeyTopology(pbReq, TopologyFacts{})
}

// CachePrefixKeyTopology is CachePrefixKey with the typed host-only
// topology facts folded in.
func CachePrefixKeyTopology(pbReq *pb.ChatRequest, topo TopologyFacts) string {
	if pbReq == nil {
		return ""
	}
	// The owning gate: the SDK's own full-domain validator runs on the PB
	// the key hashes. A request outside the domain gets no key.
	if err := pbReq.ValidateReplacement(); err != nil {
		return ""
	}
	h := sha256.New()
	writeHashField(h, domainCachePrefix)
	writeHashField(h, pbReq.Model)

	last := lastCacheMarkerPB(pbReq)
	toolLimit := len(pbReq.Tools)
	if last != nil && last.kind == markerCarrierTool {
		toolLimit = last.tool + 1
	}
	for i, t := range pbReq.Tools {
		if i >= toolLimit {
			break
		}
		writeHashField(h, "tool")
		writeHashField(h, t.Name)
		writeHashField(h, t.Description)
		// Raw authoritative bytes, framed: lexemes (1e999, key order)
		// are part of the cache identity, never canonicalized away.
		writeHashFieldBytes(h, t.ParametersJson)
		writeHashFieldBytes(h, t.CacheControlJson)
	}

	// Host-only envelope layer — deliberately DISTINCT from the SDK body
	// fingerprint below: provider extensions and safety settings are
	// top-level provider-visible prefix state the plugins never reproduce.
	writeHashFieldBytes(h, pbReq.ProviderExtensionsJson)
	writeHashFieldBytes(h, pbReq.SafetySettingsJson)

	// The typed host-only TOPOLOGY facts that change reconstruction.
	writeHashField(h, "topology")
	if topo.CodeAssist {
		writeHashField(h, "codeassist")
	} else {
		writeHashField(h, "bare")
	}
	writeHashField(h, "variant")
	writeHashField(h, string(rune('0'+topo.OpenAIVariant)))
	writeHashFieldBytes(h, topo.ResponsesInputLayout.Bytes())

	// The SDK fingerprint is ERROR-RETURNING; every error is a fail-closed
	// EMPTY KEY — an unrepresentable body must never hash a partial prefix.
	fp := func(m *pb.Message) (string, bool) {
		s, err := plugin_sdk.RequestBlocksFingerprint(m)
		if err != nil {
			return "", false
		}
		return s, true
	}

	if last == nil {
		// No marker: automatic prefix caching — the whole request is the
		// prefix (the key moves every turn; an honest reflection of a cache
		// the caller does not control).
		for _, m := range pbReq.Messages {
			f, ok := fp(m)
			if !ok {
				return ""
			}
			writeHashField(h, "msg")
			writeHashField(h, f)
		}
		return shortHex(h)
	}
	if last.kind == markerCarrierTool {
		// The prefix ended in the tools section: no message is part of it.
		return shortHex(h)
	}
	for i, m := range pbReq.Messages {
		if i > last.msg {
			break
		}
		if i < last.msg {
			// The canonical body fingerprint: the SDK's shared implementation
			// (role + ordered blocks: kind, presence, order, identities,
			// exact raw bytes, signatures, nested tool-result content, cache
			// positions — typed length framing). The cache-tier stickiness
			// mirror uses the same implementation; Edge never re-implements
			// block semantics.
			f, ok := fp(m)
			if !ok {
				return ""
			}
			writeHashField(h, "msg")
			writeHashField(h, f)
			continue
		}
		trunc := truncatePBMessage(m, last.block, last.nested)
		f, ok := fp(trunc)
		if !ok {
			return ""
		}
		writeHashField(h, "msg")
		writeHashField(h, f)
	}
	return shortHex(h)
}

// markerCarrierKind distinguishes the three cache-marker carriers in the
// ordered-prefix model.
type markerCarrierKind int

const (
	markerCarrierTool markerCarrierKind = iota + 1
	markerCarrierTopLevel
	markerCarrierNested
)

// cacheMarkerPos is one marker's position in the ordered prefix.
type cacheMarkerPos struct {
	kind   markerCarrierKind
	tool   int // markerCarrierTool
	msg    int // markerCarrierTopLevel / markerCarrierNested
	block  int // markerCarrierTopLevel / markerCarrierNested
	nested int // markerCarrierNested; -1 otherwise
}

// lastCacheMarkerPB returns the LAST marker in serialization order (tools
// first, then messages: outer blocks, then nested tool-result content), or
// nil when the request carries no marker.
func lastCacheMarkerPB(pbReq *pb.ChatRequest) *cacheMarkerPos {
	var last *cacheMarkerPos
	for i, t := range pbReq.Tools {
		if len(t.CacheControlJson) > 0 {
			last = &cacheMarkerPos{kind: markerCarrierTool, tool: i}
		}
	}
	for i, m := range pbReq.Messages {
		for j, b := range m.Blocks {
			if b.GetCacheBreakpoint() != nil {
				last = &cacheMarkerPos{kind: markerCarrierTopLevel, msg: i, block: j, nested: -1}
			}
			if tr := b.GetToolResult(); tr != nil {
				for k, c := range tr.Content {
					if c.GetCacheBreakpoint() != nil {
						last = &cacheMarkerPos{kind: markerCarrierNested, msg: i, block: j, nested: k}
					}
				}
			}
		}
	}
	return last
}

// truncatePBMessage returns the message truncated at the marker position,
// inclusive: blocks[0..lastBlock], with the marker block's tool-result
// content cut at nested[0..lastNested] when the marker is nested. PURE PB
// construction (proto.Clone + fresh slices) — the input is never mutated or
// aliased, so computing a key can never truncate the caller's live request.
func truncatePBMessage(m *pb.Message, lastBlock, lastNested int) *pb.Message {
	out := &pb.Message{Role: m.Role}
	for j, b := range m.Blocks {
		if j > lastBlock {
			break
		}
		if j == lastBlock && lastNested >= 0 && b.GetToolResult() != nil {
			nb := proto.Clone(b).(*pb.RequestBlock)
			tr := nb.GetToolResult()
			tr.Content = tr.Content[:lastNested+1]
			out.Blocks = append(out.Blocks, nb)
			continue
		}
		out.Blocks = append(out.Blocks, b)
	}
	return out
}

// messageText reduces a message to the bytes worth hashing for the conversation
// label. Multimodal content goes through JSON.
func messageText(m Message) string {
	// The text projection in wire order; non-text blocks contribute nothing
	// to the conversation label's textual fingerprint.
	return m.Text()
}

// canonicalJSON renders a value deterministically. encoding/json emits object
// keys sorted, so equal maps always produce equal bytes — the property the
// hashes depend on for multimodal content, tool arguments, and cache markers.
func canonicalJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeHashField length-prefixes every input so field boundaries cannot be
// forged by content. Without it, a system prompt whose tail happens to look like
// the next field would hash identically to a different split of the same bytes,
// and two distinct conversations would share a key.
// writeHashFieldBytes frames raw authoritative JSON bytes into the cache
// hash: length-tagged, so identical content in different fields cannot
// collide and absent vs `{}` hash differently.
func writeHashFieldBytes(h hash.Hash, b []byte) {
	var lb [8]byte
	binary.BigEndian.PutUint64(lb[:], uint64(len(b)))
	h.Write(lb[:])
	h.Write(b)
}

func writeHashField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}

func shortHex(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil)[:keyBytes])
}
