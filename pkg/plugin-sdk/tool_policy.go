package plugin_sdk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/torana-edge/torana-edge/pkg/pb"
)

const ToolOutputMarker = "[torana-tool-output v1]"

var nonzeroExitCodePattern = regexp.MustCompile(`(?i)(?:"exit_code"\s*:\s*|process exited with code\s+)([1-9][0-9]*)`)

// ToolNamesByCallID recovers provider-neutral tool names for result messages.
// Several wire adapters identify results only by tool_call_id.
func ToolNamesByCallID(messages []*pb.Message) map[string]string {
	names := make(map[string]string)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Id != "" && call.Name != "" {
				names[call.Id] = call.Name
			}
		}
	}
	return names
}

// ToolCallsByID returns original call metadata for cache identity and recovery.
func ToolCallsByID(messages []*pb.Message) map[string]*pb.ToolCall {
	calls := make(map[string]*pb.ToolCall)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Id != "" {
				calls[call.Id] = call
			}
		}
	}
	return calls
}

// ToolPolicyRule is evaluated in declaration order. Match is a
// case-insensitive shell-style pattern; the first match wins.
type ToolPolicyRule struct {
	Match     string `json:"match"`
	Mode      string `json:"mode"`
	FirstPass bool   `json:"first_pass,omitempty"`
	Rerun     string `json:"rerun,omitempty"`
}

// MatchToolPolicy returns the first configured rule matching toolName.
func MatchToolPolicy(rules []ToolPolicyRule, toolName string) (ToolPolicyRule, bool) {
	name := strings.ToLower(toolName)
	for _, rule := range rules {
		pattern := strings.ToLower(strings.TrimSpace(rule.Match))
		if pattern == "" {
			continue
		}
		matched, err := path.Match(pattern, name)
		if err != nil {
			matched = pattern == name
		}
		if matched {
			rule.Mode = strings.ToLower(strings.TrimSpace(rule.Mode))
			return rule, true
		}
	}
	return ToolPolicyRule{}, false
}

// ToolResultMustStayExact is the non-overridable safety layer. Mutation tools
// and outputs that look like failures remain verbatim even if a broad policy
// pattern would otherwise select them.
func ToolResultMustStayExact(toolName, content string) bool {
	for _, token := range toolNameTokens(toolName) {
		switch token {
		case "edit", "patch", "write", "diff", "replace", "delete", "remove", "error", "errors":
			return true
		}
	}
	compactName := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, toolName)
	for _, marker := range []string{"edit", "editor", "patch", "write", "diff", "replace", "delete", "remove", "error"} {
		if strings.Contains(compactName, marker) {
			return true
		}
	}

	lower := strings.ToLower(content)
	for _, marker := range []string{
		"error:", "fatal:", "panic:", "traceback (most recent call last):",
		"stack trace:", "command failed", `"status":"error"`, `"is_error":true`,
		": error:", "error[",
	} {
		if strings.HasPrefix(strings.TrimSpace(lower), marker) || strings.Contains(lower, "\n"+marker) || strings.Contains(lower, marker) {
			return true
		}
	}
	if nonzeroExitCodePattern.MatchString(lower) {
		return true
	}
	for _, line := range strings.Split(lower, "\n") {
		line = strings.TrimSpace(line)
		if line == "fail" || line == "failed" || strings.HasPrefix(line, "fail ") || strings.HasPrefix(line, "failed ") {
			return true
		}
		for _, prefix := range []string{"exit code ", "exit status "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
			if err == nil && code != 0 {
				return true
			}
		}
	}
	return false
}

func toolNameTokens(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// ToolOutputSHA256 returns the stable identity used by replacement metadata
// and cache keys.
func ToolOutputSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// DeterministicToolReplacement creates a stable, self-describing replacement.
// markerOnly is used for aged source reads; recoverable logs retain bounded
// head/tail evidence while still stating exactly how much was omitted.
func DeterministicToolReplacement(toolName, toolArguments, content, mode, rerun string, markerOnly bool) string {
	const retainedPartBytes = 768

	head, tail := "", ""
	if !markerOnly {
		head = validPrefix(content, retainedPartBytes)
		remaining := content[len(head):]
		if len(remaining) > retainedPartBytes {
			tail = validSuffix(remaining, retainedPartBytes)
		} else {
			head += remaining
		}
	}
	retainedBytes := len(head) + len(tail)
	omittedBytes := len(content) - retainedBytes
	if omittedBytes < 0 {
		omittedBytes = 0
	}
	if strings.TrimSpace(rerun) == "" {
		rerun = "Re-run this tool with the original arguments to recover the exact output."
	}

	arguments := oneLine(toolArguments)
	if len(arguments) > 512 {
		arguments = validPrefix(arguments, 512) + "..."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nmode: %s\ntool: %s\ntool_arguments: %s\noriginal_bytes: %d\nsha256: %s\nretained_bytes: %d\nomitted_bytes: %d\nrerun: %s",
		ToolOutputMarker, oneLine(mode), oneLine(toolName), arguments, len(content), ToolOutputSHA256(content), retainedBytes, omittedBytes, oneLine(rerun))
	if head != "" {
		b.WriteString("\n--- retained head ---\n")
		b.WriteString(head)
	}
	if tail != "" {
		b.WriteString("\n--- retained tail ---\n")
		b.WriteString(tail)
	}
	b.WriteString("\n[/torana-tool-output]")
	return b.String()
}

func IsDeterministicToolReplacement(content string) bool {
	return strings.HasPrefix(content, ToolOutputMarker+"\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func validPrefix(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func validSuffix(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func utf8RuneStart(b byte) bool { return b&0xc0 != 0x80 }
