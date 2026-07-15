package main

import (
	"context"
	"sort"
	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

const (
	minContentLength = 2000     // below this, content is already small enough
	contextLines     = 2        // lines of context around keyword matches
	maxKeepLines     = 200      // cap to prevent bloat
	maxResultBytes   = 8000     // cap result size
	intentCacheKey   = "intent" // cache key for intent (set by compactor plugin)
)

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		modified := false

		for _, msg := range req.Messages {
			if msg.Role != "tool" || msg.ToolCallId == "" || len(msg.Content) < minContentLength {
				continue
			}

			// Retrieve cached intent for this tool call.
			cacheKey := intentCacheKey + ":" + msg.ToolCallId
			intent, _ := sdk.HostCall("env.cache_get", cacheKey)
			if intent == "" {
				continue
			}

			compacted := compactDeterministic(msg.Content, intent)
			if compacted == msg.Content {
				continue
			}

			// Only apply if we actually reduced the size meaningfully (>50%).
			if len(compacted) >= len(msg.Content)/2 {
				continue
			}

			// Report savings to /stats via the host.
			sdk.HostCall("torana_record_savings",
				`{"original_bytes":`+itoa(len(msg.Content))+`,"final_bytes":`+itoa(len(compacted))+`}`)
			msg.Content = compacted
			modified = true
		}

		if !modified {
			return nil, nil
		}
		return req, nil
	})
}

// compactDeterministic extracts lines matching intent keywords.
// Falls back to head+tail truncation if no keywords match.
func compactDeterministic(content, intent string) string {
	keywords := extractKeywords(intent)
	if len(keywords) == 0 {
		return content
	}

	lines := strings.Split(content, "\n")
	if len(lines) <= 50 {
		return content
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

	if len(scoredLines) == 0 {
		return content // no matches — let model offload handle it
	}

	// Sort by score descending.
	sort.Slice(scoredLines, func(a, b int) bool { return scoredLines[a].score > scoredLines[b].score })

	// Collect unique line indices with surrounding context.
	keep := make(map[int]bool)
	for _, sl := range scoredLines {
		if len(keep) >= maxKeepLines {
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
	if len(joined) > maxResultBytes {
		return truncateHeadTail(content, 2000)
	}
	return joined
}

// extractKeywords pulls meaningful words from an intent string,
// filtering out stop words and short tokens.
func extractKeywords(intent string) []string {
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
	return head + "\n\n... [" + itoa(len(content)-n) + " bytes truncated by Torana] ...\n\n" + tail
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}
