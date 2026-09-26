# Claude Code hook adapter

Torana can show a suggestion when Claude Code finishes a turn and observe model
switches, including a model restored on resume. Your harness still owns model
selection. These hooks never block stopping, force a switch, or approve changes.

Enable MCP and set `suggestions.claude_code.enabled` to `true` in your Torana
configuration. The adapter is off by default. It uses the same local MCP token;
token rotation immediately invalidates old hook credentials.

Set the token in the shell launching Claude Code without copying it into a file:

```bash
export TORANA_MCP_TOKEN="$(torana mcp token)"
```

Add these entries alongside any existing hooks in a Claude settings file, using
your Torana port. Do not replace unrelated settings or hooks.

```json
{
  "hooks": {
    "Stop": [{"hooks": [{
      "type": "http",
      "url": "http://127.0.0.1:8080/_torana/hooks/claude-code/stop",
      "timeout": 3,
      "headers": {"Authorization": "Bearer $TORANA_MCP_TOKEN", "X-Torana-Local-Request": "1"},
      "allowedEnvVars": ["TORANA_MCP_TOKEN"]
    }]}],
    "PostModelSwitch": [{"hooks": [{
      "type": "http",
      "url": "http://127.0.0.1:8080/_torana/hooks/claude-code/post-model-switch",
      "timeout": 3,
      "headers": {"Authorization": "Bearer $TORANA_MCP_TOKEN", "X-Torana-Local-Request": "1"},
      "allowedEnvVars": ["TORANA_MCP_TOKEN"]
    }]}]
  }
}
```

`Stop` returns only an informational `systemMessage` if this conversation has
a pending suggestion. It exposes no codes, plugin-authored text, or change inputs.
For model suggestions it points to `/model`; other proposals stay in Torana's
Review view or CLI. `PostModelSwitch` records a matching suggestion as accepted
through the adapter; it cannot accept a Torana operation or mutate configuration.

The host never reads `transcript_path` or `cwd`. Identity is the same hashed
Claude session identity used by routed requests. Both endpoints require the
loopback/origin guard, explicit local-request header and current bearer token.

This implements the host callback layer. Automatic hook setup and the optional
PreModelSwitch cost warning follow separately. PreModelSwitch is deliberately
not enabled here: Claude blocks a switch if that hook times out. Hook behavior
is covered by code tests; the real Claude session walkthrough is still pending.
See [Claude's hook reference](https://code.claude.com/docs/en/hooks).
