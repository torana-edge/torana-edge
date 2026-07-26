package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
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

// CachePrefixKey returns a key identifying the provider-side cache entry this
// request would hit, or "" when there is nothing cacheable.
//
// The prefix is taken to run up to and including the last explicit cache
// breakpoint (Message.CacheControl or ToolDef.CacheControl — the IR carries
// these verbatim for every format that has them, so Anthropic, Bedrock, and any
// chat-completions provider that adopts the same shape are handled by one code
// path). With no breakpoint anywhere, the provider is doing automatic prefix
// caching and the whole request is the prefix, so everything is hashed; such a
// key changes every turn, which is an honest reflection of a cache the caller
// does not control.
//
// The model is included because provider caches never span models.
//
// This is a fingerprint of the IR, not of the provider's token sequence, which
// Torana cannot reproduce. It is built to err safely: any change to the prefix
// changes the key. Two requests sharing a key have genuinely identical prefixes;
// a changed key may occasionally mean only that something cosmetic moved, whose
// cost is one unnecessary re-warm.
func CachePrefixKey(c *ChatRequest) string {
	if c == nil || len(c.Messages) == 0 {
		return ""
	}
	h := sha256.New()
	writeHashField(h, domainCachePrefix)
	writeHashField(h, c.Model)

	// Tool definitions precede messages in the cached prefix for every format
	// that caches them, so they are hashed first and in wire order.
	for _, t := range c.Tools {
		writeHashField(h, "tool")
		writeHashField(h, t.Name)
		writeHashField(h, t.Description)
		writeHashField(h, canonicalJSON(t.Parameters))
		writeHashField(h, canonicalJSON(t.CacheControl))
	}

	for _, m := range c.Messages[:cachedMessageCount(c)] {
		writeHashField(h, "msg")
		writeHashField(h, string(m.Role))
		writeHashField(h, m.Content)
		writeHashField(h, canonicalJSON(m.ContentParts))
		writeHashField(h, m.Thinking)
		writeHashField(h, m.ThinkingSignature)
		writeHashField(h, m.RedactedThinking)
		writeHashField(h, m.ToolCallID)
		writeHashField(h, m.ToolName)
		writeHashField(h, canonicalJSON(m.CacheControl))
		for _, tc := range m.ToolCalls {
			writeHashField(h, "call")
			writeHashField(h, tc.ID)
			writeHashField(h, tc.Name)
			writeHashField(h, canonicalJSON(tc.Arguments))
			writeHashField(h, tc.Signature)
		}
	}
	return shortHex(h)
}

// cachedMessageCount returns how many leading messages fall inside the cached
// prefix: through the last message carrying a breakpoint, or all of them when
// no message does.
func cachedMessageCount(c *ChatRequest) int {
	last := -1
	for i, m := range c.Messages {
		if len(m.CacheControl) > 0 {
			last = i
		}
	}
	if last < 0 {
		return len(c.Messages)
	}
	return last + 1
}

// messageText reduces a message to the bytes worth hashing for the conversation
// label. Multimodal content goes through JSON.
func messageText(m Message) string {
	if m.Content != "" {
		return m.Content
	}
	if len(m.ContentParts) > 0 {
		return canonicalJSON(m.ContentParts)
	}
	return ""
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
func writeHashField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}

func shortHex(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil)[:keyBytes])
}
