package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// offloadTimeout bounds a single cheap-model summarization call.
const offloadTimeout = 30 * time.Second

// maxOffloadResponseBytes bounds what is read back from the offload provider.
// A cheap-model summarization is a few KB; this is generous without allowing
// an unbounded read of a faulty endpoint's memory.
const maxOffloadResponseBytes = 1 << 20

// offloadCompletionResult answers the torana_offload_completion host call: it
// POSTs the plugin-supplied prompts to the configured offload provider and
// returns the classified outcome. The value arm carries the marshaled
// OffloadResult on success; refusals are framed classified HostErrors.
//
// The provider and model come from live config. Authentication is always the
// selected provider's explicit policy; host-originated calls cannot use caller
// mode because there is no caller credential authority to borrow.
//
// Classification — the caller and the configuration are different failure
// domains, and a plugin branches on them differently:
//
//   - INVALID_ARGUMENT: malformed/unknown payload members or an override
//     missing its model. Retrying the same call cannot help.
//   - NOT_CONFIGURED: offload disabled, an override naming a provider that
//     does not exist, or a configured provider URL that cannot even be built
//     into a request. The operator must change configuration.
//   - UNAVAILABLE: transport/read failures, an upstream non-200, an
//     unparseable response, no choices, or an empty completion. The call was
//     valid but could not currently be completed.
//   - INVALID_ARGUMENT also covers an oversized response: it is deterministic
//     (the same request hits the same limit and spends money again), so the
//     plugin must change the request rather than retry it unchanged.
//   - INTERNAL: a host-side invariant failed (the success envelope cannot be
//     marshaled). Not the guest's fault, not the operator's.
func (s *Server) offloadCompletionResult(ctx context.Context, payloadJSON string) wasm.ExtensionResult {
	var p struct {
		SystemPrompt string `json:"system_prompt"`
		UserPrompt   string `json:"user_prompt"`
		Model        string `json:"model"`
		// Provider optionally overrides the configured offload provider so a
		// plugin can direct its call to a specific endpoint (e.g. a
		// guaranteed-local model for PII scanning). Must exist in Providers.
		Provider string `json:"provider"`
	}
	decoder := json.NewDecoder(strings.NewReader(payloadJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offload: parse payload: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offload: payload must contain exactly one JSON value")
	}

	cfg := s.GetConfig().Providers
	off := cfg.Offload
	overrideProvider := p.Provider != ""
	if !off.Enabled && !overrideProvider {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
			"offload not configured — set offload.enabled, offload.provider, offload.model")
	}

	// Provider precedence: plugin payload overrides the configured offload provider.
	provName := off.Provider
	if overrideProvider {
		provName = p.Provider
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		// The default is validated at startup; an override names an arbitrary provider.
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "offload: provider %q not found", provName)
	}

	// Model precedence: plugin payload overrides config. off.Model belongs to
	// the default provider, so an override must carry its own model.
	model := p.Model
	if model == "" {
		if overrideProvider {
			return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
				"offload: model required when provider is overridden")
		}
		model = off.Model
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
	// The URL is operator configuration, never guest input, so a request that
	// cannot even be built is a configuration gap, not a caller bug.
	httpReq, err := http.NewRequestWithContext(ctx, "POST", prov.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
			"offload: cannot build request to %q: %v", prov.URL, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if prov.Auth.EffectiveMode() == "caller" {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "offload: provider %q uses caller auth; host-originated calls require credential or none", provName)
	}
	if err := applyProviderCredential(ctx, httpReq, prov, callerCredentials{}, s.resolveCredential); err != nil {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "offload: provider credential unavailable")
	}

	// The URL is operator configuration, so an origin parse failure is a
	// configuration gap; a cross-origin redirect would leak the credential
	// (Go strips Authorization but not X-Api-Key), so redirects are confined
	// to the configured origin and a 3xx elsewhere becomes the outcome.
	base, err := url.Parse(prov.URL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
			"offload: provider %q has an invalid URL %q", provName, prov.URL)
	}
	client := &http.Client{Timeout: offloadTimeout, CheckRedirect: redirectPolicy(base)}
	resp, err := client.Do(httpReq)
	if err != nil {
		if errors.Is(err, errTooManyRedirects) {
			// Deterministic: the operator must fix the configured endpoint.
			return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "offload: provider %q redirected in a loop: %v", provName, err)
		}
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "offload: %v", err)
	}
	defer resp.Body.Close()
	// limit+1 read so an oversized body is detected as an overflow instead of
	// being truncated into a partial completion.
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxOffloadResponseBytes+1))
	if err != nil {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "offload: read response: %v", err)
	}
	if len(respBytes) > maxOffloadResponseBytes {
		// Deterministic, not transient: the plugin must change the request.
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offload: response exceeds %d bytes — reduce the prompt or raise the limit", maxOffloadResponseBytes)
	}
	if resp.StatusCode != 200 {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
			"offload: upstream returned %d: %s", resp.StatusCode, string(respBytes[:min(len(respBytes), 200)]))
	}
	var result struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens          int64 `json:"prompt_tokens"`
			CompletionTokens      int64 `json:"completion_tokens"`
			PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
			PromptDetails         struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "offload: parse response: %v", err)
	}
	if len(result.Choices) == 0 {
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "offload: no choices in response")
	}
	if result.Choices[0].Message.Content == "" {
		// Surface finish_reason so budget exhaustion (reasoning consumed the
		// whole max_tokens → finish_reason "length") is distinguishable from a
		// genuinely empty extraction in the logs/stats.
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
			"offload: empty response (finish_reason=%q)", result.Choices[0].FinishReason)
	}
	usage := economics.Usage{InputIncludesCacheRead: true}
	if result.Usage != nil {
		usage.Reported = true
		usage.InputTokens = result.Usage.PromptTokens
		usage.OutputTokens = result.Usage.CompletionTokens
		usage.CacheReadTokens = result.Usage.PromptDetails.CachedTokens
		if usage.CacheReadTokens == 0 {
			usage.CacheReadTokens = result.Usage.PromptCacheHitTokens
		}
		usage.CacheWriteTokens = result.Usage.PromptDetails.CacheWriteTokens
	}
	s.stats.RecordOffloadUsage(usage)
	payload, err := json.Marshal(economics.OffloadResult{
		Completion: result.Choices[0].Message.Content,
		Provider:   provName,
		Model:      model,
		Usage:      usage,
	})
	if err != nil {
		// OffloadResult is host-built from strings and a numeric Usage struct;
		// reaching here is a host invariant, not a guest input.
		return wasm.ExtensionRefusal(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "offload: encode result: %v", err)
	}
	return wasm.ExtensionValue(payload)
}
