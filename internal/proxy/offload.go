package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	}
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return "", fmt.Errorf("offload: parse payload: %w", err)
	}

	cfg := s.GetConfig().Providers
	off := cfg.Offload
	if !off.Enabled {
		return "", fmt.Errorf("offload not configured — set offload.enabled, offload.provider, offload.model")
	}
	prov, ok := cfg.Providers[off.Provider]
	if !ok {
		// Validated at startup; can only happen after a bad hot-reload.
		return "", fmt.Errorf("offload: provider %q not found", off.Provider)
	}

	// Model precedence: plugin payload overrides config.
	model := p.Model
	if model == "" {
		model = off.Model
	}

	// Auth precedence: dedicated key env, else the caller's credential.
	apiKey := ""
	if off.APIKeyEnv != "" {
		apiKey = os.Getenv(off.APIKeyEnv)
	}
	if apiKey == "" {
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
