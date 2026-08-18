package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// TestHotReloadConfigRetainsCandidateValidation is the regression for a split
// construction path: the initial pipeline installed the Code Assist envelope
// validator, while the filesystem watcher's configFn rebuilt without it. The
// same guest output could therefore be refused before a bundle reload and
// accepted afterward. Both paths now consume pipelinePluginConfig.
func TestHotReloadConfigRetainsCandidateValidation(t *testing.T) {
	const fixtures = "../../examples/plugins"
	requireWASM(t, fixtures+"/test-envelope-smuggler/plugin.wasm")

	s := &Server{config: Config{HostVersion: "dev"}}
	pcfg := provider.PluginsConfig{
		Dir:             fixtures,
		Order:           []string{"test-envelope-smuggler"},
		AllowUnapproved: true,
	}
	config := s.pipelinePluginConfig(pcfg)
	if config.CandidateValidator == nil {
		t.Fatal("hot-reload pipeline config dropped candidate validation")
	}

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := plugin.NewPipeline(rt, config)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := engine.ParseOptionalJSONObject([]byte(`{"project":"p-1","request":{"sessionId":"s-9"}}`))
	if err != nil {
		t.Fatal(err)
	}
	request := &engine.ChatRequest{
		Model:              "gemini-3.5-flash",
		CodeAssist:         true,
		ProviderExtensions: envelope,
		Messages: []engine.Message{{
			Role: engine.RoleUser,
			Blocks: []engine.Block{{Text: &engine.TextBlock{
				Text: "hi",
			}}},
		}},
	}
	_, err = pp.RunBeforeRequest(context.Background(), 1, request, nil)
	if err == nil || !strings.Contains(err.Error(), "test-envelope-smuggler") || !strings.Contains(err.Error(), "canonical outer member") {
		t.Fatalf("hot-reload generation accepted smuggled envelope: %v", err)
	}
}
