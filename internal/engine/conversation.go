package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"

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
	if c == nil {
		return ""
	}
	// NOTE: the SDK replacement domain permits zero messages (a tools-only
	// request is valid), so a tools-only request WITH a tool marker keys
	// the tool prefix, and without a marker keys the whole (tool) request.
	// "" is reserved for nil — nothing cacheable.
	h := sha256.New()
	writeHashField(h, domainCachePrefix)
	writeHashField(h, c.Model)

	// The ordered-prefix reference model: the provider-visible serialization
	// order is TOOLS FIRST, then messages (outer blocks, then nested
	// tool-result content). The prefix closes at the LAST marker in that
	// order, at its exact carrier; a message marker supersedes an earlier
	// tool marker; with no marker the whole request is the automatic-cache
	// prefix. Only the prefix projection is hashed — never a complete
	// message containing a marker.
	last := lastCacheMarker(c)

	// Tools: with a tool marker as the LAST marker, only tools through it
	// are in the prefix; with a message marker (or none), every tool
	// precedes the prefix boundary and is hashed.
	toolLimit := len(c.Tools)
	if last != nil && last.kind == markerCarrierTool {
		toolLimit = last.tool + 1
	}
	for i, t := range c.Tools {
		if i >= toolLimit {
			break
		}
		writeHashField(h, "tool")
		writeHashField(h, t.Name)
		writeHashField(h, t.Description)
		// Raw authoritative bytes, framed: lexemes (1e999, key order)
		// are part of the cache identity, never canonicalized away.
		writeHashFieldBytes(h, t.Parameters.Bytes())
		writeHashFieldBytes(h, t.CacheControl.Bytes())
	}

	// Host-only envelope layer — deliberately DISTINCT from the SDK body
	// fingerprint below: provider extensions and safety settings are
	// top-level provider-visible prefix state the plugins never reproduce.
	writeHashFieldBytes(h, c.ProviderExtensions.Bytes())
	writeHashFieldBytes(h, c.SafetySettings.Bytes())

	if last == nil {
		// No marker: automatic prefix caching — the whole request is the
		// prefix (the key moves every turn; an honest reflection of a cache
		// the caller does not control).
		for i := range c.Messages {
			if !writeMessageFingerprint(h, &c.Messages[i]) {
				return ""
			}
		}
		return shortHex(h)
	}
	if last.kind == markerCarrierTool {
		// The prefix ended in the tools section: no message is part of it.
		return shortHex(h)
	}
	// The prefix ends at a message marker: full messages before it, then the
	// marker message truncated at the exact block/nested position (the
	// marker itself included).
	for i := range c.Messages {
		if i > last.msg {
			break
		}
		if i < last.msg {
			if !writeMessageFingerprint(h, &c.Messages[i]) {
				return ""
			}
			continue
		}
		pbMsg, err := MessageToPB(prefixMessage(&c.Messages[i], last.block, last.nested))
		if err != nil {
			return ""
		}
		writeHashField(h, "msg")
		writeHashField(h, plugin_sdk.RequestBlocksFingerprint(pbMsg))
	}
	return shortHex(h)
}

// writeMessageFingerprint hashes one full message with the SDK's shared
// body fingerprint; false on a projection error (the request is outside the
// closed domain — fail-safe empty key).
func writeMessageFingerprint(h hash.Hash, m *Message) bool {
	pbMsg, err := MessageToPB(m)
	if err != nil {
		return false
	}
	writeHashField(h, "msg")
	// The canonical body fingerprint: the SDK's shared implementation
	// (role + ordered blocks: kind, presence, order, identities, exact
	// raw bytes, signatures, nested tool-result content, cache positions —
	// typed length framing). The cache-tier stickiness mirror uses the same
	// implementation; Edge never re-implements block semantics.
	writeHashField(h, plugin_sdk.RequestBlocksFingerprint(pbMsg))
	return true
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

// lastCacheMarker returns the LAST marker in serialization order (tools
// first, then messages: outer blocks, then nested tool-result content), or
// nil when the request carries no marker.
func lastCacheMarker(c *ChatRequest) *cacheMarkerPos {
	var last *cacheMarkerPos
	for i, t := range c.Tools {
		if !t.CacheControl.IsAbsent() {
			last = &cacheMarkerPos{kind: markerCarrierTool, tool: i}
		}
	}
	for i := range c.Messages {
		for j, b := range c.Messages[i].Blocks {
			if b.CacheBreakpoint != nil {
				last = &cacheMarkerPos{kind: markerCarrierTopLevel, msg: i, block: j, nested: -1}
			}
			if b.ToolResult != nil {
				for k, cblk := range b.ToolResult.Content {
					if cblk.CacheBreakpoint != nil {
						last = &cacheMarkerPos{kind: markerCarrierNested, msg: i, block: j, nested: k}
					}
				}
			}
		}
	}
	return last
}

// prefixMessage returns the message truncated at the marker position,
// inclusive: blocks[0..lastBlock], with the marker block's tool-result
// content cut at nested[0..lastNested] when the marker is nested.
//
// PURE: the truncated carrier is deep-copied — the nested content slice is
// rebuilt, never aliased — so computing a cache key can never truncate the
// caller's live request (the *ToolResultBlock pointer must not be written
// through).
func prefixMessage(m *Message, lastBlock, lastNested int) *Message {
	out := Message{Role: m.Role}
	for j, b := range m.Blocks {
		if j > lastBlock {
			break
		}
		if j == lastBlock && lastNested >= 0 && b.ToolResult != nil {
			nb := b
			tr := *b.ToolResult
			tr.Content = append([]ToolResultContentBlock(nil), b.ToolResult.Content[:lastNested+1]...)
			nb.ToolResult = &tr
			out.Blocks = append(out.Blocks, nb)
			continue
		}
		out.Blocks = append(out.Blocks, b)
	}
	return &out
}

// MessageToPB projects a message to its pb/v2 form for the SHARED SDK
// fingerprint. It is a CHECKED projection: the pointer-sum representation
// can carry zero or multiple arms, and the conversion would silently pick
// the first — so the single-arm invariant (and the nested tool-result
// conflict rule) is enforced HERE before any fact can be discarded. The
// pbconv converter is the request-path source of truth; the differential
// matrix (TestMessageToPBDifferential in pbconv) pins byte equality between
// this projection and pbconv's per-message output so the two can never
// drift.
func MessageToPB(m *Message) (*pb.Message, error) {
	if err := ValidateMessage(m); err != nil {
		return nil, err
	}
	out := &pb.Message{Role: string(m.Role)}
	for _, b := range m.Blocks {
		rb := &pb.RequestBlock{}
		switch {
		case b.Text != nil:
			rb.Kind = &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: b.Text.Text, Signature: b.Text.Signature}}
		case b.Thinking != nil:
			rb.Kind = &pb.RequestBlock_Thinking{Thinking: &pb.RequestThinkingBlock{Text: b.Thinking.Text, Signature: b.Thinking.Signature}}
		case b.RedactedThinking != nil:
			rb.Kind = &pb.RequestBlock_RedactedThinking{RedactedThinking: &pb.RequestRedactedThinkingBlock{Data: b.RedactedThinking.Data}}
		case b.ToolUse != nil:
			rb.Kind = &pb.RequestBlock_ToolUse{ToolUse: &pb.RequestToolUseBlock{
				Id: b.ToolUse.ID, Name: b.ToolUse.Name,
				ArgumentsJson: b.ToolUse.Arguments.Bytes(),
				Signature:     b.ToolUse.Signature,
			}}
		case b.ToolResult != nil:
			tr := &pb.RequestToolResultBlock{ToolCallId: b.ToolResult.ToolCallID, ToolName: b.ToolResult.ToolName}
			for _, c := range b.ToolResult.Content {
				tcb := &pb.ToolResultContentBlock{}
				switch {
				case c.Unknown != nil:
					tcb.Kind = &pb.ToolResultContentBlock_Unknown{Unknown: &pb.ToolResultUnknownBlock{
						Kind: c.Unknown.Kind, PayloadJson: c.Unknown.Payload.Bytes(),
					}}
				case c.CacheBreakpoint != nil:
					tcb.Kind = &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{
						MarkerJson: c.CacheBreakpoint.Marker.Bytes(),
					}}
				default:
					tcb.Kind = &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: c.Text}}
				}
				tr.Content = append(tr.Content, tcb)
			}
			rb.Kind = &pb.RequestBlock_ToolResult{ToolResult: tr}
		case b.CacheBreakpoint != nil:
			rb.Kind = &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{
				MarkerJson: b.CacheBreakpoint.Marker.Bytes(),
			}}
		case b.Unknown != nil:
			rb.Kind = &pb.RequestBlock_Unknown{Unknown: &pb.RequestUnknownBlock{
				Kind: b.Unknown.Kind, PayloadJson: b.Unknown.Payload.Bytes(),
			}}
		case b.TrailingSignature != nil:
			rb.Kind = &pb.RequestBlock_TrailingSignature{TrailingSignature: &pb.RequestTrailingSignatureBlock{
				Signature: b.TrailingSignature.Signature,
			}}
		}
		out.Blocks = append(out.Blocks, rb)
	}
	// The FULL message domain, not just the structural facts: the SDK's
	// absolute rules (role presence, UTF-8, tool-use identity, trailing
	// placement) apply to the cache projection exactly like every other
	// owning boundary — an SDK-invalid message must never be hashed.
	if err := (&pb.ChatRequest{Messages: []*pb.Message{out}}).ValidateReplacement(); err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateMessage checks the ENGINE-side structural facts of one message
// that the protobuf oneof cannot carry: exactly one block arm per block,
// and no nested tool-result conflicts. The remaining absolute rules (role,
// UTF-8, identity, JSON shapes, trailing placement) are the SDK validator's
// common domain, applied by the checked Engine->PB boundary.
func ValidateMessage(m *Message) error {
	if m == nil {
		return fmt.Errorf("message is nil")
	}
	for j := range m.Blocks {
		if err := validateBlock(&m.Blocks[j]); err != nil {
			return fmt.Errorf("block %d: %w", j, err)
		}
	}
	return nil
}

func validateBlock(b *Block) error {
	arms := 0
	if b.Text != nil {
		arms++
	}
	if b.Thinking != nil {
		arms++
	}
	if b.RedactedThinking != nil {
		arms++
	}
	if b.ToolUse != nil {
		arms++
	}
	if b.ToolResult != nil {
		arms++
	}
	if b.CacheBreakpoint != nil {
		arms++
	}
	if b.Unknown != nil {
		arms++
	}
	if b.TrailingSignature != nil {
		arms++
	}
	switch arms {
	case 0:
		return fmt.Errorf("block has zero arms")
	case 1:
	default:
		return fmt.Errorf("block has %d arms, want exactly one", arms)
	}
	if b.ToolResult != nil {
		if len(b.ToolResult.Content) == 0 {
			return fmt.Errorf("tool result has empty nested content")
		}
		for k := range b.ToolResult.Content {
			c := &b.ToolResult.Content[k]
			nested := 0
			if c.Text != "" {
				nested++
			}
			if c.Unknown != nil {
				nested++
			}
			if c.CacheBreakpoint != nil {
				nested++
			}
			if nested > 1 {
				return fmt.Errorf("nested content element %d has %d conflicting arms", k, nested)
			}
		}
	}
	return nil
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
