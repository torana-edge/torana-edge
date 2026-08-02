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
	case "lint":
		return lintPlugin(args[2:], stdout, stderr)
	case "install":
		return installPlugin(args[2:], stdout, stderr)
	case "list", "ls":
		return listPlugins(args[2:], stdout)
	case "remove", "rm":
		return removePlugin(args[2:], stdout)
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
	_, _ = fmt.Fprintln(w, "  torana plugin lint [plugin-directory]")
	_, _ = fmt.Fprintln(w, "  torana plugin install <source>... [--official] [--dir plugins]")
	_, _ = fmt.Fprintln(w, "  torana plugin list [--dir plugins]")
	_, _ = fmt.Fprintln(w, "  torana plugin remove <name>... [--dir plugins]")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "A source is a local path or a repository path:")
	_, _ = fmt.Fprintln(w, "  torana plugin install ./my-plugin")
	_, _ = fmt.Fprintln(w, "  torana plugin install github.com/you/your-plugins/plugins/foo")
	_, _ = fmt.Fprintln(w, "  torana plugin install github.com/you/your-plugins/plugins/foo@v1.2.0")
	_, _ = fmt.Fprintln(w, "  torana plugin install --official")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Plugins are compiled locally from source, never downloaded prebuilt,")
	_, _ = fmt.Fprintln(w, "and are not loaded until you approve their digest in the control plane.")
}

// ScaffoldSDKVersion is the SDK release `torana plugin new` writes into a new
// plugin's go.mod, and scaffoldGoVersion the Go directive it writes.
//
// They are constants because the test used to assert the scaffolded string
// literally — so the scaffold and the assertion were two copies of the same
// value, and the test locked in whatever the scaffold said rather than
// checking it. It named SDK v0.1.0 long after v0.1.3 shipped, and the test
// passed the whole time.
//
// Bumping the SDK is now one edit here. Keep ScaffoldSDKVersion to a version
// that is actually published: a scaffold naming an unreleased tag produces a
// project that cannot build.
const (
	ScaffoldSDKVersion = "v0.2.1-0.20260802125903-d4908a648024"
	// scaffoldGoVersion tracks the SDK's own go directive. A scaffolded module
	// declaring an OLDER Go version than its dependency requires fails to build
	// with "module requires go >= x", which is the same class of unbuildable
	// first project this constant pair exists to prevent.
	scaffoldGoVersion = "1.25.0"
)

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

go %s

require github.com/torana-edge/torana-plugin-sdk %s
`, pluginName, scaffoldGoVersion, ScaffoldSDKVersion),
		"plugin.wasm.go": `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// main is empty on purpose. Under -buildmode=c-shared the runtime calls
// _initialize, which runs init() -- main() is NEVER called. Registering in
// main() produces a plugin that loads and does nothing, with no error anywhere.
func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		// PassRequest leaves the request untouched. Return
		// sdk.ReplaceRequest(req) after changing something -- and declare the
		// matching ir.*.write grant in plugin.json, or the host rejects it.
		return sdk.PassRequest(), nil
	})
}
`,
		"plugin.json": fmt.Sprintf(`{
  "schema_version": 1,
  "id": "local/%s",
  "name": "%s",
  "version": "0.1.0",
  "abi_version": "v2",
  "description": "A local Torana plugin",
  "hooks": [
    {"name": "run_before_request"}
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
	// Lint before compiling. `build` used to validate nothing at all, so the
	// first check on a manifest happened at install — or, for a hook declared
	// with no handler, never. Reporting a WASM build as successful when the
	// bundle cannot load, or loads and does nothing, is the wrong answer to
	// give an author.
	if findings, err := lintDir(absDir); err == nil {
		var failed bool
		for _, f := range findings {
			if f.sev == sevError {
				failed = true
				_, _ = fmt.Fprintf(stderr, "error: %s\n", f.msg)
			} else {
				_, _ = fmt.Fprintf(stderr, "warning: %s\n", f.msg)
			}
		}
		if failed {
			return errors.New("plugin has lint errors — fix them, or run 'torana plugin lint' for detail")
		}
	}

	fmt.Fprintf(stdout, "Building WASI plugin in %s\n", absDir)
	stage, err := os.MkdirTemp("", "torana-plugin-build-*")
	if err != nil {
		return fmt.Errorf("create build staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := copyTree(absDir, stage); err != nil {
		return fmt.Errorf("stage plugin source: %w", err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = stage
	tidy.Stdout = stdout
	tidy.Stderr = stderr
	if err := tidy.Run(); err != nil {
		return fmt.Errorf("resolve plugin dependencies: %w", err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared", "-buildvcs=false", "-o", absOut, ".")
	cmd.Dir = stage
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build plugin: %w", err)
	}
	fmt.Fprintf(stdout, "Built %s\n", absOut)
	return nil
}
