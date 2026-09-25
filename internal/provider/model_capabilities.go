package provider

import pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"

var portableEffort = map[string]pb.Effort{
	"minimal": pb.Effort_EFFORT_MINIMAL,
	"low":     pb.Effort_EFFORT_LOW,
	"medium":  pb.Effort_EFFORT_MEDIUM,
	"high":    pb.Effort_EFFORT_HIGH,
	"xhigh":   pb.Effort_EFFORT_XHIGH,
	"max":     pb.Effort_EFFORT_MAX,
}

// ModelCapabilities returns only an exact operator declaration. No provider
// discovery or wildcard pricing fallback occurs at this read-only boundary.
func (c Config) ModelCapabilities(providerName, model string) (*pb.ModelCapabilities, bool) {
	upstream, found := c.Providers[providerName]
	if !found {
		return nil, false
	}
	declaration, found := upstream.Models[model]
	if !found {
		return nil, false
	}
	answer := &pb.ModelCapabilities{Format: upstream.Format, ContextWindowTokens: declaration.ContextWindowTokens}
	if declaration.Effort != nil {
		for _, level := range declaration.Effort.Levels {
			answer.EffortLevels = append(answer.EffortLevels, portableEffort[level])
		}
	}
	if price := declaration.Pricing; price != nil {
		answer.Pricing = &pb.ModelPricing{
			InputUsdPerMtok: price.InputUSDPerMTok, OutputUsdPerMtok: price.OutputUSDPerMTok,
			CacheReadUsdPerMtok: price.CacheReadUSDPerMTok, CacheWriteUsdPerMtok: price.CacheWriteUSDPerMTok,
		}
	}
	return answer, true
}
