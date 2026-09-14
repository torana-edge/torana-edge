package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/strictjson"
	"google.golang.org/protobuf/proto"
)

// completeModel invokes one operator-bound service. The guest supplies only
// provider-neutral messages and sampling hints; destination, model,
// credentials, path, timeout, and spend ceilings come from the immutable
// resource snapshot installed on that exact plugin generation.
func (s *Server) completeModel(ctx context.Context, pluginName string, resource wasm.ModelServiceResource, args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError) {
	if args == nil || args.Validate() != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "model request is outside the supported domain")
	}
	// Bound the complete canonical payload, including tool schemas and output
	// constraints. Text-only accounting would leave the new surfaces unbounded.
	if int64(proto.Size(args)) > resource.MaxInputBytes {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "model request exceeds the approved input limit")
	}
	maxTokens := resource.MaxTokens
	if args.MaxTokens != nil && *args.MaxTokens < maxTokens {
		maxTokens = *args.MaxTokens
	}
	if maxTokens == 0 || maxTokens > math.MaxInt32 {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "model service has an invalid token limit")
	}
	maxTokens32 := int32(maxTokens)
	request := proto.Clone(&pbv1.ChatRequest{Model: resource.Model, MaxTokens: &maxTokens32, Temperature: args.Temperature, Messages: args.Messages, Tools: args.Tools, OutputFormat: args.OutputFormat}).(*pbv1.ChatRequest)
	if err := request.ValidateReplacement(); err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "model request is outside the supported domain")
	}
	raw, err := proto.Marshal(request)
	if err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "model request could not be encoded")
	}
	payload, err := json.Marshal(egressRequest{Provider: resource.Provider, RequestPB: ptr(base64.StdEncoding.EncodeToString(raw)), Path: resource.Path, TimeoutMS: int(resource.Timeout / 1e6)})
	if err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "model request envelope could not be encoded")
	}
	budget := provider.EgressBudget{MaxCallsPerMinute: resource.MaxCallsPerMinute, MaxTokensPerHour: resource.MaxTokensPerHour}
	result := s.sendPluginRequestWithBudget(ctx, pluginName, string(payload), &budget, pluginName+"\x00model-service\x00"+resource.Name)
	if result.Refusal() != nil {
		return nil, proto.Clone(result.Refusal()).(*pbv1.HostError)
	}
	if result.Value() == nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "model service returned no result")
	}
	var envelope egressResponse
	if err := json.Unmarshal(result.Value(), &envelope); err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "model service returned an invalid envelope")
	}
	if envelope.HTTPStatus < 200 || envelope.HTTPStatus >= 300 {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service provider refused the request")
	}
	body, err := base64.StdEncoding.DecodeString(envelope.Body)
	if err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "model service returned an invalid body")
	}
	// Provider model results become a new typed value rather than being
	// forwarded byte-for-byte. Reject duplicate keys and parser differentials
	// before encoding/json can collapse them into an apparently valid result.
	if object, err := strictjson.DecodeObject(body); err != nil || object == nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service provider returned an unreadable response")
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service provider returned an unreadable response")
	}
	prov, ok := s.GetConfig().Providers.Providers[resource.Provider]
	if !ok {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "model service provider is unavailable")
	}
	if err := validateModelServiceResultDomain(prov.Format, decoded); err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service provider returned an unsupported response")
	}
	refs := extractResponse(prov.Format, decoded, body)
	if !refs.hasMessage {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service returned no assistant message")
	}
	response := pbconv.ToPBChatResponse(&engine.ChatResponse{Message: refs.assistantMessage()})
	if response.Message == nil || len(response.Message.Blocks) == 0 {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service returned no representable assistant output")
	}
	out := &pbv1.ModelCompleteResult{Message: response.Message, ReportedModel: refs.model, FinishReason: refs.finishReason}
	if err := out.Validate(); err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service provider returned an invalid result")
	}
	// Provider metering defects do not invalidate the completion. Use the same
	// validity rule as the token budget and leave unreliable usage unknown.
	if validProviderUsage(prov.Format, refs.usage) {
		// OpenAI/Gemini include cache reads in their input total; Anthropic
		// reports them separately. Normalize only this known overlap. Cache
		// writes have no established overlap on the non-Anthropic formats.
		input := int64(refs.usage.InputTokens)
		if prov.Format != "anthropic" {
			input -= int64(refs.usage.CacheReadTokens)
		}
		out.Usage = &pbv1.Usage{InputTokens: int32(input), OutputTokens: int32(refs.usage.OutputTokens), CacheReadTokens: int32(refs.usage.CacheReadTokens), CacheWriteTokens: int32(refs.usage.CacheWriteTokens)}
	}
	return out, nil
}

func (s *Server) modelPricing(ctx context.Context, _ string, resource wasm.PricingResource) (*pbv1.ModelPricing, *pbv1.HostError) {
	var pricing *pbv1.ModelPricing
	if resource.ForModelService == "" {
		if providerName, model, ok := currentRouteCoordinate(ctx); ok {
			pricing = resource.Prices[wasm.PricingCoordinate(providerName, model)]
		}
	} else if len(resource.Prices) == 1 {
		for _, bound := range resource.Prices {
			pricing = bound
		}
	}
	if pricing == nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "pricing is not configured for this model")
	}
	return proto.Clone(pricing).(*pbv1.ModelPricing), nil
}

func (s *Server) promptCachePolicy(ctx context.Context, _ string, resource wasm.PromptCacheResource) (*pbv1.PromptCachePolicy, *pbv1.HostError) {
	var policy *pbv1.PromptCachePolicy
	if providerName, model, ok := currentRouteCoordinate(ctx); ok {
		policy = resource.Policies[wasm.PricingCoordinate(providerName, model)]
	} else if len(resource.Policies) == 1 {
		// Background hooks have no routed request. A single binding is
		// unambiguous; multiple bindings must never be selected by map order.
		for _, bound := range resource.Policies {
			policy = bound
		}
	}
	if policy == nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "prompt cache policy is not configured for this model")
	}
	return proto.Clone(policy).(*pbv1.PromptCachePolicy), nil
}

func currentRouteCoordinate(ctx context.Context) (string, string, bool) {
	rs := reqStateFrom(ctx)
	if rs == nil {
		return "", "", false
	}
	providerName, model := rs.Provider, rs.Model
	route := rs.PendingRoute
	if rs.Pipeline != nil {
		if current := rs.Pipeline.Verdicts(rs.ID).Route(); current != nil {
			route = current
		}
	}
	if route != nil {
		if route.Provider != "" {
			providerName = route.Provider
		} else if rs.InitialProvider != "" {
			providerName = rs.InitialProvider
		}
		if route.Model != "" {
			model = route.Model
		}
	}
	if providerName == "" || model == "" {
		return "", "", false
	}
	return providerName, model, true
}

func modelHostError(code pbv1.ErrorCode, message string) *pbv1.HostError {
	return &pbv1.HostError{Code: code, Message: message}
}

func ptr[T any](value T) *T { return &value }
