# Torana documentation

Start small: route one request, see it in the feed, then add one plugin.

## Start and operate

- [Quickstart](QUICKSTART.md): build, connect your existing harness, see a request, add a plugin.
- [Harness setup](HARNESS_SETUP.md): recipes for your existing login or API key.
- [CLI](CLI.md): lifecycle, settings, pipeline, plugin approval and agent operations.
- [Suggestions](SUGGESTIONS.md) and [Torana commands](DIRECTIVES.md): review plugin advice and choose what to do from the CLI or a conversation.
- [Running plugins](PLUGINS.md): installation, resources, ordering and failure policy.
- [Credentials](CREDENTIALS.md): caller keys, named secrets and safe updates.
- [Harness compatibility](HARNESS_COMPATIBILITY.md) and [protocol bridges](PROTOCOL_BRIDGES.md).
- [Reasoning effort](EFFORT.md): optional, operator-declared effort choices across model API shapes.
- [Local models](LOCAL_MODELS.md) and [optional TLS ingress](GEMINI_ANTIGRAVITY.md).
- [Cache backends](CACHE.md), [provider prompt caching](PROMPT_CACHING.md), [Responses compaction](RESPONSES_COMPACTION.md).
- [Audit logging](AUDIT_LOG.md) and [upgrades](UPGRADE_NOTES.md).

## Extend and contribute

- [First plugin](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/FIRST_PLUGIN.md).
- [Plugin guides and examples](https://github.com/torana-edge/torana-plugins).
- [Plugin testing](PLUGIN_TESTING.md) and [agent operations](AGENT_CONTROL_PLANE.md).
- [Contributing](../CONTRIBUTING.md), [UI design](contributing/design.md), [release process](RELEASE_INSTALLERS.md).

## Evaluate the evidence

- [Performance reports](../benchmarks/README.md): measured revisions, workloads and limits.
- [Compaction experiment](https://github.com/torana-edge/torana-plugins/blob/main/plugins/compactor/DEEPSEEK_RESULTS.md): methodology and negative result, maintained with the plugin.
- [Run benchmarks](BENCHMARKS.md) against your own workload.

Edge owns operator instructions, the SDK owns authoring contracts, and Plugins
owns individual plugin behavior. The website provides a shorter introduction
and links to these references.
