# Testing a plugin

`torana plugin test` runs one compiled plugin bundle against a closed JSON
scenario. It uses the production WASM runtime, approval checks, cache isolation,
durable state implementation, hook dispatch, and mutation verification. It does
not contact a provider, read live credentials, or use the network.

```bash
torana plugin build ./my-plugin
torana plugin test ./my-plugin
torana plugin test ./my-plugin --scenario ./cases/redacts-card.json
```

The default scenario is `scenario.json` in the plugin directory. A scenario
must contain at least one of the five hook inputs: `request`, `response`,
`stream`, `http`, or `tick`. Values use the canonical protobuf JSON names from
`torana.v1`, independent of any provider wire format. Protobuf `bytes` fields
are base64 strings and 64-bit integers are JSON strings.

```json
{
  "request": {
    "model": "example",
    "messages": [{"role": "user", "blocks": [{"text": {"text": "hello"}}]}]
  },
  "expected_request": {
    "model": "example",
    "messages": [{"role": "user", "blocks": [{"text": {"text": "hello [checked]"}}]}]
  },
  "expected_verdicts": {}
}
```

The matching expectations are `expected_request`, `expected_response`,
`expected_stream`, `expected_http`, and `expected_tick`. An empty stream array
asserts that the plugin emitted nothing. `expected_http: null` asserts HTTP
pass-through, and `expected_tick: null` asserts an idle tick. An omitted
expectation makes no assertion. A response following a stream is read-only by
default; set `response_mutable` explicitly when a case needs another policy.

`expected_verdicts` checks host-owned side effects from the request hook. Its
members are canonical `BlockRequestArgs`, `SyntheticResponse`,
`RouteRequestArgs`, and `SetIdentityArgs` values:

```json
{
  "request": {
    "messages": [{"role": "user", "blocks": [{"text": {"text": "deny me"}}]}]
  },
  "expected_verdicts": {
    "block": {"status": 403, "code": "policy", "message": "denied"},
    "route": {"provider": "managed", "model": "small"},
    "identity": {"identity": "tenant-7"}
  }
}
```

Use `{}` to assert that no block, response, route, or identity verdict was
recorded. Verdict attribution is also checked against the bundle under test.
`expected_verdicts` is valid only with `request`; `null` verdict members are
rejected because omission already expresses “expect none.”

The `services` object provides deterministic host-service doubles. Cache and
state values initialize real local stores. HTTP and model fixtures are FIFO,
keyed by manifest slot, and compare the complete canonical protobuf request
before returning one response or classified error.

```json
{
  "services": {
    "cache": {"private": {"decision": "allow"}, "shared": {"team:key": "value"}},
    "state": {"cursor": "17"},
    "http": {
      "policy": [{
        "request": {"endpoint": "policy", "method": "POST", "path": "/check", "body": "e30="},
        "response": {"status": 200, "body": "eyJvayI6dHJ1ZX0="}
      }]
    },
    "model": {
      "classifier": [{
        "request": {"service": "classifier", "messages": [{"role": "user", "blocks": [{"text": {"text": "classify"}}]}]},
        "error": {"code": "ERROR_CODE_UNAVAILABLE", "message": "fixture unavailable"}
      }]
    }
  },
  "request": {"messages": [{"role": "user", "blocks": [{"text": {"text": "test"}}]}]}
}
```

Every fixture must be consumed, and extra calls fail the scenario. A service
slot is bound only when both the manifest declares it and the scenario supplies
fixtures. The test runtime approves the exact bundle digest and grants only the
permissions requested by that manifest. Resource limits remain the manifest's
declared limits, so scenarios exercise the same host authority checks as the
running proxy.

Scenario JSON is closed and strict: unknown fields, duplicate keys at any
depth, trailing documents, invalid protobuf shapes, explicit null inputs, and
orphan expectations are errors. `expected_error` matches a substring of a hook
failure; service-fixture mismatches and unused fixtures still fail the test.
