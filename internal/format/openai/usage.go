package openai

import "github.com/torana-edge/torana-edge/internal/engine"

// OpenAI reports token usage under two different sets of field names, and
// Torana reads it on two different code paths. That is how the non-streaming
// path came to understand only one of them: it read prompt_tokens /
// completion_tokens, so every non-streaming Responses API call was accounted
// at zero tokens and zero cost — silently, because a missing usage object is
// indistinguishable from a provider that did not report one.
//
// The field names live here and nowhere else. Both paths call ReadUsage, so a
// third shape (or a rename) is one edit, not a hunt for the callers that were
// missed.
//
//	Chat Completions   usage.prompt_tokens, usage.completion_tokens
//	                   usage.prompt_tokens_details.cached_tokens
//	                   usage.prompt_cache_hit_tokens        (DeepSeek)
//	Responses          usage.input_tokens, usage.output_tokens
//	                   usage.input_tokens_details.cached_tokens
//	                   usage.input_tokens_details.cache_write_tokens
//
// The two are disjoint in practice, so ReadUsage accepts either without being
// told which variant it is looking at.
func ReadUsage(usage map[string]any) *engine.StreamUsage {
	if usage == nil {
		return nil
	}

	u := &engine.StreamUsage{
		InputTokens:  firstInt(usage, "prompt_tokens", "input_tokens"),
		OutputTokens: firstInt(usage, "completion_tokens", "output_tokens"),
	}

	// Cache counts are nested, under a different key per variant.
	if d := nestedMap(usage, "prompt_tokens_details"); d != nil {
		u.CacheReadTokens = intAt(d, "cached_tokens")
	} else if d := nestedMap(usage, "input_tokens_details"); d != nil {
		u.CacheReadTokens = intAt(d, "cached_tokens")
		u.CacheWriteTokens = intAt(d, "cache_write_tokens")
	} else {
		// DeepSeek reports the hit count flat, with no details object.
		u.CacheReadTokens = intAt(usage, "prompt_cache_hit_tokens")
	}

	// An all-zero usage object carries no information, and returning it would
	// record a real response as a zero-token one.
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 {
		return nil
	}
	return u
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v := intAt(m, k); v != 0 {
			return v
		}
	}
	return 0
}

func nestedMap(m map[string]any, key string) map[string]any {
	nested, _ := m[key].(map[string]any)
	return nested
}

func intAt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64: // JSON numbers decode as float64
		return int(v)
	case int:
		return v
	}
	return 0
}
