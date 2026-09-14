package provider

import (
	"fmt"
	"math"
	"strings"

	"github.com/torana-edge/torana-edge/internal/bridge"
)

// BridgeConfig opts this provider route into API contract translation. Format
// continues to describe the upstream family (including its authentication).
// Each named provider can expose a different client contract for one backend.
type BridgeConfig struct {
	Client    bridge.Protocol `json:"client"`
	Upstream  bridge.Protocol `json:"upstream"`
	Model     string          `json:"model,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Project   string          `json:"project,omitempty"`
}

func (p Provider) ValidateBridge(name string) error {
	b := p.Bridge
	if b == nil {
		return nil
	}
	if !b.Client.Valid() || !b.Upstream.Valid() {
		return fmt.Errorf("provider %q bridge.client and bridge.upstream must name supported inference protocols", name)
	}
	if p.Format != b.Upstream.Format() {
		return fmt.Errorf("provider %q format must match bridge.upstream family %q", name, b.Upstream.Format())
	}
	if b.Client.Format() != b.Upstream.Format() && p.Auth.EffectiveMode() == "caller" {
		return fmt.Errorf("provider %q cross-provider bridge requires auth.mode credential or none; client authentication is not an upstream credential", name)
	}
	if b.MaxTokens < 0 || b.MaxTokens > math.MaxInt32 {
		return fmt.Errorf("provider %q bridge.max_tokens must be between 1 and %d when set", name, math.MaxInt32)
	}
	if b.MaxTokens != 0 && b.Upstream != bridge.Anthropic {
		return fmt.Errorf("provider %q bridge.max_tokens is only an Anthropic missing-limit default", name)
	}
	if b.Project != "" && b.Upstream != bridge.GeminiCodeAssist {
		return fmt.Errorf("provider %q bridge.project requires a Code Assist upstream", name)
	}
	if b.Upstream == bridge.GeminiCodeAssist && strings.TrimSpace(b.Project) == "" && b.Client != bridge.GeminiCodeAssist {
		return fmt.Errorf("provider %q bridge.project is required for a Code Assist upstream", name)
	}
	if b.Upstream == bridge.Gemini && b.Model != "" {
		if _, _, err := bridge.Endpoint(b.Upstream, b.Model, false); err != nil {
			return fmt.Errorf("provider %q bridge.model is not a Gemini model identifier", name)
		}
	}
	return nil
}
