package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// offloadTimeout bounds a single cheap-model summarization call.
const offloadTimeout = 30 * time.Second

// offloadCompletion handles the torana_offload_completion host call: it POSTs
// the plugin-supplied prompts to the configured offload provider and returns
// the completion text.
//
// The provider, model, and credentials come from the live config
// (s.GetConfig().Providers.Offload) — never from map-iteration order.
// Auth precedence: dedicated key from offload.api_key_env, else the caller's
// own request credential (carried host-side in reqState; never exposed to
// plugins).
func (s *Server) offloadCompletion(ctx context.Context, payloadJSON string) (string, error) {
	var p struct {
		SystemPrompt string `json:"system_prompt"`
		UserPrompt   string `json:"user_prompt"`
		Model        string `json:"model"`
		// Provider optionally overrides the configured offload provider so a
		// plugin can direct its call to a specific endpoint (e.g. a
		// guaranteed-local model for PII scanning). Must exist in Providers.
		Provider string `json:"provider"`
		// APIKeyEnv optionally names the env var holding the key for that
		// provider (only consulted alongside a provider override).
		APIKeyEnv string `json:"api_key_env"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return "", fmt.Errorf("offload: parse payload: %w", err)
	}

	cfg := s.GetConfig().Providers
	off := cfg.Offload
	overrideProvider := p.Provider != ""
	if !off.Enabled && !overrideProvider {
		return "", fmt.Errorf("offload not configured — set offload.enabled, offload.provider, offload.model")
	}

	// Provider precedence: plugin payload overrides the configured offload provider.
	provName := off.Provider
	if overrideProvider {
		provName = p.Provider
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		// The default is validated at startup; an override names an arbitrary provider.
		return "", fmt.Errorf("offload: provider %q not found", provName)
	}

	// Model precedence: plugin payload overrides config. off.Model belongs to
	// the default provider, so an override must carry its own model.
	model := p.Model
	if model == "" {
		if overrideProvider {
			return "", fmt.Errorf("offload: model required when provider is overridden")
		}
		model = off.Model
	}

	// Auth: a payload key env wins, else the configured offload key. Fall back
	// to the caller's credential ONLY for the default provider — the caller's
	// key authenticates the primary provider, not a plugin-chosen endpoint, so
	// it is never forwarded to an overridden (e.g. local) provider.
	keyEnv := off.APIKeyEnv
	if p.APIKeyEnv != "" {
		keyEnv = p.APIKeyEnv
	}
	apiKey := s.resolveSecret(keyEnv, off.APIKeyEnc)
	if apiKey == "" && !overrideProvider {
		apiKey = reqStateFrom(ctx).CallerAuth
	}

	// max_tokens must cover BOTH reasoning and content: DeepSeek-style
	// reasoning models spend this budget on reasoning_content first, so a
	// tight cap (1024) leaves reasoning-heavy summarizations with an empty
	// content field and finish_reason "length". 4096 gives content room to
	// land after the reasoning; the offload still degrades gracefully if the
	// budget is somehow exhausted.
	reqBody, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": p.SystemPrompt},
			{"role": "user", "content": p.UserPrompt},
		},
		"stream":      false,
		"max_tokens":  4096,
		"temperature": 0,
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", prov.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: offloadTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("offload: upstream returned %d: %s", resp.StatusCode, string(respBytes[:min(len(respBytes), 200)]))
	}
	var result struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return "", fmt.Errorf("offload: parse response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("offload: no choices in response")
	}
	if result.Choices[0].Message.Content == "" {
		// Surface finish_reason so budget exhaustion (reasoning consumed the
		// whole max_tokens → finish_reason "length") is distinguishable from a
		// genuinely empty extraction in the logs/stats.
		return "", fmt.Errorf("offload: empty response (finish_reason=%q)", result.Choices[0].FinishReason)
	}
	return result.Choices[0].Message.Content, nil
}
