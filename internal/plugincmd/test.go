package plugincmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// pluginTestScenario is deliberately canonical: provider wire formats belong
// to the adapters, while plugin tests exercise the same IR as the pipeline.
type pluginTestScenario struct {
	Request         json.RawMessage   `json:"request"`
	ExpectedRequest json.RawMessage   `json:"expected_request"`
	Stream          []json.RawMessage `json:"stream"`
	ExpectedStream  []json.RawMessage `json:"expected_stream"`
	FailureMode     string            `json:"failure_mode,omitempty"`
	ExpectedError   string            `json:"expected_error,omitempty"`
}

type canonicalScenarioRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role string `json:"role"`
		Text string `json:"text"`
	} `json:"messages"`
}

func decodeScenarioRequest(raw []byte) (*engine.ChatRequest, error) {
	var wire canonicalScenarioRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	request := &engine.ChatRequest{Model: wire.Model}
	for _, message := range wire.Messages {
		request.Messages = append(request.Messages, engine.Message{Role: engine.Role(message.Role), Blocks: []engine.Block{{Text: &engine.TextBlock{Text: message.Text}}}})
	}
	return request, nil
}

func testPlugin(args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 || args[0] == "" {
		return errors.New("plugin directory is required")
	}
	dir := args[0]
	scenarioPath := filepath.Join(dir, "scenario.json")
	for i := 1; i < len(args); i++ {
		if args[i] != "--scenario" || i+1 >= len(args) {
			return errors.New("usage: torana plugin test <plugin-directory> [--scenario file]")
		}
		scenarioPath = args[i+1]
		i++
	}
	scenario, err := readPluginScenario(scenarioPath)
	if err != nil {
		return err
	}
	bundle, err := plugin.ValidateBundleDir(dir)
	if err != nil {
		return fmt.Errorf("validate plugin bundle: %w", err)
	}
	if len(bundle.WASMBytes) == 0 {
		return errors.New("plugin.wasm is required")
	}
	// Discovery consumes a directory of named bundles. Stage exactly the
	// supplied bundle so this command cannot accidentally run a neighbouring
	// plugin or use a stale pipeline generation.
	root, err := os.MkdirTemp("", "torana-plugin-test-")
	if err != nil {
		return fmt.Errorf("create test staging directory: %w", err)
	}
	defer os.RemoveAll(root)
	staged := filepath.Join(root, bundle.Manifest.Name)
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return fmt.Errorf("create plugin staging directory: %w", err)
	}
	if err := copyTree(dir, staged); err != nil {
		return fmt.Errorf("stage plugin bundle: %w", err)
	}
	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
		Dir: root, Order: []string{bundle.Manifest.Name}, AllowUnapproved: true, Strict: true,
	})
	if err != nil {
		return fmt.Errorf("load plugin pipeline: %w", err)
	}
	defer pp.DrainAndClose()
	ctx := context.Background()
	var runErr error
	if len(scenario.Request) != 0 {
		request, err := decodeScenarioRequest(scenario.Request)
		if err != nil {
			return fmt.Errorf("decode scenario request: %w", err)
		}
		actual, _, err := pp.RunBeforeRequestTracked(ctx, 1, request, nil)
		if err != nil {
			runErr = err
		} else if len(scenario.ExpectedRequest) != 0 {
			expected, err := decodeScenarioRequest(scenario.ExpectedRequest)
			if err != nil {
				return fmt.Errorf("decode expected request: %w", err)
			}
			if !reflect.DeepEqual(actual, expected) {
				return fmt.Errorf("request mismatch: got %s, want %s", compactJSON(actual), compactJSON(expected))
			}
		}
	}
	if runErr == nil && len(scenario.Stream) != 0 {
		var actual []engine.StreamEvent
		for i, raw := range scenario.Stream {
			var event engine.StreamEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return fmt.Errorf("decode stream event %d: %w", i, err)
			}
			out, err := pp.RunOnStreamChunkVerified(ctx, 1, &event)
			if err != nil {
				runErr = err
				break
			}
			actual = append(actual, out...)
		}
		// Finalization is mandatory even after a callback or verifier failure:
		// it releases the stream journal and validates that no buffered prefix
		// can be silently discarded.
		endErr := pp.EndStreamVerified(1)
		if runErr == nil {
			runErr = endErr
		}
		if runErr == nil && len(scenario.ExpectedStream) != 0 {
			var expected []engine.StreamEvent
			for i, raw := range scenario.ExpectedStream {
				var event engine.StreamEvent
				if err := json.Unmarshal(raw, &event); err != nil {
					return fmt.Errorf("decode expected stream event %d: %w", i, err)
				}
				expected = append(expected, event)
			}
			if !reflect.DeepEqual(actual, expected) {
				return fmt.Errorf("stream mismatch: got %s, want %s", compactJSON(actual), compactJSON(expected))
			}
		}
	}
	pp.EndRequest(1)
	if scenario.ExpectedError != "" {
		if runErr == nil || !strings.Contains(runErr.Error(), scenario.ExpectedError) {
			return fmt.Errorf("expected error %q, got %v", scenario.ExpectedError, runErr)
		}
	} else if runErr != nil {
		return runErr
	}
	fmt.Fprintf(stdout, "Plugin test passed: %s\n", bundle.Manifest.Name)
	return nil
}

func readPluginScenario(path string) (pluginTestScenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pluginTestScenario{}, fmt.Errorf("read scenario: %w", err)
	}
	var scenario pluginTestScenario
	if err := json.Unmarshal(raw, &scenario); err != nil {
		return pluginTestScenario{}, fmt.Errorf("parse scenario: %w", err)
	}
	return scenario, nil
}

func compactJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
