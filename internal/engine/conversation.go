package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"

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
// The observable prefix is ONE SDK-owned projection
// (RequestObservablePrefix): it owns the full-domain validation gate
// (out-of-domain ⇒ error, never a partial projection), the
// tools-first/outer/nested last-marker model, exact truncation, and the
// model/tools/messages/extensions/safety/generation-params field set —
// Edge does NOT re-implement the marker traversal (the cache-tier
// reconciliation removed the duplicated algorithm; no second
// request-prefix implementation exists to drift).
//
// The Edge cache key frames that projection under its own domain and adds
// ONLY the host-only topology facts (raw-JSON checkpoint section 4): the
// wire reconstruction depends on the Code Assist variant, the OpenAI
// variant, and the Responses input layout, so a key that ignored them
// could alias two different provider wires. The plugin mirror cannot
// reproduce these host-only facts (S1 obligation: cache_tier_selector
// reconciliation); the topology layer is the ENTIRE divergence between
// the Edge key and the observable prefix.
//
// The concrete key BYTES changed vs the pre-reconciliation composition
// (the projection is now framed raw instead of re-fingerprinted
// field-by-field) while the approved sensitivity contract is preserved —
// acceptable pre-release; no legacy-key compatibility branch is wanted.
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
	// The SDK projection is SELF-GATED: any error (an out-of-domain request)
	// maps to the existing fail-closed EMPTY KEY — an unrepresentable body
	// must never hash a partial prefix. "" is reserved for nil and for
	// out-of-domain requests.
	prefix, _, err := pb.RequestObservablePrefix(pbReq)
	if err != nil {
		return ""
	}
	h := sha256.New()
	writeHashField(h, domainCachePrefix)
	// The observable component, framed raw (length-prefixed): lexemes
	// (1e999, key order) are part of the cache identity, never
	// canonicalized away.
	writeHashFieldBytes(h, prefix)

	// The typed host-only TOPOLOGY facts that change reconstruction — the
	// entire divergence from the observable prefix.
	writeHashField(h, "topology")
	if topo.CodeAssist {
		writeHashField(h, "codeassist")
	} else {
		writeHashField(h, "bare")
	}
	writeHashField(h, "variant")
	writeHashField(h, strconv.Itoa(int(topo.OpenAIVariant)))
	writeHashFieldBytes(h, topo.ResponsesInputLayout.Bytes())

	return shortHex(h)
}

// messageText reduces a message to the bytes worth hashing for the conversation
// label. Multimodal content goes through JSON.
func messageText(m Message) string {
	// The text projection in wire order; non-text blocks contribute nothing
	// to the conversation label's textual fingerprint.
	return m.Text()
}

// writeHashFieldBytes frames raw authoritative JSON bytes into the cache
// hash: length-tagged, so identical content in different fields cannot
// collide and absent vs `{}` hash differently.
func writeHashFieldBytes(h hash.Hash, b []byte) {
	var lb [8]byte
	binary.BigEndian.PutUint64(lb[:], uint64(len(b)))
	h.Write(lb[:])
	h.Write(b)
}

// writeHashField length-prefixes every input so field boundaries cannot be
// forged by content. Without it, a system prompt whose tail happens to look like
// the next field would hash identically to a different split of the same bytes,
// and two distinct conversations would share a key.
func writeHashField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}

func shortHex(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil)[:keyBytes])
}
