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
		var request engine.ChatRequest
		if err := json.Unmarshal(scenario.Request, &request); err != nil {
			return fmt.Errorf("decode scenario request: %w", err)
		}
		actual, _, err := pp.RunBeforeRequestTracked(ctx, 1, &request, nil)
		if err != nil {
			runErr = err
		} else if len(scenario.ExpectedRequest) != 0 {
			var expected engine.ChatRequest
			if err := json.Unmarshal(scenario.ExpectedRequest, &expected); err != nil {
				return fmt.Errorf("decode expected request: %w", err)
			}
			if !reflect.DeepEqual(actual, &expected) {
				return fmt.Errorf("request mismatch: got %s, want %s", compactJSON(actual), compactJSON(&expected))
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
		if runErr == nil {
			runErr = pp.EndStreamVerified(1)
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
