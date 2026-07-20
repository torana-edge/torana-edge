// The compactor shrinks large tool results by delegating extraction to a
// cheap model (torana_offload_completion), guided by the intent captured by
// the intent plugin: "given what the agent was trying to find out, keep only
// the relevant parts of this output". Compacted results are cached by
// tool_call_id, so later turns replaying the same result reuse the compact
// form for free. Cache identity includes the original content, tool arguments,
// intent, and policy version so reused call IDs cannot return stale summaries.
//
// Run it AFTER the intent plugin. It is an alternative to keyword_compactor
// (deterministic, local, no model call) — pick ONE per deployment; both
// consume the same intent cache.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
	"google.golang.org/protobuf/proto"
)

func main() {}

const (
	intentCacheKey        = "intent"
	compactionCache       = "compacted"
	policyCompactionCache = "policy_compacted"
	minOffloadChars       = 2000
)

// maxOffloadInputChars caps how much of a tool output is sent to the cheap
// summarizer. 0 (the default) means UNBOUNDED — the complete tool output is
// sent. A positive value is opt-in via plugins.config.compactor and truncates
// head+tail to that many chars. Loaded once, lazily, from the plugin config.
var (
	cfgOnce              sync.Once
	maxOffloadInputChars int
	toolPolicies         []sdk.ToolPolicyRule
	expectedApplications int64
)

func loadConfig() {
	cfgOnce.Do(func() {
		var c struct {
			MaxOffloadInputChars int                  `json:"max_offload_input_chars"`
			ToolPolicies         []sdk.ToolPolicyRule `json:"tool_policies"`
			ExpectedApplications int64                `json:"expected_applications"`
		}
		if raw := sdk.PluginConfig(); raw != "" {
			_ = json.Unmarshal([]byte(raw), &c)
		}
		if c.MaxOffloadInputChars > 0 {
			maxOffloadInputChars = c.MaxOffloadInputChars
		}
		toolPolicies = c.ToolPolicies
		expectedApplications = c.ExpectedApplications
	})
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if !compactToolResults(ctx, req) {
			return nil, nil
		}
		return req, nil
	})
}

// ==========================================================================
// Tool result compaction
// ==========================================================================

func compactToolResults(ctx context.Context, req *pb.ChatRequest) bool {
	loadConfig()
	modified := false
	var modelWorks []modelWork
	assistantAfter := assistantMessageCountsAfter(req.Messages)
	toolNames := sdk.ToolNamesByCallID(req.Messages)
	toolCalls := sdk.ToolCallsByID(req.Messages)

	for i, msg := range req.Messages {
		if msg.Role != "tool" || msg.ToolCallId == "" || len(msg.Content) < minOffloadChars || sdk.IsDeterministicToolReplacement(msg.Content) {
			continue
		}

		toolName := msg.ToolName
		if toolName == "" {
			toolName = toolNames[msg.ToolCallId]
		}
		toolArgs := ""
		if call := toolCalls[msg.ToolCallId]; call != nil {
			toolArgs = string(call.ArgumentsJson)
		}
		if toolName == "" || sdk.ToolResultMustStayExact(toolName, msg.Content) {
			continue
		}
		rule, matched := sdk.MatchToolPolicy(toolPolicies, toolName)
		if !matched || rule.Mode == "" || rule.Mode == "exact" {
			continue
		}

		// Phase 0 observability: every result big enough to compact.
		sdk.EmitMetric("torana_compact_eligible_total", sdk.MetricCounter, 1, map[string]string{"tool": toolName})

		switch rule.Mode {
		case "deterministic":
			if assistantAfter[i] == 0 && !rule.FirstPass {
				continue
			}
			if applyDeterministicPolicy(msg, toolName, toolArgs, rule, false) {
				modified = true
			}
			continue
		case "source":
			if assistantAfter[i] < 3 {
				continue
			}
			if applyDeterministicPolicy(msg, toolName, toolArgs, rule, true) {
				modified = true
			}
			continue
		case "model":
			// A model summary is never allowed before one exact consumption.
			if assistantAfter[i] == 0 {
				continue
			}
		default:
			continue
		}

		// Get intent from cache (written by the intent plugin).
		intent, _ := sdk.HostCall("env.cache_get", intentCacheKey+":"+msg.ToolCallId)
		if intent == "" {
			// Eligible but no captured intent — the money left on the table.
			sdk.EmitMetric("torana_intent_missing_total", sdk.MetricCounter, 1, map[string]string{"tool": toolName})
			continue
		}

		// Include every semantic input in the cache identity. A harness may
		// reuse tool_call_ids, and intents can change across rehydrated rounds.
		modelCacheKey := sdk.ContentAddressedCacheKey(compactionCache,
			"v3", toolName, toolArgs, msg.Content, intent, "model")
		cached, _ := sdk.HostCall("env.cache_get", modelCacheKey)
		if cached == "" || len(cached) < len(msg.Content) {
			modelWorks = append(modelWorks, modelWork{
				message: msg, index: i, intent: intent, cacheKey: modelCacheKey, cached: cached,
			})
		}
	}
	if prepareAndApplyModelBatch(req, modelWorks) {
		modified = true
	}
	return modified
}

type tokenUsage struct {
	Reported               bool  `json:"reported"`
	InputTokens            int64 `json:"input_tokens,omitempty"`
	OutputTokens           int64 `json:"output_tokens,omitempty"`
	CacheReadTokens        int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens       int64 `json:"cache_write_tokens,omitempty"`
	InputIncludesCacheRead bool  `json:"input_includes_cache_read,omitempty"`
}

type modelWork struct {
	message  *pb.Message
	index    int
	intent   string
	cacheKey string
	cached   string
}

type modelCandidate struct {
	message       *pb.Message
	index         int
	originalBytes int
	replacement   string
	source        string
	provider      string
	model         string
	usage         tokenUsage
	cacheKey      string
}

func prepareAndApplyModelBatch(req *pb.ChatRequest, works []modelWork) bool {
	if len(works) == 0 || expectedApplications <= 0 {
		return false
	}

	// Do not incur offload cost unless even a zero-cost, best-case replacement
	// would be economical. Cached candidates use their known final size; an
	// uncached candidate optimistically assumes zero bytes.
	var optimistic []modelCandidate
	hasUncached := false
	for _, work := range works {
		replacement := work.cached
		if replacement == "" {
			hasUncached = true
		}
		optimistic = append(optimistic, modelCandidate{
			message: work.message, index: work.index, originalBytes: len(work.message.Content), replacement: replacement,
			source: "cache_reuse", cacheKey: work.cacheKey,
		})
	}
	if hasUncached {
		preflight, ok := modelBatchReport(req, optimistic, false)
		if !ok || !evaluateModelReport(preflight) {
			return false
		}
	}

	var candidates []modelCandidate
	for _, work := range works {
		if work.cached != "" {
			candidates = append(candidates, modelCandidate{
				message: work.message, index: work.index, originalBytes: len(work.message.Content), replacement: work.cached,
				source: "cache_reuse", cacheKey: work.cacheKey,
			})
			continue
		}
		ctxStr := extractConversationContext(req.Messages, work.message.ToolCallId)
		payload, _ := json.Marshal(map[string]any{
			"system_prompt": "You are a tool output summarizer. Given a tool output and an extraction intent, return ONLY the relevant parts. Be concise. Do not add commentary.",
			"user_prompt": fmt.Sprintf("Intent: %s\n\nConversation context:\n%s\n\nTool output:\n%s\n\nExtract only the parts relevant to the intent.",
				work.intent, ctxStr, truncateForPrompt(work.message.Content, maxOffloadInputChars)),
		})
		result, err := sdk.HostCall("torana_offload_completion", string(payload))
		if err != nil || result == "" {
			continue
		}
		var response struct {
			Status     string     `json:"status"`
			Completion string     `json:"completion"`
			Provider   string     `json:"provider"`
			Model      string     `json:"model"`
			Usage      tokenUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(result), &response) != nil || response.Status != "ok" || response.Completion == "" || len(response.Completion) >= len(work.message.Content) {
			continue
		}
		candidates = append(candidates, modelCandidate{
			message: work.message, index: work.index, originalBytes: len(work.message.Content), replacement: response.Completion,
			source: "transformation", provider: response.Provider, model: response.Model, usage: response.Usage, cacheKey: work.cacheKey,
		})
	}
	if len(candidates) == 0 {
		return false
	}
	report, ok := modelBatchReport(req, candidates, true)
	if !ok || !evaluateModelReport(report) {
		return false
	}
	for _, candidate := range candidates {
		candidate.message.Content = candidate.replacement
		if candidate.source == "transformation" {
			payload, _ := json.Marshal(map[string]string{"key": candidate.cacheKey, "value": candidate.replacement})
			_, _ = sdk.HostCall("env.cache_set", string(payload))
		}
	}
	payload, _ := json.Marshal(report)
	_, _ = sdk.HostCall("torana_record_savings", string(payload))
	return true
}

func modelBatchReport(req *pb.ChatRequest, candidates []modelCandidate, includeOffload bool) (map[string]any, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	earliest := len(req.Messages)
	originalBytes, finalBytes := 0, 0
	source := "cache_reuse"
	var provider, model string
	usage := tokenUsage{Reported: true}
	hasTransformation := false
	for _, candidate := range candidates {
		if candidate.index < earliest {
			earliest = candidate.index
		}
		originalBytes += candidate.originalBytes
		finalBytes += len(candidate.replacement)
		if candidate.source != "transformation" {
			continue
		}
		hasTransformation = true
		source = "transformation"
		if provider == "" {
			provider, model = candidate.provider, candidate.model
		}
		if provider != candidate.provider || model != candidate.model {
			return nil, false
		}
		usage.Reported = usage.Reported && candidate.usage.Reported
		usage.InputTokens += candidate.usage.InputTokens
		usage.OutputTokens += candidate.usage.OutputTokens
		usage.CacheReadTokens += candidate.usage.CacheReadTokens
		usage.CacheWriteTokens += candidate.usage.CacheWriteTokens
		usage.InputIncludesCacheRead = usage.InputIncludesCacheRead || candidate.usage.InputIncludesCacheRead
	}

	tail := proto.Clone(req).(*pb.ChatRequest)
	tail.Messages = tail.Messages[earliest:]
	rewriteBytes := proto.Size(tail) - originalBytes + finalBytes
	report := map[string]any{
		"original_bytes":                originalBytes,
		"final_bytes":                   finalBytes,
		"estimated_tokens_removed":      estimateTokens(originalBytes - finalBytes),
		"estimated_rewrite_span_tokens": estimateTokens(rewriteBytes),
		"estimator":                     "protobuf_tail_bytes_adjusted_div_4_ceil",
		"candidate_count":               len(candidates),
		"expected_applications":         expectedApplications,
		"source":                        source,
	}
	if includeOffload && hasTransformation {
		if provider == "" || model == "" || !usage.Reported {
			return nil, false
		}
		report["offload"] = map[string]any{"provider": provider, "model": model, "usage": usage}
	}
	return report, true
}

func evaluateModelReport(report map[string]any) bool {
	payload, _ := json.Marshal(report)
	result, err := sdk.HostCall("torana_evaluate_compaction", string(payload))
	if err != nil {
		return false
	}
	var decision struct {
		Apply bool `json:"apply"`
	}
	if json.Unmarshal([]byte(result), &decision) != nil || !decision.Apply {
		return false
	}
	return true
}

func estimateTokens(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + 3) / 4
}

func assistantMessageCountsAfter(messages []*pb.Message) []int {
	counts := make([]int, len(messages))
	count := 0
	for i := len(messages) - 1; i >= 0; i-- {
		counts[i] = count
		if messages[i].Role == "assistant" {
			count++
		}
	}
	return counts
}

func applyDeterministicPolicy(msg *pb.Message, toolName, toolArgs string, rule sdk.ToolPolicyRule, markerOnly bool) bool {
	cacheKey := sdk.ContentAddressedCacheKey(policyCompactionCache,
		"v2", toolName, toolArgs, msg.Content, rule.Mode, rule.Rerun)
	cached, _ := sdk.HostCall("env.cache_get", cacheKey)
	if cached != "" {
		recordSavings(len(msg.Content), len(cached), "cache_reuse")
		msg.Content = cached
		return true
	}
	replacement := sdk.DeterministicToolReplacement(toolName, toolArgs, msg.Content, rule.Mode, rule.Rerun, markerOnly)
	if len(replacement) >= len(msg.Content) {
		return false
	}
	recordSavings(len(msg.Content), len(replacement), "transformation")
	msg.Content = replacement
	payload, _ := json.Marshal(map[string]string{"key": cacheKey, "value": replacement})
	_, _ = sdk.HostCall("env.cache_set", string(payload))
	return true
}

// recordSavings reports compaction byte savings to /stats via the host.
func recordSavings(originalBytes, finalBytes int, source string) {
	sdk.HostCall("torana_record_savings",
		fmt.Sprintf(`{"original_bytes":%d,"final_bytes":%d,"source":%q}`, originalBytes, finalBytes, source))
}

func extractConversationContext(msgs []*pb.Message, excludeToolCallID string) string {
	var parts []string
	collected := 0
	for i := len(msgs) - 1; i >= 0 && collected < 5; i-- {
		msg := msgs[i]
		if msg.ToolCallId == excludeToolCallID {
			continue
		}
		content := ""
		switch msg.Role {
		case "user":
			content = msg.Content
		case "assistant":
			if msg.Content != "" {
				content = msg.Content
			}
		case "tool":
			continue
		default:
			continue
		}
		if content != "" {
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			parts = append([]string{msg.Role + ": " + content}, parts...)
			collected++
		}
	}
	if len(parts) == 0 {
		return "no prior conversation context available"
	}
	return strings.Join(parts, "\n")
}

// truncateForPrompt bounds the tool output sent to the summarizer. maxChars <= 0
// means unbounded: the complete output is sent (the default). A positive cap
// keeps the head and tail (first + last maxChars/2), where signal tends to
// cluster, dropping the middle.
func truncateForPrompt(content string, maxChars int) string {
	if maxChars <= 0 || len(content) <= maxChars {
		return content
	}
	half := maxChars / 2
	return content[:half] + "\n\n... [truncated] ...\n\n" + content[len(content)-half:]
}
