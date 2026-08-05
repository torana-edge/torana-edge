package metrics

import "strings"

// modelFamily turns caller-controlled model identifiers into one of a fixed
// number of metric label values. Exact model strings are intentionally not
// labels: a client can choose a fresh value on every request and create an
// unbounded number of OTel series.
func modelFamily(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, family := range modelFamilies {
		for _, prefix := range family.prefixes {
			if strings.HasPrefix(model, prefix) {
				return family.name
			}
		}
	}
	return "other"
}

var modelFamilies = []struct {
	name     string
	prefixes []string
}{
	{name: "claude", prefixes: []string{"claude"}},
	{name: "openai", prefixes: []string{"gpt", "chatgpt", "o1", "o3", "o4"}},
	{name: "gemini", prefixes: []string{"gemini"}},
	{name: "deepseek", prefixes: []string{"deepseek"}},
	{name: "llama", prefixes: []string{"llama", "meta-llama"}},
	{name: "mistral", prefixes: []string{"mistral", "mixtral", "codestral"}},
	{name: "qwen", prefixes: []string{"qwen"}},
	{name: "command", prefixes: []string{"command", "cohere"}},
	{name: "grok", prefixes: []string{"grok"}},
}
