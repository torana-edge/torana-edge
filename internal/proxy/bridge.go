package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/provider"
)

type bridgeContextKey struct{}

// An exchange pins both contracts for this attempt. Retry requests get their
// own copy; a response is decoded according to the attempt that produced it.
type bridgeExchange struct {
	Client          bridge.Protocol
	Upstream        bridge.Protocol
	ClientRequest   *engine.ChatRequest
	AcceptedRequest *engine.ChatRequest
}

func exchangeFrom(ctx context.Context) *bridgeExchange {
	e, _ := ctx.Value(bridgeContextKey{}).(*bridgeExchange)
	return e
}

func bridgeResponseError(client bridge.Protocol, status int, err error) *BlockResponse {
	code, message := "api_error", "the upstream response could not be translated"
	if status == http.StatusBadRequest {
		code, message = "invalid_request_error", "the request cannot be represented by the selected upstream protocol"
		var unsupported *bridge.UnsupportedError
		if errors.As(err, &unsupported) {
			message = unsupported.Error()
		}
	}
	if client == bridge.Gemini || client == bridge.GeminiCodeAssist {
		code = "INTERNAL"
		if status == http.StatusBadRequest {
			code = "INVALID_ARGUMENT"
		}
	}
	return &BlockResponse{Status: status, ContentType: "application/json", Body: renderProviderError(client.Format(), status, code, message)}
}

func rejectBridgeRequest(req *http.Request, client bridge.Protocol, err error) {
	rc, _ := req.Context().Value(routeContextKey{}).(*RouteContext)
	if rc == nil {
		return
	}
	rc.Block = bridgeResponseError(client, http.StatusBadRequest, err)
	if rs := reqStateFrom(req.Context()); rs != nil {
		rs.Synthetic = true
		rs.Verdict = "invalid_request"
		rs.AuditErrorCode = "unsupported_protocol_feature"
		discardCompactionReports(rs)
	}
	req.Body = http.NoBody
	req.ContentLength = 0
}

func setBridgeResponse(resp *http.Response, result *BlockResponse) {
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	resp.StatusCode = result.Status
	resp.Status = ""
	resp.Header = make(http.Header)
	resp.Header.Set("Content-Type", result.ContentType)
	resp.Body = io.NopCloser(bytes.NewReader(result.Body))
	resp.ContentLength = int64(len(result.Body))
}

// A contract bridge builds transport semantics for the selected upstream.
// Client beta/version, SDK, cache and other provider headers do not cross APIs.
// Credentials are applied separately from the immutable ingress snapshot.
func prepareBridgeTransport(req *http.Request, p provider.Provider, protocol bridge.Protocol, chat *engine.ChatRequest) error {
	endpoint, query, err := bridge.Endpoint(protocol, chat.Model, chat.Stream)
	if err != nil {
		return err
	}
	target, err := url.Parse(p.URL)
	if err != nil || target.Host == "" {
		return fmt.Errorf("invalid bridge upstream URL")
	}
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.Host = target.Host
	req.URL.Path = joinURLPath(target.Path, endpoint)
	req.URL.RawPath = ""
	req.URL.RawQuery = query
	req.Header = make(http.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if chat.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if protocol == bridge.Anthropic {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	return nil
}

func bridgeOptions(p provider.Provider) bridge.RequestOptions {
	if p.Bridge == nil {
		return bridge.RequestOptions{}
	}
	return bridge.RequestOptions{MaxTokens: p.Bridge.MaxTokens, Project: p.Bridge.Project}
}

func bridgeTargetProtocol(p provider.Provider, client, current bridge.Protocol) (bridge.Protocol, bool) {
	if p.Bridge != nil {
		return p.Bridge.Upstream, p.Bridge.Client == client && p.Bridge.Upstream.Valid()
	}
	// A native route in the same API family can serve the identical endpoint.
	// Crossing to another family requires an explicit upstream contract.
	return current, p.Format == current.Format()
}

func clientErrorFormat(ctx context.Context, fallback string) string {
	if e := exchangeFrom(ctx); e != nil {
		return e.Client.Format()
	}
	return fallback
}

func bridgeUpstreamError(resp *http.Response, e *bridgeExchange) {
	status := resp.StatusCode
	code := "api_error"
	switch {
	case status == 400 || status == 422:
		code = "invalid_request_error"
	case status == 401:
		code = "authentication_error"
	case status == 403:
		code = "permission_error"
	case status == 404:
		code = "not_found_error"
	case status == 429:
		code = "rate_limit_error"
	}
	if e.Client == bridge.Gemini || e.Client == bridge.GeminiCodeAssist {
		code = map[int]string{400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED", 404: "NOT_FOUND", 429: "RESOURCE_EXHAUSTED"}[status]
		if code == "" {
			code = "INTERNAL"
		}
	}
	retryAfter := resp.Header.Get("Retry-After")
	result := &BlockResponse{Status: status, ContentType: "application/json", Body: renderProviderError(e.Client.Format(), status, code, "the upstream provider returned "+http.StatusText(status))}
	setBridgeResponse(resp, result)
	if retryAfter != "" {
		resp.Header.Set("Retry-After", retryAfter)
	}
}

func actualResponseFormat(ctx context.Context, fallback *format.Format) *format.Format {
	if e := exchangeFrom(ctx); e != nil {
		return format.Lookup(e.Upstream.Format())
	}
	return fallback
}

// prepareBridgeRetry translates the already approved request for one fallback.
// Plugins are not run a second time, and each attempt keeps its own contracts.
func prepareBridgeRetry(req *http.Request, target provider.Provider) (bool, error) {
	exchange := exchangeFrom(req.Context())
	if exchange == nil {
		return false, nil
	}
	to, ok := bridgeTargetProtocol(target, exchange.Client, exchange.Upstream)
	if !ok || (exchange.Client.Format() != target.Format && target.Auth.EffectiveMode() == "caller") {
		return true, &bridge.UnsupportedError{Feature: "fallback protocol or credential policy"}
	}
	chat := exchange.AcceptedRequest
	if chat == nil {
		return true, fmt.Errorf("missing accepted bridge request")
	}
	candidate := *chat
	if target.Bridge != nil && target.Bridge.Model != "" {
		candidate.Model = target.Bridge.Model
	}
	projected, err := bridge.ProjectRequest(&candidate, exchange.Client, to, bridgeOptions(target))
	if err != nil {
		return true, err
	}
	if to == bridge.OpenAIResponses {
		applyOpenAIResponsesCompaction(projected, target)
	}
	if err = prepareBridgeUsage(projected, to); err != nil {
		return true, err
	}
	raw, err := bridge.MarshalRequest(to, projected)
	if err != nil {
		return true, err
	}
	if err = prepareBridgeTransport(req, target, to, projected); err != nil {
		return true, err
	}
	next := *exchange
	next.Upstream = to
	ctx := context.WithValue(req.Context(), bridgeContextKey{}, &next)
	ctx = context.WithValue(ctx, engine.ChatRequestKey, projected)
	*req = *req.WithContext(ctx)
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	return true, nil
}

// requestCachePrefixKey fingerprints the checked request in the exact topology
// that will be marshaled for an upstream attempt. Keeping this in one helper is
// what lets a fallback replace the primary's provider-side cache identity.
func requestCachePrefixKey(chat *engine.ChatRequest) string {
	if chat == nil {
		return ""
	}
	pbReq, err := pbconv.ToPBChatRequestChecked(chat)
	if err != nil {
		return ""
	}
	return engine.CachePrefixKeyTopology(pbReq, engine.TopologyFacts{
		CodeAssist:            chat.CodeAssist,
		OpenAIVariant:         chat.OpenAIVariant,
		ResponsesInputLayout:  chat.ResponsesInputLayout,
		ResponsesInstructions: chat.ResponsesInstructions,
	})
}

func recordAttemptState(rs *reqState, formatName, path string, chat *engine.ChatRequest) {
	if rs == nil {
		return
	}
	rs.ActualFormat = formatName
	rs.ActualPath = path
	rs.CachePrefixKey = requestCachePrefixKey(chat)
}

func prepareBridgeUsage(chat *engine.ChatRequest, protocol bridge.Protocol) error {
	if protocol != bridge.OpenAIChat || !chat.Stream {
		return nil
	}
	var err error
	chat.ProviderExtensions, err = chat.ProviderExtensions.SetMember("stream_options", []byte(`{"include_usage":true}`))
	return err
}
