# Upgrade notes

## Plugin installation

`torana plugin install --official` has been removed. Install each plugin from
the source you chose, for example:

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
```

This does not remove installed plugins or change their approvals. It removes
the host-owned install-all list; plugin discovery remains separate from the
explicit source, local build, digest review, and approval flow.

## Responses fields in plugins

Responses `instructions`, `max_output_tokens`, `temperature`, and `top_p` now
populate canonical IR fields. Plugins must use the system message and canonical
generation-parameter fields to inspect or change them. Responses replacements
that put canonical wire members (`model`, `instructions`, `input`, `tools`,
`stream`, `max_output_tokens`, `temperature`, or `top_p`) in
`provider_extensions_json` are rejected instead of overriding the checked IR.
ABI remains v1 and existing plugins need no release or pin change; custom
plugins that used those extension members must update their field access.

## Prompt-cache prefix identity

Cache-prefix fingerprints now include Responses instruction placement. This
rotates existing prefix keys once for **all request formats**, including native
routes without a bridge. Existing entries under old keys will miss until the
cache warms again; operators may see a temporary reduction in cache hit rates.
The change prevents distinct Responses request layouts from sharing a key.

## Provider base URLs

Provider base URLs containing a query string, fragment, or userinfo now fail
configuration validation. Existing configurations with these components will
fail startup until corrected. A rejected hot reload leaves the running
configuration in place.

Use a base URL containing only the supported scheme, host, and path. Put
supported query parameters on individual request URLs, and configure credentials
through the provider authentication settings rather than URL userinfo.
Provider-level query defaults (for example Azure `api-version`) are not added
by this change; clients must supply required request query parameters.
