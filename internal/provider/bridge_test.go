package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/bridge"
)

func TestBridgeConfigurationValidation(t *testing.T) {
	base := Provider{URL: "https://example.test", Format: "openai", Auth: ProviderAuth{Mode: "none"}, Bridge: &BridgeConfig{Client: bridge.Anthropic, Upstream: bridge.OpenAIChat, Model: "local-model"}}
	if err := base.ValidateBridge("local"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*Provider)
	}{
		{"unknown client", func(p *Provider) { p.Bridge.Client = "future" }},
		{"unknown upstream", func(p *Provider) { p.Bridge.Upstream = "future" }},
		{"family mismatch", func(p *Provider) { p.Format = "anthropic" }},
		{"implicit caller credential", func(p *Provider) { p.Auth = ProviderAuth{} }},
		{"explicit caller credential", func(p *Provider) { p.Auth.Mode = "caller" }},
		{"negative tokens", func(p *Provider) { p.Bridge.MaxTokens = -1 }},
		{"unrelated token limit default", func(p *Provider) { p.Bridge.MaxTokens = 128 }},
		{"unrelated project", func(p *Provider) { p.Bridge.Project = "p" }},
		{"Code Assist missing project", func(p *Provider) { p.Format = "gemini-codeassist"; p.Bridge.Upstream = bridge.GeminiCodeAssist }},
		{"Gemini model path injection", func(p *Provider) {
			p.Format = "gemini"
			p.Bridge.Upstream = bridge.Gemini
			p.Bridge.Model = "../secret"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := base
			b := *base.Bridge
			p.Bridge = &b
			test.change(&p)
			if err := p.ValidateBridge("local"); err == nil {
				t.Fatal("invalid bridge was accepted")
			}
		})
	}
}

func TestBridgeConfigurationJSONRoundTrip(t *testing.T) {
	raw := `{"port":8080,"providers":{"local":{"url":"http://localhost:8000","format":"openai","auth":{"mode":"none"},"bridge":{"client":"anthropic","upstream":"openai-chat","model":"local-model"}}}}`
	var cfg Config
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var next Config
	if err = json.Unmarshal(encoded, &next); err != nil {
		t.Fatal(err)
	}
	if next.Providers["local"].Bridge == nil || next.Providers["local"].Bridge.Client != bridge.Anthropic {
		t.Fatal("bridge lost on config roundtrip")
	}
}
