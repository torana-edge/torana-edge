package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/format/streamio"
)

const maxStreamToolCalls = 128

var streamIdentityCounter atomic.Uint64

type parsedToolState struct {
	id   string
	kind engine.ToolInvocationKind
	args []byte
}

type guardedToolState struct {
	kind engine.ToolInvocationKind
	args []byte
}

// ParseStream parses an upstream stream using its native adapter, then applies
// the stricter completion contract required by a cross-protocol bridge. Content
// is forwarded as it arrives; only usage and the finish reason are retained so
// late usage can be delivered before the canonical terminal event.
func ParseStream(from Protocol, body io.Reader) <-chan engine.StreamEvent {
	out := make(chan engine.StreamEvent)
	go func() {
		defer close(out)
		if !from.Valid() {
			emitStreamError(out, unsupported("source streaming protocol"))
			return
		}

		observed := newObservedStream(from, body)
		native := streamAdapter(from).ParseStream(observed)
		var finish string
		var usage *engine.StreamUsage
		started := false
		finished := false
		activeTools := make(map[int]*parsedToolState)
		toolIDs := make(map[string]struct{})
		toolIndexes := make(map[int]struct{})
		hadTools := false
		messageID := ""
		bufferedToolBytes := 0
		fail := func(err error) {
			emitStreamError(out, err)
			for range native {
			}
		}

		for event := range native {
			if issue := observed.issue(); issue != nil {
				fail(issue)
				return
			}

			if event.Error != nil {
				out <- engine.StreamEvent{Error: sanitizedSourceError(from, event.Error.Code)}
				for range native {
				}
				return
			}
			if event.FinishReason != "" {
				if finished {
					fail(errors.New("protocol bridge: multiple source finish events"))
					return
				}
				finish, finished = event.FinishReason, true
				continue
			}
			if event.Usage != nil {
				copyUsage := *event.Usage
				usage = &copyUsage
				continue
			}
			if finished {
				fail(errors.New("protocol bridge: source content followed its finish event"))
				return
			}

			if event.MessageStart == nil && !started {
				started = true
				fallback := fallbackMessageStart(from)
				messageID = fallback.ID
				out <- engine.StreamEvent{MessageStart: fallback}
			}
			if event.MessageStart != nil {
				if started {
					fail(errors.New("protocol bridge: multiple source message starts"))
					return
				}
				started = true
				messageID = event.MessageStart.ID
				if messageID == "" {
					startCopy := *event.MessageStart
					startCopy.ID = generatedResponseID(from)
					event.MessageStart = &startCopy
					messageID = startCopy.ID
				}
			}
			if event.ToolCallStart != nil {
				hadTools = true
				tc := event.ToolCallStart
				if tc.Index < 0 || (tc.InvocationKind != engine.ToolInvocationFunction && tc.InvocationKind != engine.ToolInvocationFreeform) {
					fail(errors.New("protocol bridge: invalid streamed tool-call identity"))
					return
				}
				if len(toolIndexes) >= maxStreamToolCalls {
					fail(errors.New("protocol bridge: source stream has too many tool calls"))
					return
				}
				if _, exists := toolIndexes[tc.Index]; exists {
					fail(errors.New("protocol bridge: duplicate streamed tool-call index"))
					return
				}
				if tc.ID == "" {
					tcCopy := *tc
					tcCopy.ID = generatedCallID(messageID, tc.Index)
					event.ToolCallStart = &tcCopy
					tc = &tcCopy
				}
				if err := validateToolIdentity(from, tc.Name, tc.ID); err != nil {
					fail(err)
					return
				}
				if _, exists := toolIDs[tc.ID]; exists {
					fail(errors.New("protocol bridge: duplicate streamed tool-call ID"))
					return
				}
				activeTools[tc.Index] = &parsedToolState{id: tc.ID, kind: tc.InvocationKind}
				toolIDs[tc.ID] = struct{}{}
				toolIndexes[tc.Index] = struct{}{}
			}
			if event.ToolCallDelta != nil {
				state, exists := activeTools[event.ToolCallDelta.Index]
				if !exists {
					fail(errors.New("protocol bridge: tool-call delta has no matching start"))
					return
				}
				if state.kind != engine.ToolInvocationFreeform {
					fragment := event.ToolCallDelta.ArgumentsDelta
					if bufferedToolBytes+len(fragment) > streamio.MaxFrameBytes {
						fail(errors.New("protocol bridge: streamed tool arguments exceed the size limit"))
						return
					}
					state.args = append(state.args, fragment...)
					bufferedToolBytes += len(fragment)
				}
			}
			if event.ToolCallEnd != nil {
				state, exists := activeTools[event.ToolCallEnd.Index]
				if !exists {
					fail(errors.New("protocol bridge: tool-call end has no matching start"))
					return
				}
				if state.kind != engine.ToolInvocationFreeform {
					if _, err := engine.ParseRequiredJSONObject(state.args); err != nil {
						fail(errors.New("protocol bridge: streamed tool arguments are not a complete JSON object"))
						return
					}
				}
				delete(activeTools, event.ToolCallEnd.Index)
			}
			out <- event
		}

		if issue := observed.issue(); issue != nil {
			emitStreamError(out, issue)
			return
		}
		if !observed.terminal() {
			emitStreamError(out, errors.New("protocol bridge: upstream stream ended before its terminal marker"))
			return
		}
		if !finished {
			emitStreamError(out, errors.New("protocol bridge: upstream terminal marker carried no finish reason"))
			return
		}
		if len(activeTools) != 0 {
			emitStreamError(out, errors.New("protocol bridge: upstream stream ended with an open tool call"))
			return
		}
		if !started {
			out <- engine.StreamEvent{MessageStart: fallbackMessageStart(from)}
		}
		if usage != nil {
			out <- engine.StreamEvent{Usage: usage}
		}
		if (from == OpenAIResponses || from == Gemini || from == GeminiCodeAssist) && finish == "stop" && hadTools {
			finish = "tool_calls"
		}
		out <- engine.StreamEvent{FinishReason: finish}
	}()
	return out
}

func fallbackMessageStart(protocol Protocol) *engine.StreamMessageStart {
	return &engine.StreamMessageStart{Role: "assistant", ID: generatedResponseID(protocol)}
}

func generatedResponseID(protocol Protocol) string {
	prefix := "resp_torana"
	switch protocol {
	case OpenAIChat:
		prefix = "chatcmpl-torana"
	case Anthropic:
		prefix = "msg_torana"
	}
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err == nil {
		return fmt.Sprintf("%s_%x", prefix, entropy)
	}
	// crypto/rand failures are exceptional, but retaining a process-unique
	// fallback is safer than collapsing every missing upstream ID to one value.
	return fmt.Sprintf("%s_%d", prefix, streamIdentityCounter.Add(1))
}

func generatedCallID(responseID string, index int) string {
	sum := sha256.Sum256([]byte(responseID))
	return fmt.Sprintf("call_torana_%x_%d", sum[:6], index)

}

func sanitizedSourceError(from Protocol, code int) *engine.StreamError {
	return &engine.StreamError{Code: code, Message: fmt.Sprintf("protocol bridge: upstream %s stream failed", from)}
}

// SerializeStream validates and normalizes a canonical event stream before
// handing it to the destination's native serializer. The source protocol is
// needed solely for usage-unit and capability conversion; the caller's usage
// event is copied and never mutated.
func SerializeStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent, from, to Protocol, chat *engine.ChatRequest) error {
	if !from.Valid() {
		return unsupported("source streaming protocol")
	}
	if !to.Valid() {
		return unsupported("destination streaming protocol")
	}

	fallbackModel := ""
	if upstreamChat, ok := ctx.Value(engine.ChatRequestKey).(*engine.ChatRequest); ok && upstreamChat != nil {
		fallbackModel = upstreamChat.Model
	}
	ownedChat := engine.ChatRequest{}
	if chat != nil {
		ownedChat = *chat
	}
	to.ApplyTopology(&ownedChat)
	ctx = context.WithValue(ctx, engine.ChatRequestKey, &ownedChat)
	serializer := streamAdapter(to)
	if to == OpenAIResponses {
		envelope, err := openAIResponsesEnvelopeFromClient(&ownedChat)
		if err != nil {
			return err
		}
		serializer = &openai.StreamAdapter{
			EmitMessageStart:           true,
			ResponsesParallelToolCalls: &envelope.ParallelToolCalls,
			ResponsesToolChoice:        append(json.RawMessage(nil), envelope.ToolChoice...),
			ResponsesTools:             append(json.RawMessage(nil), envelope.Tools...),
		}
	}
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	guarded := make(chan engine.StreamEvent)
	guardResult := make(chan error, 1)
	emitUsage := to != OpenAIChat || openAIChatWantsUsage(chat)
	go func() {
		defer close(guarded)
		guardResult <- guardStream(guardCtx, guarded, events, from, to, emitUsage, fallbackModel)
	}()

	serializeErr := serializer.SerializeStream(guardCtx, w, guarded)
	cancel()
	guardErr := <-guardResult
	if guardErr != nil && !errors.Is(guardErr, context.Canceled) {
		return guardErr
	}
	return serializeErr
}

func guardStream(ctx context.Context, out chan<- engine.StreamEvent, events <-chan engine.StreamEvent, from, to Protocol, emitUsage bool, fallbackModel string) error {
	var finish string
	var usage *engine.StreamUsage
	finished := false
	activeTools := make(map[int]*guardedToolState)
	seenToolIndexes := make(map[int]struct{})
	messageStarted := false
	contentStarted := false
	bufferedToolBytes := 0

	send := func(event engine.StreamEvent) bool {
		select {
		case <-ctx.Done():
			return false
		case out <- event:
			return true
		}
	}
	fail := func(err error) error {
		if !send(engine.StreamEvent{Error: streamError(err)}) {
			return ctx.Err()
		}
		return err
	}

	for {
		var event engine.StreamEvent
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok = <-events:
		}
		if !ok {
			break
		}
		if event.Error != nil {
			safe := fmt.Errorf("protocol bridge: upstream %s stream failed", from)
			if !send(engine.StreamEvent{Error: &engine.StreamError{Code: event.Error.Code, Message: safe.Error()}}) {
				return ctx.Err()
			}
			return safe
		}
		if event.FinishReason != "" {
			if finished {
				return fail(errors.New("protocol bridge: multiple finish events"))
			}
			finish, finished = event.FinishReason, true
			continue
		}
		if event.Usage != nil {
			normalized, err := normalizeStreamUsage(*event.Usage, from, to)
			if err != nil {
				return fail(err)
			}
			if emitUsage && normalized.CacheWriteTokens != 0 && (to == OpenAIChat || to == Gemini || to == GeminiCodeAssist) {
				return fail(unsupported("cache write token accounting in " + string(to) + " streams"))
			}
			usage = &normalized
			continue
		}
		if finished {
			return fail(errors.New("protocol bridge: content followed the finish event"))
		}
		if event.MessageStart != nil {
			if messageStarted || contentStarted {
				return fail(errors.New("protocol bridge: message start arrived after stream content"))
			}
			messageStarted = true
			if event.MessageStart.Model == "" && fallbackModel != "" {
				copyStart := *event.MessageStart
				copyStart.Model = fallbackModel
				event.MessageStart = &copyStart
			}
		} else {
			contentStarted = true
		}
		if err := guardStreamEvent(event, from, to, activeTools, seenToolIndexes, &bufferedToolBytes); err != nil {
			return fail(err)
		}
		if !send(event) {
			return ctx.Err()
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if !finished {
		return fail(errors.New("protocol bridge: event stream closed before a finish event"))
	}
	if len(activeTools) != 0 {
		return fail(errors.New("protocol bridge: event stream closed with an open tool call"))
	}
	if to == Anthropic && usage == nil {
		return fail(errors.New("protocol bridge: Anthropic destination requires terminal stream usage"))
	}

	// Chat Completions carries usage in a separate chunk after its finish
	// choice. Other destinations either embed usage in their terminal frame or
	// buffer both fields, so they receive usage first.
	if to == OpenAIChat {
		if !send(engine.StreamEvent{FinishReason: finish}) {
			return ctx.Err()
		}
		if emitUsage && usage != nil && !send(engine.StreamEvent{Usage: usage}) {
			return ctx.Err()
		}
		return nil
	}
	if usage != nil && !send(engine.StreamEvent{Usage: usage}) {
		return ctx.Err()
	}
	if !send(engine.StreamEvent{FinishReason: finish}) {
		return ctx.Err()
	}
	return nil
}

func openAIChatWantsUsage(chat *engine.ChatRequest) bool {
	if chat == nil || chat.ProviderExtensions.IsAbsent() {
		return false
	}
	var extensions map[string]json.RawMessage
	if json.Unmarshal(chat.ProviderExtensions.Bytes(), &extensions) != nil {
		return false
	}
	var options struct {
		IncludeUsage bool `json:"include_usage"`
	}
	return json.Unmarshal(extensions["stream_options"], &options) == nil && options.IncludeUsage
}

func guardStreamEvent(event engine.StreamEvent, from, to Protocol, active map[int]*guardedToolState, seenToolIndexes map[int]struct{}, bufferedToolBytes *int) error {
	switch {
	case event.BlockStart != nil:
		if event.BlockStart.Kind == engine.BlockKindProvider && from != to {
			return unsupported("provider-specific streaming content blocks")
		}
		if event.BlockStart.Kind == engine.BlockKindThinking && from != to {
			return unsupported("streamed thinking blocks across these protocols")
		}
	case event.ThinkingDelta != nil:
		if from != to {
			return unsupported("streamed thinking blocks across these protocols")
		}
	case event.SignatureDelta != nil:
		if from != to || !supportsBlockSignatures(from) || !supportsBlockSignatures(to) {
			return unsupported("streamed provider signatures across these protocols")
		}
	case event.ToolCallStart != nil:
		tc := event.ToolCallStart
		if tc.Index < 0 || (tc.InvocationKind != engine.ToolInvocationFunction && tc.InvocationKind != engine.ToolInvocationFreeform) {
			return errors.New("protocol bridge: invalid streamed tool-call identity")
		}
		if err := validateToolIdentity(from, tc.Name, tc.ID); err != nil {
			return err
		}
		if err := validateToolIdentity(to, tc.Name, tc.ID); err != nil {
			return err
		}
		if len(seenToolIndexes) >= maxStreamToolCalls {
			return errors.New("protocol bridge: stream has too many tool calls")
		}
		if _, duplicate := seenToolIndexes[tc.Index]; duplicate {
			return errors.New("protocol bridge: duplicate tool-call start index")
		}
		if tc.InvocationKind == engine.ToolInvocationFreeform && (!supportsFreeform(from) || !supportsFreeform(to)) {
			return unsupported("streamed free-form tool calls across these protocols")
		}
		if tc.Signature != "" && (from != to || !supportsToolSignatures(from) || !supportsToolSignatures(to)) {
			return unsupported("streamed tool-call signatures across these protocols")
		}
		active[tc.Index] = &guardedToolState{kind: tc.InvocationKind}
		seenToolIndexes[tc.Index] = struct{}{}
	case event.ToolCallDelta != nil:
		state, exists := active[event.ToolCallDelta.Index]
		if !exists {
			return errors.New("protocol bridge: tool-call delta has no matching start")
		}
		if state.kind == engine.ToolInvocationFreeform {
			if event.ToolCallDelta.InputTextDelta == nil || event.ToolCallDelta.ArgumentsDelta != "" {
				return errors.New("protocol bridge: free-form tool delta has the wrong payload family")
			}
		} else if event.ToolCallDelta.InputTextDelta != nil {
			return errors.New("protocol bridge: function tool delta has free-form input")
		} else {
			fragment := event.ToolCallDelta.ArgumentsDelta
			if *bufferedToolBytes+len(fragment) > streamio.MaxFrameBytes {
				return errors.New("protocol bridge: streamed tool arguments exceed the size limit")
			}
			state.args = append(state.args, fragment...)
			*bufferedToolBytes += len(fragment)
		}
	case event.ToolCallEnd != nil:
		state, exists := active[event.ToolCallEnd.Index]
		if !exists {
			return errors.New("protocol bridge: tool-call end has no matching start")
		}
		if state.kind != engine.ToolInvocationFreeform {
			if _, err := engine.ParseRequiredJSONObject(state.args); err != nil {
				return errors.New("protocol bridge: streamed tool arguments are not a complete JSON object")
			}
		}
		delete(active, event.ToolCallEnd.Index)
	}
	return nil
}

func normalizeStreamUsage(source engine.StreamUsage, from, to Protocol) (engine.StreamUsage, error) {
	if source.InputTokens < 0 || source.OutputTokens < 0 || source.CacheReadTokens < 0 || source.CacheWriteTokens < 0 {
		return engine.StreamUsage{}, errors.New("protocol bridge: stream usage must be non-negative")
	}
	cache, ok := streamCheckedAdd(source.CacheReadTokens, source.CacheWriteTokens)
	if !ok {
		return engine.StreamUsage{}, errors.New("protocol bridge: stream cache usage overflows")
	}
	if from != Anthropic && cache > source.InputTokens {
		return engine.StreamUsage{}, errors.New("protocol bridge: stream cache usage exceeds input usage")
	}
	result := source
	if from == Anthropic && to != Anthropic {
		total, ok := streamCheckedAdd3(result.InputTokens, result.CacheReadTokens, result.CacheWriteTokens)
		if !ok {
			return engine.StreamUsage{}, errors.New("protocol bridge: stream input usage overflows")
		}
		result.InputTokens = total
	}
	if from != Anthropic && to == Anthropic {
		result.InputTokens -= cache
	}
	if _, ok := streamCheckedAdd(result.InputTokens, result.OutputTokens); !ok {
		return engine.StreamUsage{}, errors.New("protocol bridge: stream total usage overflows")
	}
	return result, nil
}

func streamCheckedAdd(a, b int) (int, bool) {
	if a < 0 || b < 0 || a > int(^uint(0)>>1)-b {
		return 0, false
	}
	return a + b, true
}

func streamCheckedAdd3(a, b, c int) (int, bool) {
	ab, ok := streamCheckedAdd(a, b)
	if !ok {
		return 0, false
	}
	return streamCheckedAdd(ab, c)
}

func supportsBlockSignatures(p Protocol) bool {
	return p == Anthropic || p == Gemini || p == GeminiCodeAssist
}

func supportsToolSignatures(p Protocol) bool {
	return p == Gemini || p == GeminiCodeAssist
}

func supportsFreeform(p Protocol) bool { return p == OpenAIResponses }

func streamAdapter(p Protocol) format.StreamAdapter {
	switch p {
	case OpenAIChat, OpenAIResponses:
		return &openai.StreamAdapter{EmitMessageStart: true}
	case Anthropic:
		return &anthropic.StreamAdapter{EmitMessageStart: true, ExplicitBlocks: true}
	case Gemini:
		return &gemini.StreamAdapter{EmitMessageStart: true, PreserveMissingToolID: true}
	case GeminiCodeAssist:
		return &gemini.StreamAdapter{Wrapped: true, EmitMessageStart: true, PreserveMissingToolID: true}
	default:
		panic("streamAdapter called with invalid protocol")
	}
}

func emitStreamError(out chan<- engine.StreamEvent, err error) {
	out <- engine.StreamEvent{Error: streamError(err)}
}

func streamError(err error) *engine.StreamError {
	return &engine.StreamError{Code: -1, Message: err.Error()}
}

// observedStream inspects complete wire lines as the native parser reads them.
// Its buffer is capped at the adapters' shared frame limit.
type observedStream struct {
	protocol Protocol
	reader   io.Reader

	mu       sync.Mutex
	line     []byte
	complete bool
	err      error
	// Responses terminal objects repeat the complete output. IDs observed in
	// lifecycle events prove those terminal blocks were actually streamed.
	responseItems     map[string]observedResponseItem
	responseID        string
	model             string
	chatToolIDs       map[int]string
	chatToolNames     map[int]string
	chatToolIndexes   map[int]struct{}
	observedToolBytes int
	anthropicStart    bool
}

type observedResponseItem struct {
	kind, callID, name string
}

func newObservedStream(protocol Protocol, reader io.Reader) *observedStream {
	return &observedStream{protocol: protocol, reader: reader}
}

func (s *observedStream) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if n > 0 {
		s.observe(p[:n])
	}
	if err == io.EOF {
		s.finishLine()
	}
	return n, err
}

func (s *observedStream) observe(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			s.appendLine(data)
			return
		}
		s.appendLine(data[:i])
		s.observeLineLocked(bytes.TrimSuffix(s.line, []byte{'\r'}))
		s.line = s.line[:0]
		data = data[i+1:]
	}
}

func (s *observedStream) appendLine(data []byte) {
	if s.err != nil {
		return
	}
	if len(s.line)+len(data) > streamio.MaxFrameBytes {
		s.err = errors.New("protocol bridge: source stream frame exceeds the size limit")
		s.line = nil
		return
	}
	s.line = append(s.line, data...)
}

func (s *observedStream) finishLine() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.line) > 0 {
		s.observeLineLocked(bytes.TrimSuffix(s.line, []byte{'\r'}))
		s.line = nil
	}
}

func (s *observedStream) terminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.complete
}

func (s *observedStream) issue() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *observedStream) observeLineLocked(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || s.err != nil {
		return
	}
	payload := line
	if rest, ok := bytes.CutPrefix(payload, []byte("data:")); ok {
		payload = bytes.TrimSpace(rest)
	} else if payload[0] != '{' {
		return
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		if s.protocol == OpenAIChat {
			if s.complete {
				s.err = errors.New("protocol bridge: source stream has multiple terminal markers")
			} else {
				s.complete = true
			}
		}
		return
	}
	if s.complete {
		s.err = errors.New("protocol bridge: source data followed its terminal marker")
		return
	}

	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil {
		return // the native adapter owns malformed-JSON diagnostics
	}
	switch s.protocol {
	case OpenAIChat:
		if typ := rawString(root["type"]); strings.HasPrefix(typ, "response.") {
			s.err = unsupported("a Responses API stream on an OpenAI Chat bridge")
			return
		}
		s.observeIdentity(rawString(root["id"]), rawString(root["model"]))
		if raw, ok := root["usage"]; ok {
			if _, err := parseOpenAIUsage(raw, false); err != nil {
				s.err = errors.New("protocol bridge: invalid OpenAI Chat stream usage")
				return
			}
		}
		inspectOpenAIChat(root, s)
	case OpenAIResponses:
		typ := rawString(root["type"])
		if typ == "" {
			if _, choices := root["choices"]; choices {
				s.err = unsupported("a Chat Completions stream on an OpenAI Responses bridge")
			}
			return
		}
		switch typ {
		case "response.completed", "response.incomplete":
			inspectResponsesTerminal(typ, root["response"], s)
		case "response.failed":
			s.complete = true
			s.err = errors.New("protocol bridge: upstream OpenAI Responses stream failed")
		case "response.output_item.added":
			var item struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			_ = json.Unmarshal(root["item"], &item)
			if item.Status != "" && item.Status != "in_progress" {
				s.err = errors.New("protocol bridge: OpenAI Responses output item started in a terminal state")
			} else if item.Type != "" && item.Type != "message" && item.Type != "function_call" && item.Type != "custom_tool_call" {
				s.err = unsupported("provider-specific OpenAI Responses output blocks")
			} else if item.ID != "" {
				if s.responseItems == nil {
					s.responseItems = make(map[string]observedResponseItem)
				}
				if len(s.responseItems) >= maxStreamToolCalls {
					s.err = errors.New("protocol bridge: OpenAI Responses stream has too many output items")
				} else if _, duplicate := s.responseItems[item.ID]; duplicate {
					s.err = errors.New("protocol bridge: duplicate OpenAI Responses output item ID")
				} else {
					s.responseItems[item.ID] = observedResponseItem{kind: item.Type, callID: item.CallID, name: item.Name}
				}
			}
		case "response.created", "response.in_progress", "response.queued":
			var response struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			}
			_ = json.Unmarshal(root["response"], &response)
			s.observeIdentity(response.ID, response.Model)
		case "response.output_item.done":
			var item struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			_ = json.Unmarshal(root["item"], &item)
			streamed, ok := s.responseItems[item.ID]
			if !ok || streamed.kind != item.Type ||
				((item.Type == "function_call" || item.Type == "custom_tool_call") &&
					(streamed.callID != item.CallID || streamed.name != item.Name)) ||
				(item.Status != "" && item.Status != "completed") {
				s.err = errors.New("protocol bridge: OpenAI Responses output item changed during the stream")
			}
		case "response.content_part.added", "response.content_part.done":
			var part struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(root["part"], &part)
			if part.Type != "" && part.Type != "output_text" {
				s.err = unsupported("provider-specific OpenAI Responses content parts")
			}
		case "response.output_text.delta", "response.output_text.done",
			"response.function_call_arguments.delta", "response.function_call_arguments.done",
			"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
			itemID := rawString(root["item_id"])
			if itemID != "" {
				if _, ok := s.responseItems[itemID]; !ok {
					s.err = errors.New("protocol bridge: OpenAI Responses delta references an unknown output item")
				}
			}
		case
			"error":
		default:
			if strings.HasPrefix(typ, "response.") {
				s.err = unsupported("provider-specific OpenAI Responses output blocks")
			}
		}
	case Anthropic:
		typ := rawString(root["type"])
		if typ == "message_start" {
			if s.anthropicStart {
				s.err = errors.New("protocol bridge: multiple Anthropic message starts")
				return
			}
			s.anthropicStart = true
			var message struct {
				ID    string          `json:"id"`
				Model string          `json:"model"`
				Usage json.RawMessage `json:"usage"`
			}
			_ = json.Unmarshal(root["message"], &message)
			s.observeIdentity(message.ID, message.Model)
			if len(message.Usage) != 0 && !validPartialAnthropicStreamUsage(message.Usage) {
				s.err = errors.New("protocol bridge: invalid Anthropic stream usage")
				return
			}
		}
		if typ == "message_delta" {
			if raw, ok := root["usage"]; ok && !validPartialAnthropicStreamUsage(raw) {
				s.err = errors.New("protocol bridge: invalid Anthropic stream usage")
				return
			}
		}
		if typ == "message_stop" {
			s.complete = true
		}
		if typ == "content_block_start" {
			var block struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(root["content_block"], &block)
			if block.Type != "text" && block.Type != "thinking" && block.Type != "tool_use" {
				s.err = unsupported("provider-specific Anthropic content blocks")
			}
		}
		if typ == "content_block_delta" {
			var delta struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(root["delta"], &delta)
			switch delta.Type {
			case "text_delta", "thinking_delta", "signature_delta", "input_json_delta":
			default:
				s.err = unsupported("provider-specific Anthropic content deltas")
			}
		}
	case Gemini, GeminiCodeAssist:
		response, wrapped := root["response"]
		if s.protocol == GeminiCodeAssist && !wrapped {
			s.err = unsupported("an unwrapped Gemini stream on a Code Assist bridge")
			return
		}
		if s.protocol == Gemini && wrapped {
			s.err = unsupported("a Code Assist response envelope on a Gemini bridge")
			return
		}
		chunk := payload
		if wrapped {
			chunk = response
		}
		inspectGeminiChunk(chunk, s)
	}
}

func validPartialAnthropicStreamUsage(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	obj, err := rawObject(raw, "anthropic stream usage")
	if err != nil || rejectUnknown(obj, "anthropic stream usage", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens") != nil {
		return false
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		value, _, err := optionalInt(obj, key, "anthropic stream usage")
		if err != nil || value < 0 {
			return false
		}
	}
	return true
}

func (s *observedStream) observeIdentity(id, model string) {
	if id != "" {
		if s.responseID != "" && s.responseID != id {
			s.err = errors.New("protocol bridge: upstream response ID changed during the stream")
			return
		}
		s.responseID = id
	}
	if model != "" {
		if s.model != "" && s.model != model {
			s.err = errors.New("protocol bridge: upstream model changed during the stream")
			return
		}
		s.model = model
	}
}

func inspectResponsesTerminal(eventType string, raw json.RawMessage, observed *observedStream) {
	var response struct {
		ID     string            `json:"id"`
		Model  string            `json:"model"`
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
		Usage  json.RawMessage   `json:"usage"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return
	}
	wantStatus := "completed"
	if eventType == "response.incomplete" {
		wantStatus = "incomplete"
	}
	if response.Status != wantStatus {
		observed.err = errors.New("protocol bridge: OpenAI Responses terminal event has an inconsistent status")
		return
	}
	observed.observeIdentity(response.ID, response.Model)
	if observed.err != nil {
		return
	}
	if len(response.Usage) != 0 {
		if _, err := parseOpenAIUsage(response.Usage, true); err != nil {
			observed.err = errors.New("protocol bridge: invalid OpenAI Responses stream usage")
			return
		}
	}
	seen := make(map[string]struct{}, len(response.Output))
	for _, rawItem := range response.Output {
		var item struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Status  string `json:"status"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if json.Unmarshal(rawItem, &item) != nil {
			continue
		}
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				if content.Type != "output_text" {
					observed.err = unsupported("provider-specific OpenAI Responses output blocks")
					return
				}
			}
		case "function_call", "custom_tool_call":
		default:
			observed.err = unsupported("provider-specific OpenAI Responses output blocks")
			return
		}
		if item.Status != "" {
			validStatus := item.Status == "completed"
			if eventType == "response.incomplete" {
				validStatus = validStatus || item.Status == "incomplete"
			}
			if !validStatus {
				observed.err = errors.New("protocol bridge: OpenAI Responses terminal output item did not complete")
				return
			}
		}
		streamed, ok := observed.responseItems[item.ID]
		if item.ID == "" || !ok || streamed.kind != item.Type ||
			((item.Type == "function_call" || item.Type == "custom_tool_call") &&
				(streamed.callID != item.CallID || streamed.name != item.Name)) {
			observed.err = errors.New("protocol bridge: OpenAI Responses terminal output was not streamed")
			return
		}
		seen[item.ID] = struct{}{}
	}
	if len(seen) != len(observed.responseItems) {
		observed.err = errors.New("protocol bridge: OpenAI Responses terminal output differs from streamed output")
		return
	}
	observed.complete = true
}

func inspectOpenAIChat(root map[string]json.RawMessage, observed *observedStream) {
	var rawChoices []struct {
		Index int             `json:"index"`
		Delta json.RawMessage `json:"delta"`
	}
	if json.Unmarshal(root["choices"], &rawChoices) != nil {
		return
	}
	if len(rawChoices) > 1 {
		observed.err = unsupported("multiple streamed OpenAI Chat choices")
		return
	}
	for _, rawChoice := range rawChoices {
		if rawChoice.Index != 0 {
			observed.err = unsupported("a nonzero streamed OpenAI Chat choice index")
			return
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(rawChoice.Delta, &delta) != nil {
			continue
		}
		for key := range delta {
			switch key {
			case "role", "content", "reasoning_content", "tool_calls":
			default:
				observed.err = unsupported("provider-specific OpenAI Chat delta fields")
				return
			}
		}
		var calls []struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		_ = json.Unmarshal(delta["tool_calls"], &calls)
		for _, call := range calls {
			if call.Index < 0 {
				observed.err = errors.New("protocol bridge: invalid streamed tool-call index")
				return
			}
			if call.Type != "" && call.Type != "function" {
				observed.err = unsupported("non-function OpenAI Chat tool calls")
				return
			}
			if observed.chatToolIDs == nil {
				observed.chatToolIDs = make(map[int]string)
				observed.chatToolNames = make(map[int]string)
				observed.chatToolIndexes = make(map[int]struct{})
			}
			if _, known := observed.chatToolIndexes[call.Index]; !known && len(observed.chatToolIndexes) >= maxStreamToolCalls {
				observed.err = errors.New("protocol bridge: OpenAI Chat stream has too many tool calls")
				return
			}
			observed.chatToolIndexes[call.Index] = struct{}{}
			if observed.observedToolBytes+len(call.Function.Arguments) > streamio.MaxFrameBytes {
				observed.err = errors.New("protocol bridge: streamed tool arguments exceed the size limit")
				return
			}
			observed.observedToolBytes += len(call.Function.Arguments)
			if call.ID != "" {
				if prior := observed.chatToolIDs[call.Index]; prior != "" && prior != call.ID {
					observed.err = errors.New("protocol bridge: streamed tool-call ID changed")
					return
				}
				observed.chatToolIDs[call.Index] = call.ID
			}
			if call.Function.Name != "" {
				if prior := observed.chatToolNames[call.Index]; prior != "" && prior != call.Function.Name {
					observed.err = errors.New("protocol bridge: streamed tool-call name changed")
					return
				}
				observed.chatToolNames[call.Index] = call.Function.Name
			}
		}
	}
}

func inspectGeminiChunk(raw []byte, observed *observedStream) {
	var chunk struct {
		ResponseID   string          `json:"responseId"`
		ModelVersion string          `json:"modelVersion"`
		Usage        json.RawMessage `json:"usageMetadata"`
		Candidates   []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finishReason"`
			Content      *struct {
				Parts []map[string]json.RawMessage `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &chunk) != nil {
		return
	}
	observed.observeIdentity(chunk.ResponseID, chunk.ModelVersion)
	if len(chunk.Usage) != 0 {
		if _, err := parseGeminiUsage(chunk.Usage); err != nil {
			observed.err = errors.New("protocol bridge: invalid Gemini stream usage")
			return
		}
	}
	if len(chunk.Candidates) > 1 {
		observed.err = unsupported("multiple streamed candidates")
		return
	}
	for _, candidate := range chunk.Candidates {
		if candidate.Index != 0 {
			observed.err = unsupported("a nonzero streamed Gemini candidate index")
			return
		}
		if candidate.FinishReason != "" {
			observed.complete = true
		}
		if candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			for key := range part {
				switch key {
				case "text", "thought", "thoughtSignature", "functionCall":
				default:
					observed.err = unsupported("provider-specific Gemini response parts")
					return
				}
			}
		}
	}
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
