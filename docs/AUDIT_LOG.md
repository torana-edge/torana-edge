# Structured audit log

Torana can write one compact JSON object per intercepted inference request to
an operator-owned JSONL file. The feature is disabled by default because the
records are sensitive: they identify providers, models, plugin decisions, tool
names and IDs, and may include the intent plugin's explicit `"i"` string.

This is a host feature, not a WASM plugin. Plugins have no filesystem access,
cannot choose the destination, and cannot inject arbitrary error text into the
record. Only the exact plugin names and attributed verdict observed by the host
are recorded.

## Configuration

Add `audit` at the top level of the managed configuration:

```json
{
  "audit": {
    "enabled": true,
    "path": "/var/lib/torana/audit.jsonl",
    "max_file_bytes": 16777216,
    "max_files": 5
  }
}
```

`path` must be absolute. Torana creates the active file with mode `0600` and
refuses an existing symlink, non-regular file, or file accessible by group or
others. Zero or omitted bounds select 16 MiB and five files. `max_files` may be
at most 100. Rotation names older files `audit.jsonl.1`, `audit.jsonl.2`, and so
on. One unusually large record is kept intact rather than split across files.

The settings API preserves the current audit configuration when the `audit`
member is omitted. Sending `"audit": null` disables the sink. A replacement is
opened and validated before the persisted configuration and live writer are
swapped; a failed replacement leaves the last known-good writer active.

## What is recorded

Schema version 1 contains:

- timestamp and Torana request ID;
- initial and final provider and model;
- recognized format and inference path;
- caller-body and final upstream-request byte counts;
- ordered plugins actually invoked;
- attributed verdict, plugin, plugin-failure state, and a host-owned machine
  error class (including late stream failures after a 200 header); guest-chosen
  block codes and messages are never copied;
- ordered tool-call ID, name, and the explicit string `"i"` intent, when set;
- final HTTP status.

It never contains the raw request body, prompt text, tool arguments, arbitrary
plugin messages, credentials, or response body. Treat the file as sensitive
nonetheless: tool names, IDs, intent, routing, and timing can reveal operational
details. Apply your normal retention and access policy to the active and rotated
files.

## Interception boundary

Only a method and path recognized as an inference endpoint by the configured
provider format enters this log. Account, model-list, status, telemetry,
update, authentication, and unknown auxiliary calls pass through without an
audit record. A malformed body on a recognized inference endpoint is recorded
as `invalid_request`; it is still inside the inference boundary even though no
valid IR or plugin invocation exists.

## Failure behavior

Audit writes are serialized as complete lines but are not `fsync`ed per
request. Audit I/O is observability, not part of the provider response contract:
an append failure does not rewrite a response that may already have been sent.
Every failure increments `audit_write_failures` in host stats and
`torana_audit_write_failures_total` in OpenTelemetry. A value-free diagnostic
is emitted at most once per minute to avoid a failing disk causing a log storm.

Rotation failures put that writer into a stable failed state. Correct the
destination and replace or re-enable the audit configuration; Torana does not
silently redirect sensitive records elsewhere.
