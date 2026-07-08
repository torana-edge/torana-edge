// Package middleware provides hooks for the Torana proxy pipeline.
package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// OffloadHook intercepts tool results in outbound requests and compacts
// them using a cheaper model when an extraction intent is available.
//
// Implements RequestHook only — it mutates tool result content in-place
// before the request reaches the upstream LLM.
type OffloadHook struct {
	// IntentCache is the shared cache populated by SchemaTranslator.
	IntentCache cache.IntentCache
	// Config controls offload behaviour (model, provider, enabled).
	Config provider.OffloadConfig
	// ProviderURL is the base URL of the provider used for offload calls.
	ProviderURL string
	// APIKeyExtractor extracts the API key from incoming requests.
	APIKeyExtractor func(req *http.Request) string
}

// NewOffloadHook creates an OffloadHook.
func NewOffloadHook(ic cache.IntentCache, cfg provider.OffloadConfig, providerURL string) *OffloadHook {
	return &OffloadHook{
		IntentCache:    ic,
		Config:      cfg,
		ProviderURL: providerURL,
		APIKeyExtractor: func(req *http.Request) string {
			auth := req.Header.Get("Authorization")
			return strings.TrimPrefix(auth, "Bearer ")
		},
	}
}

func (o *OffloadHook) Name() string { return "offload" }

// BeforeRequest scans chat messages for tool results, looks up cached
// intents, and compacts massive tool outputs before they reach the LLM.
func (o *OffloadHook) BeforeRequest(ctx context.Context, req *http.Request, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	if chat == nil || !o.Config.Enabled {
		return chat, nil
	}

	// Skip if schema translation didn't run (no mutations registry).
	// This happens when the format adapter fails to unmarshal the request
	// and it passes through untouched — there are no intents to find.
	if chat.ToranaMeta == nil || chat.ToranaMeta[metaKeyMutations] == nil {
		return chat, nil
	}

	compacted := 0
	bytesSaved := 0

	for i := range chat.Messages {
		msg := &chat.Messages[i]
		if msg.Role != engine.RoleTool || msg.ToolCallID == "" {
			continue
		}

		// Look up intent from the cache.
		intent, ok := o.IntentCache.Get(msg.ToolCallID)
		if !ok {
			continue
		}
		if intent == "" || msg.Content == "" {
			continue
		}

		originalLen := len(msg.Content)

		// Try deterministic compaction first (fast, free).
		compacted_content := o.compactDeterministic(msg.Content, intent)

		// If deterministic didn't help enough, try model delegation.
		if len(compacted_content) > len(msg.Content)/2 && originalLen > 2000 {
			context := extractConversationContext(chat, i, 5)
			modelResult, err := o.compactWithModel(ctx, req, msg.Content, intent, context)
			if err != nil {
				log.Printf("[offload] model compaction failed for %s: %v — using deterministic", msg.ToolCallID, err)
			} else if modelResult != "" {
				compacted_content = modelResult
			}
		}

		if compacted_content != msg.Content {
			saved := originalLen - len(compacted_content)
			pct := float64(saved) / float64(originalLen) * 100
			log.Printf("[offload] compacted %s: %d → %d bytes (%.1f%% reduction) intent=%q",
				msg.ToolCallID, originalLen, len(compacted_content), pct, intent)
			msg.Content = compacted_content
			compacted++
			bytesSaved += saved
		}
	}

	if compacted > 0 {
		log.Printf("[offload] compacted %d tool results, saved %d bytes total", compacted, bytesSaved)
	}

	return chat, nil
}

// extractConversationContext walks backward from msgIndex and collects
// the last N user/assistant messages with meaningful text content.
// Tool results are skipped or truncated to avoid context bloat.
func extractConversationContext(chat *engine.ChatRequest, msgIndex, maxMessages int) string {
	var parts []string
	collected := 0

	for i := msgIndex - 1; i >= 0 && collected < maxMessages; i-- {
		msg := chat.Messages[i]
		content := ""

		switch msg.Role {
		case engine.RoleUser:
			if msg.Content != "" {
				content = msg.Content
			}
		case engine.RoleAssistant:
			// Use text content if available, otherwise skip tool-call-only messages.
			if msg.Content != "" {
				content = msg.Content
			} else if len(msg.ContentParts) > 0 {
				// Flatten content parts into brief text.
				var parts []string
				for _, p := range msg.ContentParts {
					parts = append(parts, fmt.Sprintf("%v", p))
				}
				content = strings.Join(parts, " ")
			}
		case engine.RoleTool:
			// Skip tool results — they're too large for context.
			continue
		default:
			continue
		}

		if content != "" {
			// Truncate long content to 500 chars to avoid context bloat.
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			parts = append([]string{string(msg.Role) + ": " + content}, parts...)
			collected++
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return "Conversation history leading to this tool call:\n" + strings.Join(parts, "\n")
}

// ==========================================================================
// Deterministic compaction — grep for intent keywords in the output.
// ==========================================================================

// compactDeterministic extracts lines from content that match keywords
// derived from the intent string. Falls back to head+tail truncation if
// no keywords match.
func (o *OffloadHook) compactDeterministic(content, intent string) string {
	keywords := extractKeywords(intent)
	if len(keywords) == 0 {
		// No keywords to match — pass through to trigger model delegation.
		return content
	}

	lines := strings.Split(content, "\n")
	if len(lines) <= 50 {
		return content // already small enough
	}

	// Score each line by keyword matches.
	type scored struct {
		idx   int
		score int
	}
	var scoredLines []scored
	for i, line := range lines {
		s := 0
		lower := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				s++
			}
		}
		if s > 0 {
			scoredLines = append(scoredLines, scored{i, s})
		}
	}

	// If no lines match keywords, pass through to trigger model delegation.
	if len(scoredLines) == 0 {
		return content
	}

	// Sort by score descending, keep top matches + context.
	sort.Slice(scoredLines, func(a, b int) bool { return scoredLines[a].score > scoredLines[b].score })

	// Collect unique line indices with surrounding context (2 lines each side).
	keep := make(map[int]bool)
	contextLines := 2
	maxKeep := 200 // cap to prevent bloat
	for _, sl := range scoredLines {
		if len(keep) >= maxKeep {
			break
		}
		start := sl.idx - contextLines
		if start < 0 {
			start = 0
		}
		end := sl.idx + contextLines + 1
		if end > len(lines) {
			end = len(lines)
		}
		for j := start; j < end; j++ {
			keep[j] = true
		}
	}

	// Build result in original line order.
	var result []string
	for i, line := range lines {
		if keep[i] {
			result = append(result, line)
		}
	}

	joined := strings.Join(result, "\n")
	if len(joined) > 8000 {
		return truncateHeadTail(content, 2000)
	}
	return joined
}

// extractKeywords pulls meaningful words from an intent string.
func extractKeywords(intent string) []string {
	// Remove common stop words and punctuation.
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"in": true, "of": true, "to": true, "for": true, "and": true,
		"or": true, "that": true, "this": true, "be": true, "it": true,
		"what": true, "find": true, "extract": true, "look": true,
		"from": true, "with": true, "specify": true, "explicitly": true,
		"critical": true, "specifically": true, "information": true,
		"output": true, "tool": true, "result": true, "need": true,
	}

	words := strings.Fields(strings.ToLower(intent))
	var kw []string
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'()[]{}")
		if len(w) < 3 {
			continue
		}
		if stopWords[w] {
			continue
		}
		kw = append(kw, w)
	}
	return kw
}

// truncateHeadTail keeps the first and last N characters of content.
func truncateHeadTail(content string, n int) string {
	if len(content) <= n*2 {
		return content
	}
	half := n / 2
	head := content[:half]
	tail := content[len(content)-half:]
	return fmt.Sprintf("%s\n\n... [%d bytes truncated by Torana] ...\n\n%s",
		head, len(content)-n, tail)
}

// ==========================================================================
// Model-based compaction — delegate to a cheap LLM.
// ==========================================================================

// compactWithModel sends the tool result to a cheap model for summarization.
// conversationContext provides the last N user/assistant messages so the
// cheap model can infer the user's real goal even when the intent is terse.
func (o *OffloadHook) compactWithModel(ctx context.Context, req *http.Request, content, intent, conversationContext string) (string, error) {
	apiKey := o.APIKeyExtractor(req)
	if apiKey == "" {
		return "", fmt.Errorf("no API key found in request")
	}

	model := o.Config.Model
	if model == "" {
		model = "deepseek-v4-flash"
	}

	// Build a compact prompt for the cheap model, including conversation
	// context so it can infer the user's goal even from terse intents.
	systemPrompt := "You are a tool output summarizer. Given a tool output and an extraction intent, return ONLY the relevant parts. If conversation context is provided, use it to understand what the user was actually trying to accomplish. Be concise. Do not add commentary."

	userPrompt := fmt.Sprintf("Intent: %s\n\nTool output:\n%s", intent, truncateForPrompt(content, 14000))
	if conversationContext != "" {
		userPrompt = conversationContext + "\n\n" + userPrompt
	}
	userPrompt += "\n\nExtract only the parts relevant to the intent and conversation context. Return the filtered/summarized output."

	payload := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"stream":      false,
		"max_tokens":  1024,
		"temperature": 0,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", o.ProviderURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("offload model returned %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 200)]))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing offload response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in offload response")
	}

	summary := result.Choices[0].Message.Content
	if summary == "" {
		return "", fmt.Errorf("empty summary from offload model")
	}

	return summary, nil
}

// truncateForPrompt caps content to fit within a prompt budget.
func truncateForPrompt(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}
	half := maxChars / 2
	return content[:half] + "\n\n... [truncated for offload] ...\n\n" + content[len(content)-half:]
}

// Compile-time guard.
var _ engine.RequestHook = (*OffloadHook)(nil)

// Ensure imports used.
var _ = sort.Ints
