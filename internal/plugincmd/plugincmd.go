// Package plugincmd implements Torana's plugin-authoring subcommands.
package plugincmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Run executes a `torana plugin ...` command.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "plugin" {
		Usage(stderr)
		return errors.New("plugin subcommand required")
	}
	switch args[1] {
	case "init":
		return initPlugin(args[2:], stdout)
	case "build":
		return buildPlugin(args[2:], stdout, stderr)
	case "help", "-h", "--help":
		Usage(stdout)
		return nil
	default:
		Usage(stderr)
		return fmt.Errorf("unknown plugin command %q", args[1])
	}
}

// Usage prints the authoring command summary.
func Usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  torana plugin init <name>")
	_, _ = fmt.Fprintln(w, "  torana plugin build [plugin-directory] [-o plugin.wasm]")
}

func initPlugin(args []string, stdout io.Writer) error {
	if len(args) < 1 || args[0] == "" {
		return errors.New("plugin name is required")
	}
	pluginDir := args[0]
	pluginName := filepath.Base(pluginDir)
	absDir, err := filepath.Abs(pluginDir)
	if err != nil {
		return fmt.Errorf("resolve plugin directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return fmt.Errorf("create plugin directory: %w", err)
	}
	goModPath := filepath.Join(absDir, "go.mod")
	if _, err := os.Stat(goModPath); err == nil {
		return fmt.Errorf("%s already contains a go.mod file", absDir)
	}

	files := map[string]string{
		"go.mod": fmt.Sprintf(`module %s

go 1.26

require github.com/torana-edge/torana-plugin-sdk v0.1.0
`, pluginName),
		"plugin.wasm.go": `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		return req, nil
	})
}
`,
		"plugin.json": fmt.Sprintf(`{
  "schema_version": 1,
  "id": "local/%s",
  "name": "%s",
  "version": "0.1.0",
  "abi_version": "v1",
  "description": "A local Torana plugin",
  "hooks": [
    {"name": "run_before_request", "priority": 100}
  ],
  "permissions": [],
  "failure_mode": "pass"
}
`, pluginName, pluginName),
		"schema.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(absDir, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	fmt.Fprintf(stdout, "Initialized %s in %s\n", pluginName, absDir)
	fmt.Fprintf(stdout, "Next: torana plugin build %s\n", pluginDir)
	return nil
}

func buildPlugin(args []string, stdout, stderr io.Writer) error {
	dir, out := ".", ""
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			if i+1 >= len(args) {
				return errors.New("-o requires an output path")
			}
			out = args[i+1]
			i++
		} else if dir == "." {
			dir = args[i]
		} else {
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if out == "" {
		out = filepath.Join(dir, "plugin.wasm")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve plugin directory: %w", err)
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("resolve output: %w", err)
	}
	fmt.Fprintf(stdout, "Building WASI plugin in %s\n", absDir)
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", absOut, ".")
	cmd.Dir = absDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build plugin: %w", err)
	}
	fmt.Fprintf(stdout, "Built %s\n", absOut)
	return nil
}
