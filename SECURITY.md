# Security boundaries

## Reporting a vulnerability

Please [report vulnerabilities privately through GitHub](https://github.com/torana-edge/torana-edge/security/advisories/new).
Include the affected version, reproduction steps, and expected impact. Do not
post credentials or exploit details in a public issue.

Torana runs on your machine. Its local control plane lets you and your tools
manage providers, approve plugins, and change the pipeline.

The control plane checks the caller's loopback address and Host. Mutations
also require a matching browser Origin or `X-Torana-Local-Request: 1` from a
local tool. These checks protect against browser cross-site requests and DNS
rebinding; the header is **not an operator credential**.

## Model-facing tools and local commands

MCP, `ask`, and conversation directives use a narrower operation policy.
Protected operations cannot be reached through those channels, and eligible
model-requested changes require user confirmation. The MCP token grants access
to that interface, not to the broader operator API.

These rules are not a sandbox for other programs running as your user. A
harness with unrestricted shell or local network access can call the operator
API directly, including token setup and plugin management. Protect that access
with your harness's permission prompts and sandbox/file/network restrictions.
Review requests to run local administrative commands as operator actions, not
as ordinary model-facing tool calls.

Keep the control plane on loopback. Do not expose it through a public tunnel or
forward it to an untrusted environment. Treat MCP tokens, instance secrets,
and the managed data directory as credentials.
