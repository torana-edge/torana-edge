package proxy

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// validateRouteHost refuses mistakes that can be checked when the plugin
// issues its verdict. The final transport still rechecks mutable credentials
// and projection after every plugin has finished.
func (s *Server) validateRouteHost(ctx context.Context, args *pb.RouteRequestArgs) *pb.HostError {
	rs := reqStateFrom(ctx)
	if rs == nil || args == nil {
		return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: "route context is unavailable"}
	}
	if args.Provider == "" || args.Provider == rs.Provider {
		return nil
	}
	config := s.GetConfig().Providers
	target, found := config.Providers[args.Provider]
	if !found {
		return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "unknown provider"}
	}
	scope, ok := ctx.Value(syntheticResponseScopeKey{}).(syntheticResponseScope)
	if !ok || scope.request == nil || scope.format == nil {
		return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: "route request is unavailable"}
	}
	if isOpenAIResponsesRequest(scope.request) && hasProviderSideResponsesHistory(scope.request) {
		return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_UNSUPPORTED, Message: "server_state: provider-side Responses history cannot move providers"}
	}
	if exchange := exchangeFrom(ctx); exchange != nil {
		upstream, supported := bridgeTargetProtocol(target, exchange.Client, exchange.Upstream)
		if !supported {
			return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_UNSUPPORTED, Message: "bridge_unrepresentable: target protocol unavailable"}
		}
		if exchange.Client.Format() != target.Format && target.Auth.EffectiveMode() == "caller" {
			return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_PERMISSION_DENIED, Message: "credential_policy: target requires a credential policy"}
		}
		candidate := *scope.request
		if args.Model != "" {
			candidate.Model = args.Model
		} else if target.Bridge != nil && target.Bridge.Model != "" {
			candidate.Model = target.Bridge.Model
		}
		if _, err := bridge.ProjectRequest(&candidate, exchange.Client, upstream, bridgeOptions(target)); err != nil {
			return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_UNSUPPORTED, Message: "bridge_unrepresentable: request cannot be projected"}
		}
	} else if target.Format != rs.InitialFormat || target.Bridge != nil {
		return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_UNSUPPORTED, Message: "format_mismatch: target needs an explicit bridge"}
	}
	targetURL, err := url.Parse(target.URL)
	if err != nil || targetURL.Host == "" {
		return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, Message: "invalid_target_url: provider URL is invalid"}
	}
	return nil
}

// applyRoute validates and applies a plugin routing verdict: rewrite the
// upstream URL to the target provider, swap credentials, and override the
// model. Every violation fails OPEN to the original route (log + keep going)
// — a bad verdict must not take the request down.
//
// The target provider's explicit auth policy is applied after routing.
func (s *Server) applyRoute(req *http.Request, chat *engine.ChatRequest, origFormat, origName string, v *wasm.RouteVerdict, cfg provider.Config) bool {
	refuse := func(reason string) bool {
		if rs := reqStateFrom(req.Context()); rs != nil {
			rs.RouteRefused = reason
			rs.RouteProvider = rs.Provider
			rs.RouteModel = chat.Model
		}
		return false
	}
	if rs := reqStateFrom(req.Context()); rs != nil {
		rs.RouteAttempted = true
		rs.RoutePlugin = v.Plugin
		rs.RouteRefused = ""
	}
	if v.Provider == "" || v.Provider == origName {
		// Model-only override (or no-op): there is no provider to validate,
		// so the model stands on its own.
		if v.Model != "" || v.Effort != pb.Effort_EFFORT_UNSPECIFIED {
			if v.Model != "" {
				chat.Model = v.Model
				if origFormat == "gemini" {
					req.URL.Path = rewriteGeminiModelPath(req.URL.Path, chat.Model)
					req.URL.RawPath = ""
				}
			}
			if rs := reqStateFrom(req.Context()); rs != nil {
				rs.RouteProvider = rs.Provider
				rs.RouteModel = chat.Model
			}
			return true
		}
		if rs := reqStateFrom(req.Context()); rs != nil {
			rs.RouteAttempted = false
		}
		return false
	}

	target, ok := cfg.Providers[v.Provider]
	if !ok {
		log.Printf("[route] %s routed to unknown provider %q — keeping %q", v.Plugin, v.Provider, origName)
		return refuse("unknown_provider")
	}
	if isOpenAIResponsesRequest(chat) && hasProviderSideResponsesHistory(chat) {
		return refuse("server_state")
	}
	exchange := exchangeFrom(req.Context())
	var upstreamProtocol bridge.Protocol
	routedModel := v.Model
	if exchange != nil {
		var supported bool
		upstreamProtocol, supported = bridgeTargetProtocol(target, exchange.Client, exchange.Upstream)
		if !supported {
			log.Printf("[route] keeping original bridge: target protocol is unsupported")
			return refuse("bridge_unrepresentable")
		}
		if exchange.Client.Format() != target.Format && target.Auth.EffectiveMode() == "caller" {
			log.Printf("[route] keeping original bridge: target protocol or credential policy is incompatible")
			return refuse("credential_policy")
		}
		candidate := *chat
		if routedModel == "" && target.Bridge != nil {
			routedModel = target.Bridge.Model
		}
		if routedModel != "" {
			candidate.Model = routedModel
		}
		if _, err := bridge.ProjectRequest(&candidate, exchange.Client, upstreamProtocol, bridgeOptions(target)); err != nil {
			log.Printf("[route] keeping original bridge: target cannot represent request features")
			return refuse("bridge_unrepresentable")
		}
	} else if target.Format != origFormat || target.Bridge != nil {
		log.Printf("[route] provider %q format %q != %q — cross-format routing unsupported, keeping %q",
			v.Provider, target.Format, origFormat, origName)
		return refuse("format_mismatch")
	}
	turl, err := url.Parse(target.URL)
	if err != nil {
		log.Printf("[route] provider %q has invalid URL: %v — keeping %q", v.Provider, err, origName)
		return refuse("invalid_target_url")
	}

	rc, _ := req.Context().Value(routeContextKey{}).(*RouteContext)
	if rc == nil {
		return refuse("missing_route_context")
	}
	authCandidate := req.Clone(req.Context())
	authCandidate.Header = req.Header.Clone()
	caller := callerCredentials{}
	if rs := reqStateFrom(req.Context()); rs != nil {
		caller = rs.CallerCredentials
	}
	if err := applyProviderCredential(req.Context(), authCandidate, target, caller, s.resolveCredential); err != nil {
		log.Printf("[route] provider %q credential unavailable — keeping %q", v.Provider, origName)
		return refuse("credential_policy")
	}
	req.Header = authCandidate.Header
	req.URL.RawQuery = authCandidate.URL.RawQuery

	// Only now, with every check passed. A verdict is ONE decision: "send this
	// to provider X as model Y". Applying the model up front meant a verdict
	// rejected for an unknown provider, a format mismatch, an unparseable URL
	// or a missing credential still changed it — so the request went to the
	// ORIGINAL provider carrying a model chosen for a different one, which
	// that provider does not serve. Failing open has to mean the original
	// route, not half of a route nobody asked for.
	if routedModel != "" {
		chat.Model = routedModel
	}
	if exchange != nil {
		exchange.Upstream = upstreamProtocol
	}

	req.URL.Scheme = turl.Scheme
	req.URL.Host = turl.Host
	req.Host = turl.Host
	path := rc.StrippedPath
	if target.Format == "gemini" && routedModel != "" {
		path = rewriteGeminiModelPath(path, routedModel)
	}
	req.URL.Path = joinURLPath(turl.Path, path)
	req.URL.RawPath = ""
	// Failover fallbacks and metrics now follow the target.
	rc.ProviderName = v.Provider
	if rs := reqStateFrom(req.Context()); rs != nil {
		rs.Provider = v.Provider
		rs.RouteProvider = v.Provider
		rs.RouteModel = chat.Model
	}

	metrics.RecordRoutedRequest(req.Context(), origName, v.Provider)
	log.Printf("[route] %s → %s (model %q)", origName, v.Provider, chat.Model)
	return true
}

func hasProviderSideResponsesHistory(chat *engine.ChatRequest) bool {
	if chat == nil || chat.ProviderExtensions.IsAbsent() {
		return false
	}
	fields, _, err := chat.ProviderExtensions.DecodeObject()
	if err != nil {
		return false
	}
	var id string
	return json.Unmarshal(fields["previous_response_id"], &id) == nil && id != ""
}

func rewriteGeminiModelPath(path, model string) string {
	start := strings.Index(path, "/models/")
	if start < 0 {
		return path
	}
	start += len("/models/")
	end := strings.IndexAny(path[start:], "/:")
	if end < 0 {
		return path
	}
	return path[:start] + strings.TrimPrefix(model, "models/") + path[start+end:]
}
