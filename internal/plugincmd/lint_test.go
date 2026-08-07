package plugincmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePlugin lays down a minimal bundle. manifest and source are written
// verbatim so a test can express exactly the mistake it is about.
func writePlugin(t *testing.T, manifest, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validManifest = `{
  "schema_version": 1,
  "id": "test/example",
  "name": "example",
  "version": "0.1.0",
  "abi_version": "v2",
  "failure_mode": "pass",
  "description": "test fixture",
  "hooks": [{"name": "run_before_request"}],
  "permissions": [{"name": "env.log", "description": "logging"}]
}`

func manifestWith(hooks, permissions string) string {
	return `{
  "schema_version": 1,
  "id": "test/example",
  "name": "example",
  "version": "0.1.0",
  "abi_version": "v2",
  "failure_mode": "pass",
  "description": "test fixture",
  "hooks": [` + hooks + `],
  "permissions": [` + permissions + `]
}`
}

func lintMessages(t *testing.T, dir string) []string {
	t.Helper()
	findings, err := lintDir(dir)
	if err != nil {
		t.Fatalf("lintDir: %v", err)
	}
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		prefix := "error: "
		if f.sev == sevWarning {
			prefix = "warning: "
		}
		out = append(out, prefix+f.msg)
	}
	return out
}

func assertContains(t *testing.T, msgs []string, substr string) {
	t.Helper()
	for _, m := range msgs {
		if strings.Contains(m, substr) {
			return
		}
	}
	t.Fatalf("no finding containing %q; got:\n  %s", substr, strings.Join(msgs, "\n  "))
}

func assertClean(t *testing.T, msgs []string) {
	t.Helper()
	if len(msgs) != 0 {
		t.Fatalf("expected no findings, got:\n  %s", strings.Join(msgs, "\n  "))
	}
}

// The bug PLUGIN_SEMANTICS.md §2 currently teaches. Under -buildmode=c-shared
// the host calls _initialize, which runs init() — main() never runs, so the
// handler is never registered and the plugin does nothing forever, with no
// error anywhere. Nothing detected this before.
func TestLintCatchesRegistrationInMain(t *testing.T) {
	dir := writePlugin(t, validManifest, `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, "registered inside func main()")
	assertContains(t, msgs, "would load healthy and never act")
}

func TestLintAcceptsRegistrationInInit(t *testing.T) {
	dir := writePlugin(t, validManifest, `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

// A package-level var initializer runs before main and is a valid place to
// register a handler. Tracking "which function am I in" with a variable that
// was set on entering func main() and never cleared meant everything declared
// after main inherited it, so this was reported as dead code and the build was
// blocked.
func TestLintAcceptsRegistrationInPackageInitializer(t *testing.T) {
	// The registration sits in a var initializer that appears AFTER func main.
	// Walking the whole file with a running scope variable left "main" set from
	// the preceding declaration, so this was flagged as dead code.
	dir := writePlugin(t, validManifest, `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

var _ = func() bool {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
	return true
}()
`)
	assertClean(t, lintMessages(t, dir))
}

// A registration genuinely inside main must still be caught even when other
// declarations follow it — the scope fix must not overshoot into missing the
// real bug.
func TestLintStillCatchesMainWhenDeclarationsFollow(t *testing.T) {
	dir := writePlugin(t, validManifest, `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
}

var unrelated = 1

func helper() int { return unrelated }
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, "registered inside func main()")
}

// A plugin is compiled with GOOS=wasip1 GOARCH=wasm, so a file excluded by a
// build constraint is not in the binary the host loads. Scanning it anyway
// reports handlers and capabilities that do not exist at runtime.
func TestLintHonoursBuildConstraints(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_tick"}`, `{"name":"env.log","description":"logging"}`),
		`package main

func main() {}
`)
	// The only OnTick registration lives in a file that never reaches a WASI
	// build, so the declared hook has no handler in the shipped plugin.
	excluded := `//go:build linux

package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func init() {
	sdk.OnTick(func(ctx context.Context, req *pb.TickRequest) (*pb.TickResult, error) {
		sdk.Log("tick", sdk.LogLevelInfo)
		return nil, nil
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "linux_only.go"), []byte(excluded), 0o600); err != nil {
		t.Fatal(err)
	}

	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `declares hook "run_on_tick" but no sdk.OnTick call`)
}

// The filename suffix convention is the same rule, and MatchFile honours both.
func TestLintHonoursFilenameConstraints(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_tick"}`, ``),
		`package main

func main() {}
`)
	excluded := `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func init() {
	sdk.OnTick(func(ctx context.Context, req *pb.TickRequest) (*pb.TickResult, error) {
		return nil, nil
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "hooks_darwin.go"), []byte(excluded), 0o600); err != nil {
		t.Fatal(err)
	}

	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `declares hook "run_on_tick" but no sdk.OnTick call`)
}

// wasip1/wasm builds with CGO_ENABLED=0, so a cgo-constrained file is not in
// the compiled plugin. build.Default inherits the host's CgoEnabled, which is
// usually true, and would otherwise credit the plugin with a handler it does
// not have.
func TestLintHonoursCgoConstraint(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_tick"}`, `{"name":"env.log","description":"logging"}`),
		`package main

func main() {}
`)
	excluded := `//go:build cgo

package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func init() {
	sdk.OnTick(func(ctx context.Context, req *pb.TickRequest) (*pb.TickResult, error) {
		sdk.Log("tick", sdk.LogLevelInfo)
		return nil, nil
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "cgo_only.go"), []byte(excluded), 0o600); err != nil {
		t.Fatal(err)
	}

	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `declares hook "run_on_tick" but no sdk.OnTick call`)
}

// The complement: a file constrained TO the plugin's real target must be
// scanned.
func TestLintScansWasipConstrainedFiles(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_tick"}`, `{"name":"env.log","description":"logging"}`),
		`package main

func main() {}
`)
	included := `//go:build wasip1

package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func init() {
	sdk.OnTick(func(ctx context.Context, req *pb.TickRequest) (*pb.TickResult, error) {
		sdk.Log("tick", sdk.LogLevelInfo)
		return nil, nil
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "wasm_only.go"), []byte(included), 0o600); err != nil {
		t.Fatal(err)
	}

	assertClean(t, lintMessages(t, dir))
}

func TestLintCatchesDeclaredHookWithNoHandler(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"},{"name":"run_on_tick"}`,
			`{"name":"env.log","description":"logging"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `declares hook "run_on_tick" but no sdk.OnTick call`)
}

func TestLintCatchesHandlerWithNoDeclaration(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, `{"name":"env.log","description":"logging"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (*pb.StreamEventResult, error) {
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `handler is registered for "run_on_stream_chunk" but plugin.json does not declare it`)
}

func TestLintCatchesUndeclaredCapability(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, `{"name":"env.log","description":"logging"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		_ = sdk.StateSet("k", "v")
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "env.state_set" but plugin.json does not request it`)
}

// A literal HostCall command maps to a capability by the same rule the host
// uses: env.* commands are their own permission, everything else is namespaced
// under env.host_call. Neither name is derivable from the other, which is
// exactly why this check earns its keep.
func TestLintDerivesCapabilityFromHostCallLiteral(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, `{"name":"env.log","description":"logging"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("x", sdk.LogLevelInfo)
		_, _ = sdk.HostCall("torana_offload_completion", "{}")
		_, _ = sdk.HostCall("env.cache_get", "k")
		return nil, nil
	})
}

`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `"env.host_call.torana_offload_completion"`)
	assertContains(t, msgs, `"env.cache_get"`)
}

func TestLintAttributesPrivateAndSharedCacheHelpersSeparately(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, `{"name":"env.cache_get","description":"private read"}`),
		`package main
import (
    "context"
    sdk "github.com/torana-edge/torana-plugin-sdk"
    pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)
func main() {}
func init() {
    sdk.OnBeforeRequest(func(context.Context, *pb.ChatRequest) (sdk.RequestResult, error) {
        _, _, _ = sdk.CacheGet("private")
        _, _ = sdk.SharedCacheSet("contract:key", "value")
        return sdk.PassRequest(), nil
    })
}`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "env.shared_cache_set" but plugin.json does not request it`)
	for _, msg := range msgs {
		if strings.Contains(msg, `uses "env.cache_get" but plugin.json does not request it`) {
			t.Fatalf("declared private cache helper was misattributed: %s", msg)
		}
	}
}

func TestLintWarnsOnUnusedCapability(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"env.log","description":"logging"},{"name":"env.now","description":"clock"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `requests "env.now" but no code uses it`)
}

// Hook gates are checked by the host before dispatch, not called from source.
// Reporting them as unused would be a false positive, and a linter that cries
// wolf gets ignored.
func TestLintDoesNotFlagHookGatesAsUnused(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_tick"},{"name":"run_on_http_request"}`,
			`{"name":"env.background_tick","description":"tick"},{"name":"env.serve_http","description":"http"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnTick(func(ctx context.Context, req *pb.TickRequest) (*pb.TickResult, error) {
		return nil, nil
	})
	sdk.OnHTTPRequest(func(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

func TestLintFlagsHookGateWithoutItsHook(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"env.background_tick","description":"tick"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `does not declare the "run_on_tick" hook it gates`)
}

// env.request_headers is injected into ToranaMeta by the host; the plugin
// reads it out of a JSON blob, so there is no call to attribute. It must never
// be reported as unused.
func TestLintDoesNotFlagUnattributableCapability(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"env.request_headers","description":"caller headers"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

// A HostCall whose command is computed cannot be attributed, so the linter
// must not claim the declared capabilities are unused — it can no longer see
// everything the plugin reaches for.
func TestLintSuppressesUnusedWhenHostCallIsDynamic(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"env.now","description":"clock"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

var cmd = "env.now"

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		_, _ = sdk.HostCall(cmd, "")
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

func TestLintRejectsUnknownVocabulary(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_whenever"}`, `{"name":"env.take_over_the_world","description":"no"}`),
		`package main

func main() {}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `unknown hook "run_whenever"`)
	assertContains(t, msgs, `unknown capability "env.take_over_the_world"`)
}

func TestLintReportsInvalidManifest(t *testing.T) {
	dir := writePlugin(t, `{"name":"example"}`, "package main\n\nfunc main() {}\n")
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, "plugin.json:")
}

// An unaliased import still resolves: the SDK's package name is plugin_sdk.
func TestLintResolvesUnaliasedImport(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, ``),
		`package main

import (
	"context"

	"github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	plugin_sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		plugin_sdk.Log("hi", plugin_sdk.LogLevelInfo)
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "env.log" but plugin.json does not request it`)
}

// writePluginWithSubpackage lays down a plugin whose registration and capability
// use both live in a subpackage, reached through a local import.
func writePluginWithSubpackage(t *testing.T, manifest string) string {
	t.Helper()
	dir := writePlugin(t, manifest, `package main

import "example.com/sub/internal/hooks"

func main() {}

func init() { hooks.Register() }
`)
	// The linter resolves local imports against the module path, so the
	// subpackage is only reachable if go.mod says who "example.com/sub" is.
	//
	// The trailing comment is deliberate and load-bearing: valid go.mod syntax
	// that a hand-rolled parser reads as part of the path, which then matches
	// no import and silently hides every subpackage.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/sub // the plugin\n\ngo 1.25.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "internal", "hooks")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `package hooks

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func Register() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hello", sdk.LogLevelInfo)
		_ = sdk.StateSet("k", "v")
		return nil, nil
	})
}
`
	if err := os.WriteFile(filepath.Join(sub, "hooks.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Scanning only the plugin's top-level directory got both directions wrong: a
// capability used from a subpackage was invisible, which is the failure this
// command exists to prevent.
func TestLintScansSubpackagesForCapabilities(t *testing.T) {
	dir := writePluginWithSubpackage(t,
		manifestWith(`{"name":"run_before_request"}`, ``))

	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "env.log" but plugin.json does not request it`)
	assertContains(t, msgs, `uses "env.state_set" but plugin.json does not request it`)
}

// The same blind spot produced a false POSITIVE too: a handler registered from a
// subpackage was reported as never registered, which blocked a valid build.
func TestLintSeesRegistrationInSubpackage(t *testing.T) {
	dir := writePluginWithSubpackage(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"env.log","description":"logging"},{"name":"env.state_set","description":"state"}`))

	assertClean(t, lintMessages(t, dir))
}

// A local package nothing imports is not compiled into the plugin, so its
// capabilities must not be reported. Walking the whole directory tree flagged a
// `tools/` helper and rejected a plugin that was perfectly correct.
func TestLintIgnoresUnreachablePackages(t *testing.T) {
	dir := writePlugin(t, validManifest, `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
}
`)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/plug\n\ngo 1.25.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A code generator or scratch helper: present on disk, imported by nothing.
	gen := filepath.Join(dir, "tools", "unused")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `package unused

import sdk "github.com/torana-edge/torana-plugin-sdk"

func Scratch() { _ = sdk.StateSet("k", "v") }
`
	if err := os.WriteFile(filepath.Join(gen, "gen.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	assertClean(t, lintMessages(t, dir))

	// Import it, and the same code must now be reported: reachability is the
	// property that decides, not presence on disk.
	main := filepath.Join(dir, "main.go")
	raw, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	withImport := strings.Replace(string(raw),
		`pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"`,
		"\"github.com/torana-edge/torana-plugin-sdk/pb\"\n\n\t\"example.com/plug/tools/unused\"", 1)
	withImport = strings.Replace(withImport,
		`sdk.Log("hi", sdk.LogLevelInfo)`,
		"sdk.Log(\"hi\", sdk.LogLevelInfo)\n\t\tunused.Scratch()", 1)
	if err := os.WriteFile(main, []byte(withImport), 0o600); err != nil {
		t.Fatal(err)
	}

	assertContains(t, lintMessages(t, dir), `uses "env.state_set" but plugin.json does not request it`)
}

// Directories the Go toolchain itself ignores must be ignored here too, or the
// linter reports capabilities from code that is never compiled into the plugin.
func TestLintSkipsVendorAndTestdata(t *testing.T) {
	dir := writePlugin(t, validManifest, `package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("hi", sdk.LogLevelInfo)
		return nil, nil
	})
}
`)
	stray := `package stray

import sdk "github.com/torana-edge/torana-plugin-sdk"

func Unused() { _ = sdk.StateSet("k", "v") }
`
	for _, sub := range []string{"vendor", "testdata", "_ignored"} {
		p := filepath.Join(dir, sub, "stray")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "stray.go"), []byte(stray), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	assertClean(t, lintMessages(t, dir))
}

// --- ir.cache_control.write (batch-3 prerequisite) ------------------------

// A plugin using the cache-breakpoint helpers with the grant declared is
// clean.
func TestLintCacheBreakpointHelperWithGrantIsClean(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"ir.cache_control.write","description":"markers"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.SetCacheBreakpoint(req.Messages[0], map[string]any{"type": "ephemeral"})
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

// Helper use WITHOUT the declaration is caught: the scanner attributes
// SetCacheBreakpoint / MoveCacheBreakpoint to ir.cache_control.write.
func TestLintCacheBreakpointHelperWithoutGrantIsCaught(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, ``),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.MoveCacheBreakpoint(req.Messages[0], req.Messages[1])
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "ir.cache_control.write" but plugin.json does not request it`)
}

// Direct protobuf assignment exercises a write grant with no attributable
// call site: the declared grant must never be warned as unused.
func TestLintDirectCacheControlAssignmentNotFlaggedUnused(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"ir.cache_control.write","description":"markers"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		req.Messages[0].CacheControlJson = []byte(`+"`"+`{"type":"ephemeral"}`+"`"+`)
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

// A plugin using the tool-result seam helper with the grant declared is
// clean.
func TestLintToolResultHelperWithGrantIsClean(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"ir.tool_results.write","description":"text"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		_, err := sdk.ReplaceToolResultText(req.Messages[0], 0, "changed")
		if err != nil {
			return nil, err
		}
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

// Helper use WITHOUT the declaration is caught: the scanner attributes
// ReplaceToolResultText to ir.tool_results.write.
func TestLintToolResultHelperWithoutGrantIsCaught(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, ``),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		_, err := sdk.ReplaceToolResultText(req.Messages[0], 0, "changed")
		if err != nil {
			return nil, err
		}
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "ir.tool_results.write" but plugin.json does not request it`)
}

// Direct protobuf assignment exercises the same grant with no attributable
// call site: the declared grant must never be warned as unused.
func TestLintDirectToolResultAssignmentNotFlaggedUnused(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"ir.tool_results.write","description":"text"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		req.Messages[0].Blocks[0].GetToolResult().Content[0].GetText().Text = "changed"
		return nil, nil
	})
}
`)
	assertClean(t, lintMessages(t, dir))
}

// The rule is general: NO write grant is warned as unused, because every one
// of them is exercised by direct protobuf mutation.
func TestLintNoWriteGrantWarnedUnused(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"ir.model.write","description":"model"},`+
				`{"name":"ir.messages.write.user","description":"user"},`+
				`{"name":"ir.tools.write","description":"tools"},`+
				`{"name":"ir.cache_control.write","description":"markers"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	for _, m := range msgs {
		if strings.Contains(m, "no code uses it") {
			t.Fatalf("write grant warned as unused: %s", m)
		}
	}
}

// TestLintAttributesSetIdentity — sdk.SetIdentity maps to env.set_identity,
// so using it without the declared permission is caught in the
// used-but-undeclared direction.
func TestLintAttributesSetIdentity(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`, ``),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.SetIdentity("tenant:x|team:y")
		return nil, nil
	})
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "env.set_identity" but plugin.json does not request it`)
}

// TestLintAttributesStreamMutationActions — every SDK stream mutation helper
// maps to ir.stream.write in the used-but-undeclared direction; each helper
// name gets an explicit decision.
func TestLintAttributesStreamMutationActions(t *testing.T) {
	actions := []string{
		`sdk.SuppressEvent()`,
		`sdk.EmitEvents()`,
		`sdk.EmitAssembledToolCall(sdk.ToolCall{}, "{}")`,
		`sdk.ReplaceToolArguments("{}")`,
		`sdk.SuppressToolCall()`,
		`sdk.ReplaceText("x")`,
		`sdk.SuppressText()`,
	}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			dir := writePlugin(t,
				manifestWith(`{"name":"run_before_request"}`, ``),
				`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		`+action+`
		return nil, nil
	})
}
`)
			msgs := lintMessages(t, dir)
			assertContains(t, msgs, `uses "ir.stream.write" but plugin.json does not request it`)
		})
	}
}

// TestLintOnToolCallFormsRequireStreamWrite — the bounded receiver analysis
// attributes ir.stream.write for OnToolCall on a NewStreamHandler-rooted
// value in every fluent shape: stored, chained, fluent OnTextDelta->OnToolCall,
// and an assigned fluent chain.
func TestLintOnToolCallFormsRequireStreamWrite(t *testing.T) {
	forms := map[string]string{
		"stored": `handler := sdk.NewStreamHandler()
	handler.OnToolCall(func(ctx context.Context, call sdk.ToolCall) (sdk.ToolCallAction, error) {
		return sdk.PassToolCall(), nil
	})
	handler.Register()`,
		"chained": `sdk.NewStreamHandler().OnToolCall(func(ctx context.Context, call sdk.ToolCall) (sdk.ToolCallAction, error) {
		return sdk.PassToolCall(), nil
	}).Register()`,
		"fluent text then tool": `sdk.NewStreamHandler().OnTextDelta(func(ctx context.Context, text string) (sdk.TextAction, error) {
		return sdk.PassText(), nil
	}).OnToolCall(func(ctx context.Context, call sdk.ToolCall) (sdk.ToolCallAction, error) {
		return sdk.PassToolCall(), nil
	}).Register()`,
		"assigned fluent chain": `handler := sdk.NewStreamHandler().OnTextDelta(func(ctx context.Context, text string) (sdk.TextAction, error) {
		return sdk.PassText(), nil
	})
	handler.OnToolCall(func(ctx context.Context, call sdk.ToolCall) (sdk.ToolCallAction, error) {
		return sdk.PassToolCall(), nil
	})
	handler.Register()`,
	}
	for name, body := range forms {
		t.Run(name, func(t *testing.T) {
			dir := writePlugin(t,
				manifestWith(`{"name":"run_on_stream_chunk"}`, ``),
				`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	`+body+`
}
`)
			msgs := lintMessages(t, dir)
			assertContains(t, msgs, `uses "ir.stream.write" but plugin.json does not request it`)
		})
	}
}

// TestLintNegativeHelpersNeverInferStreamWrite — every deliberately
// non-attributed assembler, plumbing, and pass helper is pinned
// INDIVIDUALLY: a mistaken mapping of any one of them would produce a
// diagnostic row here.
func TestLintNegativeHelpersNeverInferStreamWrite(t *testing.T) {
	negatives := map[string]string{
		"NewStreamHandler":   `sdk.NewStreamHandler()`,
		"NewStreamAssembler": `sdk.NewStreamAssembler()`,
		"WithToolAssembly":   `sdk.NewStreamAssembler().WithToolAssembly()`,
		"Feed":               `sdk.NewStreamAssembler().Feed(nil)`,
		"Register":           `sdk.NewStreamHandler().Register()`,
		"OnTextDelta": `sdk.NewStreamHandler().OnTextDelta(func(ctx context.Context, text string) (sdk.TextAction, error) {
		return sdk.PassText(), nil
	})`,
		"PassEvent":    `sdk.PassEvent()`,
		"PassToolCall": `sdk.PassToolCall()`,
		"PassText":     `sdk.PassText()`,
		"PassRequest":  `sdk.PassRequest()`,
		"PassResponse": `sdk.PassResponse()`,
	}
	for name, body := range negatives {
		t.Run(name, func(t *testing.T) {
			dir := writePlugin(t,
				manifestWith(`{"name":"run_on_stream_chunk"}`, ``),
				`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	`+body+`
}
`)
			msgs := lintMessages(t, dir)
			for _, m := range msgs {
				if strings.Contains(m, "ir.stream.write") {
					t.Fatalf("%s inferred ir.stream.write: %s", name, m)
				}
			}
		})
	}
}

// TestLintDeclaredStreamWriteNotWarnedAsUnused — the declared-but-unused
// suppression for write grants keeps a text-only plugin that DECLARES
// ir.stream.write quiet; attribution is about the undeclared direction.
func TestLintDeclaredStreamWriteNotWarnedAsUnused(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_stream_chunk"}`,
			`{"name":"ir.stream.write","description":"stream"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.NewStreamHandler().OnTextDelta(func(ctx context.Context, text string) (sdk.TextAction, error) {
		return sdk.PassText(), nil
	}).Register()
}
`)
	msgs := lintMessages(t, dir)
	for _, m := range msgs {
		if strings.Contains(m, "no code uses it") {
			t.Fatalf("write grant warned as unused: %s", m)
		}
	}
}

// TestLintStreamHandlerProvenanceIsLexical — provenance is per-function and
// per-declaration-identity, never identifier text: a local named "handler"
// seeded from sdk.NewStreamHandler() in one function must not mark an
// unrelated receiver of the same name in another function, a parameter of
// any type, or a shadowed local.
func TestLintStreamHandlerProvenanceIsLexical(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_stream_chunk"}`, ``),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

type custom struct{}

func (*custom) OnToolCall() {}

func seed() {
	handler := sdk.NewStreamHandler()
	_ = handler
}

func unrelated(handler *custom) {
	handler.OnToolCall()
}

func init() {}
`)
	msgs := lintMessages(t, dir)
	for _, m := range msgs {
		if strings.Contains(m, "ir.stream.write") {
			t.Fatalf("provenance leaked across functions by identifier text: %s", m)
		}
	}
}

// TestLintStreamHandlerProvenanceRespectsParameterBoundary — a
// *sdk.StreamHandler parameter never attributes ir.stream.write, even when a
// local of the same name was constructed from NewStreamHandler in another
// function.
func TestLintStreamHandlerProvenanceRespectsParameterBoundary(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_stream_chunk"}`, ``),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func seed() {
	handler := sdk.NewStreamHandler()
	_ = handler
}

func use(handler *sdk.StreamHandler) {
	handler.OnToolCall()
}

func init() {}
`)
	msgs := lintMessages(t, dir)
	for _, m := range msgs {
		if strings.Contains(m, "ir.stream.write") {
			t.Fatalf("parameter boundary violated: %s", m)
		}
	}
}

// TestLintStreamHandlerProvenanceShadowing — a shadowed local of the same
// name (inner block or closure parameter) does not inherit provenance.
func TestLintStreamHandlerProvenanceShadowing(t *testing.T) {
	for name, body := range map[string]string{
		"inner block shadow": `handler := sdk.NewStreamHandler()
	_ = handler
	{
		type custom struct{}
		var handler *custom
		handler.OnToolCall()
	}`,
		"closure parameter shadow": `handler := sdk.NewStreamHandler()
	_ = handler
	func(handler *sdk.StreamHandler) {
		handler.OnToolCall()
	}(handler)`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := writePlugin(t,
				manifestWith(`{"name":"run_on_stream_chunk"}`, ``),
				`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func init() {
	`+body+`
}
`)
			msgs := lintMessages(t, dir)
			for _, m := range msgs {
				if strings.Contains(m, "ir.stream.write") {
					t.Fatalf("shadowed local inherited provenance: %s", m)
				}
			}
		})
	}
}

// TestLintOnToolCallVarDeclarationForm — the var declaration form
// (var handler = sdk.NewStreamHandler()) is tracked like :=/assignment.
func TestLintOnToolCallVarDeclarationForm(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_on_stream_chunk"}`, ``),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func init() {
	var handler = sdk.NewStreamHandler()
	handler.OnToolCall(func(ctx context.Context, call sdk.ToolCall) (sdk.ToolCallAction, error) {
		return sdk.PassToolCall(), nil
	})
	handler.Register()
}
`)
	msgs := lintMessages(t, dir)
	assertContains(t, msgs, `uses "ir.stream.write" but plugin.json does not request it`)
}
