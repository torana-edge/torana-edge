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
  "abi_version": "v1",
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
  "abi_version": "v1",
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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

func TestLintCatchesDeclaredHookWithNoHandler(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"},{"name":"run_on_tick"}`,
			`{"name":"env.log","description":"logging"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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

func TestLintWarnsOnUnusedCapability(t *testing.T) {
	dir := writePlugin(t,
		manifestWith(`{"name":"run_before_request"}`,
			`{"name":"env.log","description":"logging"},{"name":"env.now","description":"clock"}`),
		`package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
