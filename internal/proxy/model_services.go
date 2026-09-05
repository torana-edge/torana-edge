package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"

	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

// completeModel invokes one operator-bound service. The guest supplies only
// provider-neutral messages and sampling hints; destination, model,
// credentials, path, timeout, and spend ceilings come from the immutable
// resource snapshot installed on that exact plugin generation.
func (s *Server) completeModel(ctx context.Context, pluginName string, resource wasm.ModelServiceResource, args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError) {
	var inputBytes int64
	for _, message := range args.Messages {
		inputBytes += int64(len(message.Role) + len(message.Content))
		if inputBytes > resource.MaxInputBytes {
			return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "model request exceeds the approved input limit")
		}
	}
	maxTokens := resource.MaxTokens
	if args.MaxTokens != nil && *args.MaxTokens < maxTokens {
		maxTokens = *args.MaxTokens
	}
	if maxTokens == 0 || maxTokens > math.MaxInt32 {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "model service has an invalid token limit")
	}
	maxTokens32 := int32(maxTokens)
	request := &pbv1.ChatRequest{Model: resource.Model, MaxTokens: &maxTokens32, Temperature: args.Temperature}
	for _, message := range args.Messages {
		request.Messages = append(request.Messages, &pbv1.Message{Role: message.Role, Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: message.Content}}}}})
	}
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
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "model service provider returned an unreadable response")
	}
	prov, ok := s.GetConfig().Providers.Providers[resource.Provider]
	if !ok {
		return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "model service provider is unavailable")
	}
	refs := extractResponse(prov.Format, decoded, body)
	out := &pbv1.ModelCompleteResult{Content: refs.content, ReportedModel: refs.model, FinishReason: refs.finishReason}
	if refs.usage != nil {
		for _, count := range []int{refs.usage.InputTokens, refs.usage.OutputTokens, refs.usage.CacheReadTokens, refs.usage.CacheWriteTokens} {
			if count < 0 || int64(count) > math.MaxInt32 {
				return nil, modelHostError(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "model service provider returned invalid usage")
			}
		}
		out.Usage = &pbv1.Usage{InputTokens: int32(refs.usage.InputTokens), OutputTokens: int32(refs.usage.OutputTokens), CacheReadTokens: int32(refs.usage.CacheReadTokens), CacheWriteTokens: int32(refs.usage.CacheWriteTokens)}
	}
	return out, nil
}

func (s *Server) modelPricing(ctx context.Context, _ string, resource wasm.PricingResource) (*pbv1.ModelPricing, *pbv1.HostError) {
	var pricing *pbv1.ModelPricing
	if resource.ForModelService == "" {
		rs := reqStateFrom(ctx)
		if rs != nil {
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

func modelHostError(code pbv1.ErrorCode, message string) *pbv1.HostError {
	return &pbv1.HostError{Code: code, Message: message}
}

func ptr[T any](value T) *T { return &value }
