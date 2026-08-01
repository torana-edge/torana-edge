package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// StreamAdapter translates between Gemini SSE streams and StreamEvent channels.
//
// Parsing is flavor-agnostic and tolerant: it accepts `data:`-prefixed SSE or
// bare line-JSON, and unwraps a {"response":…} envelope when present. Only
// serialization differs by endpoint, controlled by Wrapped: Code Assist
// (Wrapped=true) emits `data: {"response":{<chunk>}}`, the public Gemini API /
// Vertex AI (Wrapped=false) emit bare `data: {<chunk>}`.
type StreamAdapter struct {
	Wrapped bool
}

// --- Stream wire types ---

// streamFrame is one SSE payload. Code Assist nests the chunk under "response";
// bare Gemini puts candidates/usageMetadata at the root (Response stays nil and
// we fall back to parsing the whole payload as a chunk).
type streamFrame struct {
	Response *geminiStreamChunk `json:"response"`
}

type geminiStreamChunk struct {
	Candidates    []geminiStreamCandidate `json:"candidates"`
	UsageMetadata *geminiUsageMetadata    `json:"usageMetadata,omitempty"`
	ModelVersion  string                  `json:"modelVersion,omitempty"`
	ResponseID    string                  `json:"responseId,omitempty"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	// Prompt tokens served from Gemini's (implicit or explicit) context
	// cache; subset of PromptTokenCount.
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

type geminiStreamCandidate struct {
	Content      *geminiStreamContent `json:"content,omitempty"`
	FinishReason string               `json:"finishReason,omitempty"`
}

type geminiStreamContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// --- ParseStream ---

// ParseStream reads a Gemini/Code Assist SSE (or bare line-JSON) response and
// emits StreamEvents. The channel closes when the stream ends or errors.
func (s *StreamAdapter) ParseStream(body io.Reader) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent)

	go func() {
		defer close(ch)
		reader := bufio.NewReader(body)
		var lastUsage *geminiUsageMetadata
		// Per-stream content-block counter. The ABI invariant requires block
		// indexes unique within one streamed message: every part that opens a
		// block — tool, text, or thinking — must receive a distinct sequential
		// index, shared by that block's Start/Delta/End. Shared across chunks
		// because parts arrive split over SSE frames.
		var blockIndex int
		for {
			line, err := reader.ReadBytes('\n')
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				payload := trimmed
				// Strip the SSE "data:" prefix if present.
				if rest, ok := bytes.CutPrefix(payload, []byte("data:")); ok {
					payload = bytes.TrimSpace(rest)
				} else if payload[0] != '{' && payload[0] != '[' {
					// Non-data SSE line (event:, id:, comment) — ignore.
					payload = nil
				}
				if bytes.Equal(payload, []byte("[DONE]")) {
					payload = nil
				}
				if len(payload) > 0 {
					if aborted := emitChunk(ch, payload, &lastUsage, &blockIndex); aborted {
						return
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("gemini: read stream: %v", err)}}
				}
				return
			}
		}
	}()

	return ch
}

// emitChunk parses one SSE payload and pushes its events. Returns true if the
// stream should abort (unrecoverable error already sent).
func emitChunk(ch chan<- engine.StreamEvent, payload []byte, lastUsage **geminiUsageMetadata, blockIndex *int) bool {
	var frame streamFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("gemini: parse frame: %v", err)}}
		return true
	}
	chunk := frame.Response
	if chunk == nil {
		// Bare Gemini: the payload IS the chunk.
		chunk = &geminiStreamChunk{}
		if err := json.Unmarshal(payload, chunk); err != nil {
			ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("gemini: parse chunk: %v", err)}}
			return true
		}
	}

	if chunk.UsageMetadata != nil {
		*lastUsage = chunk.UsageMetadata
	}
	if len(chunk.Candidates) == 0 {
		return false
	}
	candidate := chunk.Candidates[0]

	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if aborted := emitPart(ch, part, blockIndex); aborted {
				return true
			}
		}
	}

	if candidate.FinishReason != "" {
		reason := mapGeminiFinishReason(candidate.FinishReason)
		if reason != "" {
			if lu := *lastUsage; lu != nil && (lu.PromptTokenCount > 0 || lu.CandidatesTokenCount > 0) {
				ch <- engine.StreamEvent{Usage: &engine.StreamUsage{
					InputTokens:     lu.PromptTokenCount,
					OutputTokens:    lu.CandidatesTokenCount,
					CacheReadTokens: lu.CachedContentTokenCount,
				}}
				*lastUsage = nil
			}
			ch <- engine.StreamEvent{FinishReason: reason}
		}
	}
	return false
}

func emitPart(ch chan<- engine.StreamEvent, part geminiPart, blockIndex *int) bool {
	switch {
	case part.FunctionCall != nil:
		// One functionCall part is one tool-call block: assign the block's
		// sequential index once and share it across Start/Delta/End so the
		// events of a block stay bound together (SignatureScopeToolCallBlockByIndex).
		idx := *blockIndex
		*blockIndex = idx + 1
		id := part.FunctionCall.ID
		if id == "" {
			id = part.FunctionCall.Name
		}
		ch <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: idx, ID: id, Name: part.FunctionCall.Name, Signature: part.ThoughtSignature}}
		argsJSON, err := json.Marshal(part.FunctionCall.Args)
		if err != nil {
			ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("gemini: marshal function call args: %v", err)}}
			return true
		}
		delta := string(argsJSON)
		ch <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: idx, ArgumentsDelta: delta}}
		ch <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: idx}}
	case part.Thought:
		// One thought part is one thinking block — ARM PRESENCE decides:
		// thought:true opens a thinking block even when the text member is
		// empty or absent (v2 permits zero-delta blocks, and an empty block
		// still consumes an index). The signature rides INSIDE the open
		// block (the provider's same-part signature binds the current
		// block).
		idx := *blockIndex
		*blockIndex = idx + 1
		ch <- engine.StreamEvent{BlockStart: &engine.BlockStart{Index: idx, Kind: engine.BlockKindThinking}}
		if t := partText(part); t != "" {
			ch <- engine.StreamEvent{ThinkingDelta: &t}
		}
		if part.ThoughtSignature != "" {
			sig := part.ThoughtSignature
			ch <- engine.StreamEvent{SignatureDelta: &sig}
		}
		ch <- engine.StreamEvent{BlockStop: &engine.BlockStop{Index: idx}}
	case part.Text != nil && *part.Text == "" && part.ThoughtSignature != "":
		// Trailing standalone signature — kept NARROW to the settled Code
		// Assist shape: the NON-thinking explicit-empty-text signature part
		// (final {"text":"","thoughtSignature":…}). NO block events, so
		// the signature stays unbound to any open block. A thought:true
		// part with a signature is handled above as a thinking block with
		// the signature INSIDE it.
		sig := part.ThoughtSignature
		ch <- engine.StreamEvent{SignatureDelta: &sig}
	case part.Text != nil:
		// One text part is one text block — ARM PRESENCE decides: an
		// explicit text member opens a text block even when empty (a
		// zero-delta block consumes an index; dropping it would shift every
		// later block's index). Every block (tool, text, thinking) consumes
		// a sequential index from the shared per-stream counter: the v2
		// invariant requires unique indexes within one streamed message.
		idx := *blockIndex
		*blockIndex = idx + 1
		ch <- engine.StreamEvent{BlockStart: &engine.BlockStart{Index: idx, Kind: engine.BlockKindText}}
		if t := *part.Text; t != "" {
			ch <- engine.StreamEvent{TextDelta: &t}
		}
		if part.ThoughtSignature != "" {
			sig := part.ThoughtSignature
			ch <- engine.StreamEvent{SignatureDelta: &sig}
		}
		ch <- engine.StreamEvent{BlockStop: &engine.BlockStop{Index: idx}}
	case part.ThoughtSignature != "":
		// Bare signature part with no text member at all (absent, not
		// explicit-empty): not the contract trailing shape, but tolerated on
		// the stream path — standalone SignatureDelta, unbound.
		sig := part.ThoughtSignature
		ch <- engine.StreamEvent{SignatureDelta: &sig}
	}
	return false
}

// mapGeminiFinishReason maps Gemini finish reasons to canonical values.
func mapGeminiFinishReason(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER":
		return "error"
	default:
		return "stop"
	}
}

// --- SerializeStream ---

// serializeState buffers ONE tool call's wire part while its argument deltas
// arrive. State is keyed by the block index: the ABI permits concurrent tool
// blocks (parallel calls), and each index owns its own id/name/signature/args
// accumulator — a second start at a busy index or a delta/stop for an index
// that never started is malformed IR and errors rather than corrupting a
// sibling call's scope.
type serializeState struct {
	ID        string
	Name      string
	Signature string
	ArgsJSON  strings.Builder
}

// serializePart is one buffered, not-yet-flushed text or thinking part: the
// serializer accumulates its deltas and mid-block signature into a single
// wire part, flushed by the matching BlockStop. One part is open at a time,
// mirroring the ABI's single-open-block invariant.
type serializePart struct {
	isThinking bool
	text       strings.Builder
	sig        string // mid-block signature; empty means the part carries none
}

// SerializeStream writes StreamEvents as Gemini SSE frames to writer, wrapping
// each in {"response":…} for the Code Assist flavor (s.Wrapped).
func (s *StreamAdapter) SerializeStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error {
	// toolStates holds the buffered tool call for each block index opened by
	// ToolCallStart; concurrent tool blocks each own their index's state.
	toolStates := make(map[int]*serializeState)
	var openPart *serializePart
	var pendingUsage *engine.StreamUsage

	for event := range events {
		switch {
		case event.Error != nil:
			_ = writeFrame(w, chunkFinish("OTHER", nil), s.Wrapped)
			return fmt.Errorf("gemini: stream error: %s", event.Error.Message)

		case event.FinishReason != "":
			var usage *geminiUsageMetadata
			if pendingUsage != nil {
				usage = &geminiUsageMetadata{
					PromptTokenCount:        pendingUsage.InputTokens,
					CandidatesTokenCount:    pendingUsage.OutputTokens,
					TotalTokenCount:         pendingUsage.InputTokens + pendingUsage.OutputTokens,
					CachedContentTokenCount: pendingUsage.CacheReadTokens,
				}
			}
			return writeFrame(w, chunkFinish(mapCanonicalToGeminiFinishReason(event.FinishReason), usage), s.Wrapped)

		case event.Usage != nil:
			pendingUsage = event.Usage

		case event.BlockStart != nil:
			// Provider blocks cannot be rendered on the Gemini wire: the part
			// model has text/thought/functionCall arms but no provider slot,
			// and casting a provider block to a text part would silently
			// change the topology the host verified. Error instead — never
			// cast.
			if event.BlockStart.Kind == engine.BlockKindProvider {
				return fmt.Errorf("gemini: provider block kind %q is not supported by this serializer", event.BlockStart.ProviderKind)
			}
			// Open a text or thinking part. Well-formed streams have no part
			// open here (the ABI allows one block at a time); a leftover part
			// would mean the previous block never stopped, which the
			// stream-verifier rework polices — flush defensively rather than
			// lose the buffered content.
			if openPart != nil {
				if err := flushPart(w, openPart, s.Wrapped); err != nil {
					return err
				}
			}
			openPart = &serializePart{isThinking: event.BlockStart.Kind == engine.BlockKindThinking}

		case event.BlockStop != nil:
			if openPart != nil {
				if err := flushPart(w, openPart, s.Wrapped); err != nil {
					return err
				}
				openPart = nil
			}
			// No open part: a stray stop (e.g. a tool block's stop arriving as
			// BlockStop from a hand-built stream) has nothing to flush.

		case event.TextDelta != nil:
			if openPart != nil {
				// Delta inside the open part: accumulate — the provider part
				// model splits one part's text over many deltas.
				openPart.text.WriteString(*event.TextDelta)
			} else {
				// Bare delta with no open block (legacy paths/tests that emit
				// deltas without block events): emit a per-delta part, as
				// before blocks existed. Compat, kept for those callers.
				if err := writeFrame(w, chunkPart(geminiPart{Text: new(*event.TextDelta)}), s.Wrapped); err != nil {
					return err
				}
			}

		case event.ThinkingDelta != nil:
			if openPart != nil {
				openPart.text.WriteString(*event.ThinkingDelta)
			} else {
				if err := writeFrame(w, chunkPart(geminiPart{Thought: true, Text: new(*event.ThinkingDelta)}), s.Wrapped); err != nil {
					return err
				}
			}

		case event.SignatureDelta != nil:
			if openPart != nil {
				// Mid-block signature: attach to the open part — the
				// provider's same-part thoughtSignature binds the current
				// block. An empty signature is a CLEAR marker: it overwrites
				// any earlier signature and the flushed part carries none
				// (flushPart only emits non-empty signatures).
				openPart.sig = *event.SignatureDelta
			} else if *event.SignatureDelta != "" {
				// The standalone signature part keeps the provider's EXPLICIT
				// empty text member ({"text":"","thoughtSignature":…}): a bare
				// {"thoughtSignature":…} would not round-trip the text arm.
				if err := writeFrame(w, chunkPart(geminiPart{Text: new(""), ThoughtSignature: *event.SignatureDelta}), s.Wrapped); err != nil {
					return err
				}
			}
			// An empty signature with no open part is a clear marker with
			// nothing to clear: emit nothing rather than a signature-only
			// part for an empty token.

		case event.ToolCallStart != nil:
			if _, dup := toolStates[event.ToolCallStart.Index]; dup {
				return fmt.Errorf("gemini: duplicate tool call start at index %d", event.ToolCallStart.Index)
			}
			toolStates[event.ToolCallStart.Index] = &serializeState{
				ID:        event.ToolCallStart.ID,
				Name:      event.ToolCallStart.Name,
				Signature: event.ToolCallStart.Signature,
			}

		case event.ToolCallDelta != nil:
			st, ok := toolStates[event.ToolCallDelta.Index]
			if !ok {
				return fmt.Errorf("gemini: tool call delta for unknown index %d", event.ToolCallDelta.Index)
			}
			st.ArgsJSON.WriteString(event.ToolCallDelta.ArgumentsDelta)

		case event.ToolCallEnd != nil:
			st, ok := toolStates[event.ToolCallEnd.Index]
			if !ok {
				return fmt.Errorf("gemini: tool call end for unknown index %d", event.ToolCallEnd.Index)
			}
			if err := emitFunctionCall(w, st, s.Wrapped); err != nil {
				return err
			}
			delete(toolStates, event.ToolCallEnd.Index)
		}
	}
	return nil
}

// flushPart emits the buffered open part as ONE wire part, carrying the
// provider part's explicit text member (even when empty) and its mid-block
// signature when one is set.
func flushPart(w io.Writer, p *serializePart, wrapped bool) error {
	part := geminiPart{
		Text:    new(p.text.String()),
		Thought: p.isThinking,
	}
	if p.sig != "" {
		part.ThoughtSignature = p.sig
	}
	return writeFrame(w, chunkPart(part), wrapped)
}

func emitFunctionCall(w io.Writer, st *serializeState, wrapped bool) error {
	var args map[string]any
	if err := json.Unmarshal([]byte(st.ArgsJSON.String()), &args); err != nil {
		log.Printf("[gemini] function call %q: accumulated args are not valid JSON (%v): %.200s", st.Name, err, st.ArgsJSON.String())
		_ = writeFrame(w, chunkFinish("OTHER", nil), wrapped)
		return fmt.Errorf("gemini: function call %q args invalid: %w", st.Name, err)
	}
	part := geminiPart{
		ThoughtSignature: st.Signature,
		FunctionCall:     &geminiFuncCall{Name: st.Name, Args: args, ID: st.ID},
	}
	return writeFrame(w, chunkPart(part), wrapped)
}

func chunkPart(part geminiPart) geminiStreamChunk {
	return geminiStreamChunk{Candidates: []geminiStreamCandidate{{
		Content: &geminiStreamContent{Role: "model", Parts: []geminiPart{part}},
	}}}
}

func chunkFinish(reason string, usage *geminiUsageMetadata) geminiStreamChunk {
	return geminiStreamChunk{
		Candidates:    []geminiStreamCandidate{{FinishReason: reason}},
		UsageMetadata: usage,
	}
}

// mapCanonicalToGeminiFinishReason maps canonical finish reasons back to Gemini.
func mapCanonicalToGeminiFinishReason(r string) string {
	switch r {
	case "stop", "tool_calls", "length":
		return "STOP"
	case "error":
		return "OTHER"
	default:
		return "STOP"
	}
}

// writeFrame emits one SSE frame. Wrapped (Code Assist) nests the chunk under
// "response" — `data: {"response":<chunk>}\n\n`; bare (Gemini API / Vertex AI)
// emits the chunk directly — `data: {<chunk>}\n\n`.
func writeFrame(w io.Writer, chunk geminiStreamChunk, wrapped bool) error {
	var payload []byte
	var err error
	if wrapped {
		payload, err = json.Marshal(streamFrame{Response: &chunk})
	} else {
		payload, err = json.Marshal(chunk)
	}
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n\n"))
	return err
}
