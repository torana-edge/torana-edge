package vertex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// StreamAdapter translates between Gemini/Code Assist SSE streams and
// StreamEvent channels. Code Assist streams are text/event-stream with
// `data: {"response": {<GenerateContentResponse>}, ...}` frames; bare Gemini
// line-delimited JSON (no data: prefix, no wrapper) is also tolerated.
type StreamAdapter struct{}

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
					if aborted := emitChunk(ch, payload, &lastUsage); aborted {
						return
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("vertex: read stream: %v", err)}}
				}
				return
			}
		}
	}()

	return ch
}

// emitChunk parses one SSE payload and pushes its events. Returns true if the
// stream should abort (unrecoverable error already sent).
func emitChunk(ch chan<- engine.StreamEvent, payload []byte, lastUsage **geminiUsageMetadata) bool {
	var frame streamFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("vertex: parse frame: %v", err)}}
		return true
	}
	chunk := frame.Response
	if chunk == nil {
		// Bare Gemini: the payload IS the chunk.
		chunk = &geminiStreamChunk{}
		if err := json.Unmarshal(payload, chunk); err != nil {
			ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("vertex: parse chunk: %v", err)}}
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
			if aborted := emitPart(ch, part); aborted {
				return true
			}
		}
	}

	if candidate.FinishReason != "" {
		reason := mapGeminiFinishReason(candidate.FinishReason)
		if reason != "" {
			if lu := *lastUsage; lu != nil && (lu.PromptTokenCount > 0 || lu.CandidatesTokenCount > 0) {
				ch <- engine.StreamEvent{Usage: &engine.StreamUsage{InputTokens: lu.PromptTokenCount, OutputTokens: lu.CandidatesTokenCount}}
				*lastUsage = nil
			}
			ch <- engine.StreamEvent{FinishReason: reason}
		}
	}
	return false
}

func emitPart(ch chan<- engine.StreamEvent, part geminiPart) bool {
	switch {
	case part.FunctionCall != nil:
		id := part.FunctionCall.ID
		if id == "" {
			id = part.FunctionCall.Name
		}
		ch <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: id, Name: part.FunctionCall.Name, Signature: part.ThoughtSignature}}
		argsJSON, err := json.Marshal(part.FunctionCall.Args)
		if err != nil {
			ch <- engine.StreamEvent{Error: &engine.StreamError{Code: -1, Message: fmt.Sprintf("vertex: marshal function call args: %v", err)}}
			return true
		}
		delta := string(argsJSON)
		ch <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: delta}}
		ch <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 0}}
	case part.Thought:
		if part.Text != "" {
			t := part.Text
			ch <- engine.StreamEvent{ThinkingDelta: &t}
		}
		if part.ThoughtSignature != "" {
			sig := part.ThoughtSignature
			ch <- engine.StreamEvent{SignatureDelta: &sig}
		}
	case part.Text != "":
		t := part.Text
		ch <- engine.StreamEvent{TextDelta: &t}
		if part.ThoughtSignature != "" {
			sig := part.ThoughtSignature
			ch <- engine.StreamEvent{SignatureDelta: &sig}
		}
	case part.ThoughtSignature != "":
		// Standalone signature part (Code Assist emits one on the final chunk).
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

type serializeState struct {
	ID        string
	Name      string
	Signature string
	ArgsJSON  strings.Builder
}

// SerializeStream writes StreamEvents as Code Assist SSE frames to writer.
func (s *StreamAdapter) SerializeStream(w io.Writer, events <-chan engine.StreamEvent) error {
	var toolState *serializeState
	var pendingUsage *engine.StreamUsage

	for event := range events {
		switch {
		case event.Error != nil:
			_ = writeFrame(w, chunkFinish("OTHER", nil))
			return fmt.Errorf("vertex: stream error: %s", event.Error.Message)

		case event.FinishReason != "":
			var usage *geminiUsageMetadata
			if pendingUsage != nil {
				usage = &geminiUsageMetadata{
					PromptTokenCount:     pendingUsage.InputTokens,
					CandidatesTokenCount: pendingUsage.OutputTokens,
					TotalTokenCount:      pendingUsage.InputTokens + pendingUsage.OutputTokens,
				}
			}
			return writeFrame(w, chunkFinish(mapCanonicalToGeminiFinishReason(event.FinishReason), usage))

		case event.Usage != nil:
			pendingUsage = event.Usage

		case event.TextDelta != nil:
			if err := writeFrame(w, chunkPart(geminiPart{Text: *event.TextDelta})); err != nil {
				return err
			}

		case event.ThinkingDelta != nil:
			if err := writeFrame(w, chunkPart(geminiPart{Thought: true, Text: *event.ThinkingDelta})); err != nil {
				return err
			}

		case event.SignatureDelta != nil:
			if err := writeFrame(w, chunkPart(geminiPart{ThoughtSignature: *event.SignatureDelta})); err != nil {
				return err
			}

		case event.ToolCallStart != nil:
			toolState = &serializeState{ID: event.ToolCallStart.ID, Name: event.ToolCallStart.Name, Signature: event.ToolCallStart.Signature}

		case event.ToolCallDelta != nil && toolState != nil:
			toolState.ArgsJSON.WriteString(event.ToolCallDelta.ArgumentsDelta)

		case event.ToolCallEnd != nil && toolState != nil:
			if err := emitFunctionCall(w, toolState); err != nil {
				return err
			}
			toolState = nil
		}
	}
	return nil
}

func emitFunctionCall(w io.Writer, st *serializeState) error {
	var args map[string]any
	if err := json.Unmarshal([]byte(st.ArgsJSON.String()), &args); err != nil {
		log.Printf("[vertex] function call %q: accumulated args are not valid JSON (%v): %.200s", st.Name, err, st.ArgsJSON.String())
		_ = writeFrame(w, chunkFinish("OTHER", nil))
		return fmt.Errorf("vertex: function call %q args invalid: %w", st.Name, err)
	}
	part := geminiPart{
		ThoughtSignature: st.Signature,
		FunctionCall:     &geminiFuncCall{Name: st.Name, Args: args, ID: st.ID},
	}
	return writeFrame(w, chunkPart(part))
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

// writeFrame emits one Code Assist SSE frame: `data: {"response": <chunk>}\n\n`.
func writeFrame(w io.Writer, chunk geminiStreamChunk) error {
	payload, err := json.Marshal(streamFrame{Response: &chunk})
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
