# Contributing to Torana

Help improve the proxy, a test, or a confusing first step. Start with the
build below, then choose the guide for the part you want to change.

## Getting a build

```bash
go build ./...
make testdata     # builds the WASM test fixtures the plugin tests need
make test         # quick gate: fixtures + go test ./... (a few minutes)
make test-race    # slow pre-merge gate: fixtures + go test ./... -race (~15 min)
```

`.wasm` files are build artifacts and are gitignored, so both targets build the
fixtures first. `TORANA_E2E=1` turns a missing fixture from a skip into a hard
failure.

> **Use `make test`, not `go test ./...`.** On a tree whose fixtures are not
> built, a bare `go test ./...` skips every plugin-behaviour test and still
> prints `ok` for each package — a green run that exercised none of the plugin
> sandbox, the hook pipeline, or the capability boundary. `make test` sets
> `TORANA_E2E=1`, which makes a missing fixture fail instead. The
> fixture-dependent packages print a banner when you take the other path, but
> the banner is a safety net, not the contract: **`make test` is the gate CI
> runs.**

`make test` is the everyday iteration command. `make test-race` is the slow
gate run before merging — the proxy package alone takes ~13 minutes under
`-race`, so it is a deliberate separate step rather than the default (CI runs
it with the same 1800s timeout).

### The official plugins are not in this repository

They live in [torana-plugins](https://github.com/torana-edge/torana-plugins),
and are not copied into this repository. Test bundles built from their owning
source so the tests exercise the same implementation users install.

So the tests split by what they assert:

- **Host mechanics** — hook dispatch, grant refusal, verdict handling, the raw
  ABI — run against the purpose-built fixtures in `examples/plugins/`. These are
  the tests you are almost certainly writing, and they need nothing external.
- **Plugin behaviour** — does `pii` block, does the warmer stop at break-even —
  is gated on `TORANA_PLUGIN_BUNDLES_DIR` and runs from torana-plugins CI, which
  builds the bundles from the repo that owns them.

To run the second set locally:

```bash
git clone https://github.com/torana-edge/torana-plugins ../torana-plugins
make official-plugins
TORANA_PLUGIN_BUNDLES_DIR=$(pwd)/../torana-plugins/dist go test ./...
```

**If you are adding a test, ask which kind it is.** A test that needs a real
plugin to state its assertion is testing that plugin, and belongs behind the
gate. Host-only tests should use purpose-built fixtures.

### Working across the four repositories

Torana is split across `torana-edge` (the proxy), `torana-plugin-sdk` (the ABI
and SDKs), `torana-plugins` (the official plugins), and `torana-site`.

If you are changing the SDK and the proxy together, `go.mod` will still point at
the published SDK version and your local changes will be invisible. Link them
with a workspace:

```bash
cd torana-edge
go work init . ../torana-plugin-sdk
```

`go.work` is gitignored, so this stays local. Without it you will see confusing
`undefined: pb.Something` errors for types you just added.

## Formatting and linting

For embedded control-plane JavaScript changes, run
`node --test internal/controlplane/*_test.mjs` with Node 22 or newer.
These exercise UI behavior; the Go tests also verify embedded asset serving.

CI runs `golangci-lint`. It does **not** run `gofmt`, so a handful of files have
drifted and `gofmt -l` is currently noisy. Format what you touch; don't reformat
files you didn't otherwise change, since that buries your diff.

One lint trap worth knowing: `.golangci.yml` excludes unchecked errors from
`fmt.Fprint` and `fmt.Fprintf` but **not `fmt.Fprintln`**. If you use `Fprintln`,
write `_, _ = fmt.Fprintln(...)` or errcheck will fail your build.

## Testing

Tests here are expected to explain *why* they exist, not just assert. A comment
saying what breaks in production if the assertion fails is worth more than the
assertion. Look at `internal/plugin/cache_compliance_test.go` for the house
style.

Some specific things:

- **Run `-race` before merging.** `make test-race` does (the quick `make test`
  does not). Anything touching the plugin runtime, the conversation registry,
  or stats is concurrent by nature.
- **Don't switch wazero to interpreter mode to make `-race` faster.** `-race`
  makes wazero's compiler pathologically slow and it is tempting, but interpreter
  mode stops exercising the code path that actually ships.
- **Verify a regression test actually regresses.** Disable your fix, watch the
  test fail, restore it. A test that passes both ways is worse than none.
- `httptest.NewRequest` sets a non-loopback `RemoteAddr`, and every control-plane
  route rejects those with a 403. Set `req.RemoteAddr = "127.0.0.1:12345"` and
  the `X-Torana-Local-Request: 1` header for mutations.

## Changing the plugin ABI

This is the highest-friction area, so it gets its own list.

The ABI lives in `torana-plugin-sdk`. Adding a hook or host call means touching
both repos:

**In the SDK:**

1. Add messages to `proto/torana/v1/torana.proto`. **Additive only** — CI runs
   `buf breaking` against `main`, so changing a field number or removing a field
   fails.
2. Regenerate with `./scripts/generate-go.sh` and **commit the result**. CI
   re-runs it and does `git diff --exit-code`.
   - This needs `protoc` and `protoc-gen-go` at the exact versions in the
     generated file's header. Mismatched versions produce a large spurious diff:
     ```bash
     go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
     ```
3. Register the hook in `sdk.go` **and add a no-op stub to `sdk_other.go`**.
   `sdk.go` is `//go:build wasip1`; without the stub, every plugin fails to
   compile for host-side tests.
4. Update the Go and Rust ABI-v1 SDK helpers and conformance tests. Keep both
   compiled conformance guests in the Edge host test: language support is a
   host-boundary contract, not merely two libraries that compile.
5. Update the plugin-author documentation, which lives in the **SDK
   repository**, not here: `torana-plugin-sdk/docs/PLUGIN_SEMANTICS.md` (the
   normative field table), `torana-plugin-sdk/docs/WRITING_A_PLUGIN.md`, and
   **`torana-plugin-sdk/docs/WASM_PLUGIN_GUIDE.md`**. That last one is the
   low-level contract reference for SDK implementers. Keep its host-function
   section and checklist aligned with the implementation.

**In torana-edge:**

6. Bump the SDK dependency. Hook and permission names are imported from
   `sdk.Hooks` and `sdk.Permissions`; do not maintain a second vocabulary.
   A recognized name alone does not implement a feature: add or update host
   dispatch, enforcement, resource plumbing and integration tests as needed.
7. ABI v1 exports one `run_hook` dispatcher and a `supported_hooks` bitmap,
   not one WASM function per hook. The loader checks the ABI revision, function
   signatures and exact equality between the manifest hooks and bitmap.
   Test both a valid guest and each invalid declaration.

### Everything an ABI change breaks

**Every plugin needs re-approving.** `bundleDigest` covers `plugin.json`,
`plugin.wasm`, `schema.json`, and `agent.json`. Adding a hook changes the
manifest, which changes the digest, which invalidates the operator's approval —
by design, since approvals are bound to exact artifacts. Update the golden digest
vectors in `internal/plugin/discovery_test.go` when you do.

**Never reimplement the digest.** `plugin.BundleDigestForDir` is the single
source of truth, shared with `torana plugin install`. A second implementation
that disagrees produces values that simply never match, and nothing errors.

## Things that are easy to get wrong

**`schema.json` has two supported shapes with different contracts.** A JSON
Schema document validates the complete stored configuration, including nested
objects, arrays, required values, and `additionalProperties`; the control plane
derives scalar form controls from it and leaves structured values to the raw
editor. The original `{"fields":[...]}` format remains supported as a UI
manifest: declared fields are type-checked, but undeclared keys are allowed. With
no schema, the host stores the plugin configuration opaquely.

**Adding a per-provider config field?** Add it to `unmanagedProviderFields` in
`internal/proxy/server.go` unless you also add it to the settings form. The form
rebuilds each provider from the fields it renders, so anything it doesn't know
about can be dropped on save. Add a round-trip test for the new field.

**Plugin state scoping.** `env.meta_*` is per-request and namespaced per plugin.
`env.cache_*` is a plugin-private, cross-request TTL cache.
`env.shared_cache_*` is the separately granted shared flat keyspace, so prefix
keys used in a documented producer/consumer protocol. Neither cache survives a
restart with the memory backend; durable `env.state_*` is private per plugin.

**Determinism is enforced.** `internal/plugin/cache_compliance_test.go` asserts
every request-mutating plugin produces byte-identical output across two runs with
different request IDs. This is not stylistic: a plugin that writes a timestamp or
a request ID into the request changes the prefix bytes, which invalidates the
provider's prompt cache and costs real money on every turn. If you add a plugin
that touches requests, add it to that test.

**Background hooks have no request.** Inside `run_on_tick` there is no request,
so unavailable `env.original_request`, `env.original_response`, and `env.meta_*`
reads return classified `NOT_FOUND` results, not ambiguous empty values. There
is no caller identity or credential context. Anything a tick needs must come
from the plugin's own state or an operator-bound platform resource; it must
never borrow credentials from unrelated caller traffic.

## Pull requests

For an opt-in check against a real provider, see
`scripts/live-harness-regression.sh`. It uses isolated Torana state, requires
credentials through environment-backed credential sources, and is deliberately
not part of CI. Run `scripts/live-harness-regression-test.sh` first to verify
its lifecycle and semantic-response guards without network access or a real
credential.

- Explain **why** in the description. The what is in the diff.
- Say what you verified and how. "Tests pass" is less useful than "disabled the
  fix, watched `TestX` fail, restored it".
- If you found something surprising while working, add it to this file. That's
  what it's for.
- Small, focused PRs review faster. If you find an unrelated bug, we would rather
  have it as its own PR than folded in.

If you are unsure whether an idea fits, open an issue first — it's cheaper than
building the wrong thing.
