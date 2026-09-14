// Package plugincmd implements Torana's plugin-authoring subcommands.
package plugincmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run executes a `torana plugin ...` command.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "plugin" {
		Usage(stderr)
		return errors.New("plugin subcommand required")
	}
	switch args[1] {
	case "init", "new":
		return initPlugin(args[2:], stdout)
	case "build":
		return buildPlugin(args[2:], stdout, stderr)
	case "test":
		return testPlugin(args[2:], stdout, stderr)
	case "lint":
		return lintPlugin(args[2:], stdout, stderr)
	case "install":
		return installPlugin(args[2:], stdout, stderr)
	case "list", "ls":
		return listPlugins(args[2:], stdout)
	case "remove", "rm":
		return removePlugin(args[2:], stdout)
	case "files":
		return listPluginFiles(args[2:], stdout)
	case "file":
		return pluginFile(args[2:], stdout)
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
	_, _ = fmt.Fprintln(w, "  torana plugin new <name>")
	_, _ = fmt.Fprintln(w, "  torana plugin init <name>")
	_, _ = fmt.Fprintln(w, "  torana plugin build [plugin-directory] [-o plugin.wasm]")
	_, _ = fmt.Fprintln(w, "  torana plugin test <plugin-directory> [--scenario file]")
	_, _ = fmt.Fprintln(w, "  torana plugin lint [plugin-directory]")
	_, _ = fmt.Fprintln(w, "  torana plugin install <source>... [--official] [--dir plugins]")
	_, _ = fmt.Fprintln(w, "  torana plugin list [--dir plugins]")
	_, _ = fmt.Fprintln(w, "  torana plugin remove <name>... [--dir plugins]")
	_, _ = fmt.Fprintln(w, "  torana plugin files <name>")
	_, _ = fmt.Fprintln(w, "  torana plugin file path <name> <logical-path>")
	_, _ = fmt.Fprintln(w, "  torana plugin file read <name> <logical-path>")
	_, _ = fmt.Fprintln(w, "  torana plugin file tail <name> <logical-path> [--follow]")
	_, _ = fmt.Fprintln(w, "  torana plugin file purge <name>")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "A source is a local path or a repository path:")
	_, _ = fmt.Fprintln(w, "  torana plugin install ./my-plugin")
	_, _ = fmt.Fprintln(w, "  torana plugin install github.com/you/your-plugins/plugins/foo")
	_, _ = fmt.Fprintln(w, "  torana plugin install github.com/you/your-plugins/plugins/foo@v1.2.0")
	_, _ = fmt.Fprintln(w, "  torana plugin install --official")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Plugins are compiled locally from source, never downloaded prebuilt,")
	_, _ = fmt.Fprintln(w, "and are not loaded until you approve their digest in the control plane.")
	_, _ = fmt.Fprintln(w, "Remote Rust sources must be cloned and reviewed before installing a local path,")
	_, _ = fmt.Fprintln(w, "because Cargo build scripts execute native code before digest approval.")
}

// ScaffoldSDKVersion is the exact module version used by the host and Go
// scaffolds. Rust resolves the same source through ScaffoldSDKRevision; a Git
// revision works before a package release and does not depend on crates.io.
const (
	ScaffoldSDKVersion  = "v0.4.3-0.20260914113223-3eed3394e409"
	ScaffoldSDKRevision = "3eed3394e4096ffd9e3afc3c75270353afd5120b"
	scaffoldSDKGitURL   = "https://github.com/torana-edge/torana-plugin-sdk"
	// scaffoldGoVersion tracks the SDK's own go directive. A scaffolded module
	// declaring an OLDER Go version than its dependency requires fails to build
	// with "module requires go >= x", which is the same class of unbuildable
	// first project this constant pair exists to prevent.
	scaffoldGoVersion = "1.25.0"
)

func scaffoldRustSDKDependency() string {
	return fmt.Sprintf(`torana-plugin-sdk = { git = %q, rev = %q }`, scaffoldSDKGitURL, ScaffoldSDKRevision)
}

func initPlugin(args []string, stdout io.Writer) error {
	if len(args) < 1 || args[0] == "" {
		return errors.New("plugin name is required")
	}
	pluginDir := args[0]
	language := "go"
	for i := 1; i < len(args); i++ {
		if args[i] != "--language" || i+1 >= len(args) {
			return errors.New("usage: torana plugin new <name> [--language go|rust]")
		}
		language = args[i+1]
		i++
	}
	if language != "go" && language != "rust" {
		return fmt.Errorf("unsupported plugin language %q", language)
	}
	pluginName := filepath.Base(pluginDir)
	absDir, err := filepath.Abs(pluginDir)
	if err != nil {
		return fmt.Errorf("resolve plugin directory: %w", err)
	}
	// Build off to the side so dependency failures leave no partial project.
	// Existing files are never overwritten, including files unrelated to Go.
	existed := false
	if info, statErr := os.Lstat(absDir); statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", absDir)
		}
		entries, readErr := os.ReadDir(absDir)
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			return fmt.Errorf("%s is not empty", absDir)
		}
		existed = true
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.MkdirAll(filepath.Dir(absDir), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(absDir), ".torana-plugin-new-")
	if err != nil {
		return fmt.Errorf("stage plugin directory: %w", err)
	}
	defer os.RemoveAll(stage)

	files := map[string]string{
		"go.mod": fmt.Sprintf(`module %s

go %s

require github.com/torana-edge/torana-plugin-sdk %s
`, pluginName, scaffoldGoVersion, ScaffoldSDKVersion),
		"plugin.wasm.go": `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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
		"plugin.wasm_test.go": `//go:build !wasip1

package main

import (
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// TestBeforeRequest exercises the registered hook through the SDK's native
// harness. This catches a scaffold whose init registration or checked result
// handling differs from the WASI entry point.
func TestBeforeRequest(t *testing.T) {
	result := sdktest.New(t).BeforeRequest(&pb.ChatRequest{})
	if result.Err != nil {
		t.Fatalf("before request: %v", result.Err)
	}
	if !result.PassedThrough {
		t.Fatalf("default hook did not pass the request through: %#v", result)
	}
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
	if language == "rust" {
		files = map[string]string{
			"Cargo.toml": fmt.Sprintf("[package]\nname = \"%s\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[lib]\ncrate-type = [\"cdylib\"]\n\n[dependencies]\n%s\n", pluginName, scaffoldRustSDKDependency()),
			"src/lib.rs": `use torana_plugin_sdk::{export_plugin_v1, info, pbv1, Plugin, RequestResult, HOOK_BEFORE_REQUEST};

struct PluginImpl;
impl Plugin for PluginImpl {
    const SUPPORTED_HOOKS: u32 = HOOK_BEFORE_REQUEST;
    fn before_request(request: pbv1::ChatRequest) -> Result<RequestResult, String> {
        info(&format!("received request for {}", request.model));
        Ok(RequestResult::pass())
    }
}
export_plugin_v1!(PluginImpl);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn before_request_passes_through() {
        let result = <PluginImpl as Plugin>::before_request(pbv1::ChatRequest::default())
            .expect("default hook succeeds");
        assert!(result.into_hook_result().is_none());
    }
}
`,
			"plugin.json": fmt.Sprintf(`{"schema_version":1,"id":"local/%s","name":"%s","version":"0.1.0","abi_version":"v1","description":"A local Torana Rust plugin","hooks":[{"name":"run_before_request"}],"permissions":[{"name":"env.log","description":"Diagnostic logging"}],"failure_mode":"pass"}`+"\n", pluginName, pluginName),
			"README.md":   "# " + pluginName + "\n\nThe Torana Rust SDK is pinned to the host SDK's exact Git revision in `Cargo.toml`. Run `cargo test` for native checks and `cargo build --release --target wasm32-wasip1` for the WASI artifact.\n",
		}
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(stage, name)), 0o755); err != nil {
			return fmt.Errorf("create directory for %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(stage, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	// Resolve the complete native test dependency graph while the project is
	// created. Without go.sum, the first `go test ./...` asks the author to run
	// `go mod tidy` manually, even though the scaffold is otherwise complete.
	if language == "go" {
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = stage
		tidy.Env = append(os.Environ(), "GOWORK=off", "GO111MODULE=on")
		if output, tidyErr := tidy.CombinedOutput(); tidyErr != nil {
			return fmt.Errorf("resolve scaffold dependencies: %w\n%s", tidyErr, strings.TrimSpace(string(output)))
		}
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if existed {
		// Remove only an empty directory; a concurrent user write aborts safely.
		if err := os.Remove(absDir); err != nil {
			return fmt.Errorf("publish plugin directory: %w", err)
		}
	} else if _, err := os.Lstat(absDir); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("plugin directory appeared during creation: %s", absDir)
	}
	if err := os.Rename(stage, absDir); err != nil {
		if existed {
			_ = os.Mkdir(absDir, 0o755)
		}
		return fmt.Errorf("publish plugin directory: %w", err)
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
	language, err := detectPluginLanguage(absDir)
	if err != nil {
		return err
	}
	// Lint Go source before compiling. `build` used to validate nothing at all, so the
	// first check on a manifest happened at install — or, for a hook declared
	// with no handler, never. Reporting a WASM build as successful when the
	// bundle cannot load, or loads and does nothing, is the wrong answer to
	// give an author.
	// Rust has an ABI-v1 SDK and the installed bundle receives the same host
	// validation, but the Go AST capability-attribution linter does not pretend
	// it can understand Rust source.
	if language == pluginGo {
		if findings, lintErr := lintDir(absDir); lintErr == nil {
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
	if _, err := buildPluginSource(stage, absOut, false, stdout, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Built %s\n", absOut)
	return nil
}
