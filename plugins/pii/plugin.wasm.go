package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

const cleanCachePrefix = "pii_clean"

// pii scans tool results (grep/bash/etc. output) before they are forwarded to
// the cloud upstream. A deterministic regex pre-filter catches high-precision
// categories; anything else is sent to a configured LOCAL model for contextual
// detection. If PII is found the request is vetoed (env.block_request) with an
// actionable, value-free error so the upstream model can adjust next turn.

type piiConfig struct {
	Provider     string   `json:"provider"`       // local-model provider (required to enable the model scan)
	Model        string   `json:"model"`          // model name for the scan
	APIKeyEnv    string   `json:"api_key_env"`    // optional key env for that provider
	Tools        []string `json:"tools"`          // tool-name allowlist; empty or ["*"] = all tool results
	OnError      string   `json:"on_error"`       // "block" (default, fail-closed) | "allow" (fail-open)
	MaxScanChars int      `json:"max_scan_chars"` // cap on model-scan input; 0 = unbounded
}

var (
	cfgOnce sync.Once
	cfg     piiConfig
)

func loadConfig() {
	cfgOnce.Do(func() {
		cfg = piiConfig{OnError: "block"}
		if raw := sdk.PluginConfig(); raw != "" {
			_ = json.Unmarshal([]byte(raw), &cfg)
		}
		if cfg.OnError == "" {
			cfg.OnError = "block"
		}
	})
}

// High-precision patterns — deterministic, no model call, exact line numbers,
// and they still catch obvious PII when the local model is unavailable.
var piiPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"email", regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)},
	{"us_ssn", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{"aws_access_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

type finding struct {
	Type string
	Line int
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		loadConfig()

		// tool_call_id → tool name, so the allowlist can be applied even when
		// the tool-result message itself doesn't carry the name (OpenAI/Anthropic).
		nameByID := map[string]string{}
		for _, m := range req.Messages {
			for _, tc := range m.ToolCalls {
				nameByID[tc.Id] = tc.Name
			}
		}

		for _, msg := range req.Messages {
			if msg.Role != "tool" || msg.Content == "" {
				continue
			}
			toolName := msg.ToolName
			if toolName == "" {
				toolName = nameByID[msg.ToolCallId]
			}
			if !toolAllowed(toolName) {
				continue
			}
			// Skip results cleared on a prior turn (avoids re-scanning history).
			if msg.ToolCallId != "" {
				if clean, _ := sdk.HostCall("env.cache_get", cleanCachePrefix+":"+msg.ToolCallId); clean != "" {
					continue
				}
			}

			findings, err := scan(msg.Content, toolName)
			if err != nil {
				// Scanner failed. Fail-closed by default.
				if cfg.OnError == "allow" {
					continue
				}
				sdk.BlockRequest(req, 422, "pii_scan_failed",
					fmt.Sprintf("PII scan unavailable for %s; request blocked (fail-closed). Retry, or set pii.on_error=\"allow\" to forward unscanned.",
						toolLabel(toolName, msg.ToolCallId)))
				return req, nil
			}
			if len(findings) > 0 {
				sdk.BlockRequest(req, 422, "pii_detected", blockMessage(toolName, msg.ToolCallId, findings))
				return req, nil
			}
			if msg.ToolCallId != "" {
				sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":"%s:%s","value":"1"}`, cleanCachePrefix, msg.ToolCallId))
			}
		}
		return nil, nil
	})
}

func toolAllowed(name string) bool {
	if len(cfg.Tools) == 0 {
		return true // scan every tool result by default
	}
	for _, t := range cfg.Tools {
		if t == "*" || strings.EqualFold(t, name) {
			return true
		}
	}
	// Unknown tool name with an allowlist set: err toward scanning (don't miss PII).
	return name == ""
}

// scan runs the deterministic pre-filter first, then the local model.
func scan(content, toolName string) ([]finding, error) {
	if f := regexScan(content); len(f) > 0 {
		return f, nil
	}
	if cfg.Provider == "" {
		// No local model configured: regex-only mode. Nothing more to check.
		return nil, nil
	}
	return modelScan(content, toolName)
}

func regexScan(content string) []finding {
	var out []finding
	seen := map[string]bool{}
	for i, line := range strings.Split(content, "\n") {
		for _, p := range piiPatterns {
			if p.re.MatchString(line) {
				key := fmt.Sprintf("%s:%d", p.name, i+1)
				if !seen[key] {
					seen[key] = true
					out = append(out, finding{Type: p.name, Line: i + 1})
				}
			}
		}
	}
	return out
}

const piiSystemPrompt = `You are a PII detector. Examine the tool output and decide whether it contains personally identifiable information or secrets: emails, phone numbers, physical addresses, government IDs (e.g. SSNs), credit-card or bank numbers, API keys, passwords, private keys, or access tokens.

Respond with ONLY a JSON object and no other text:
{"pii": true|false, "findings": [{"type": "<category>", "line": <1-based line number>}]}

Never include the actual PII values — only the category and line number. If there is no PII, respond {"pii": false, "findings": []}.`

func modelScan(content, toolName string) ([]finding, error) {
	scanContent := content
	if cfg.MaxScanChars > 0 && len(scanContent) > cfg.MaxScanChars {
		scanContent = scanContent[:cfg.MaxScanChars]
	}
	payload, _ := json.Marshal(map[string]any{
		"provider":      cfg.Provider,
		"model":         cfg.Model,
		"api_key_env":   cfg.APIKeyEnv,
		"system_prompt": piiSystemPrompt,
		"user_prompt":   "Tool: " + toolName + "\n\nOutput to scan:\n" + scanContent,
	})
	res, err := sdk.HostCall("torana_offload_completion", string(payload))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Status     string `json:"status"`
		Completion string `json:"completion"`
		Message    string `json:"message"`
	}
	if json.Unmarshal([]byte(res), &resp) != nil || resp.Status != "ok" {
		return nil, fmt.Errorf("pii scan failed: %s", resp.Message)
	}
	var verdict struct {
		PII      bool `json:"pii"`
		Findings []struct {
			Type string `json:"type"`
			Line int    `json:"line"`
		} `json:"findings"`
	}
	if json.Unmarshal([]byte(extractJSON(resp.Completion)), &verdict) != nil {
		return nil, fmt.Errorf("pii scan: unparseable verdict")
	}
	if !verdict.PII {
		return nil, nil
	}
	out := make([]finding, 0, len(verdict.Findings))
	for _, f := range verdict.Findings {
		out = append(out, finding{Type: f.Type, Line: f.Line})
	}
	if len(out) == 0 {
		out = append(out, finding{Type: "unspecified"})
	}
	return out, nil
}

// extractJSON pulls the first {...} object out of a model reply that may be
// wrapped in prose or code fences.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func blockMessage(toolName, id string, findings []finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.Line > 0 {
			parts = append(parts, fmt.Sprintf("%s (line %d)", f.Type, f.Line))
		} else {
			parts = append(parts, f.Type)
		}
	}
	return fmt.Sprintf(
		"Blocked: PII detected in %s and NOT sent upstream. Found: %s. "+
			"Do not resend this content; reformulate to exclude or redact these values before returning the tool result.",
		toolLabel(toolName, id), strings.Join(parts, ", "))
}

func toolLabel(toolName, id string) string {
	switch {
	case toolName != "" && id != "":
		return fmt.Sprintf("`%s` output (%s)", toolName, id)
	case toolName != "":
		return fmt.Sprintf("`%s` output", toolName)
	case id != "":
		return fmt.Sprintf("tool result (%s)", id)
	default:
		return "a tool result"
	}
}
