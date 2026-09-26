# Claude Code hook adapter

Torana can show a suggestion when Claude Code finishes a turn and observe model
switches, including a model restored on resume. Your harness still owns model
selection. These hooks never block stopping, force a switch, or approve changes.

Enable MCP and set `suggestions.claude_code.enabled` to `true` in your Torana
configuration. The adapter is off by default. It uses the same local MCP token;
token rotation immediately invalidates old hook credentials.

Preview and install the two nonblocking hooks without replacing your other
hooks or settings:

```bash
torana harness hooks setup claude-code --dry-run
torana harness hooks setup claude-code
```

User settings are the default. Choose `--scope project` for per-user project
settings (`.claude/settings.local.json`), never the shared `settings.json`,
or `--addr http://127.0.0.1:PORT` to pin a different Torana instance. Private
recovery backups and ownership records stay in Torana's data directory.
Teardown removes only exact groups Torana installed and refuses edited ones:

```bash
torana harness hooks teardown claude-code
```

Set the token in the shell launching Claude Code without copying it into a file:

```bash
export TORANA_MCP_TOKEN="$(torana mcp token)"
```

This environment variable is also visible to Claude's Bash tool. It is not
private from an agent with local shell access; see the boundary in
[SECURITY.md](../SECURITY.md).

For manual setup, add these entries alongside existing hooks in a settings file, using
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

`Stop` returns only an informational `systemMessage` once per pending suggestion,
with durable suppression across restarts. It exposes no codes, plugin-authored
text, or change inputs.
For model suggestions it points to `/model`; other proposals stay in Torana's
Review view or CLI. `PostModelSwitch` records the latest reported model/source;
only `command`, `picker` and `sdk` accept matching model advice through the
adapter. Automatic fallback and resume are observations, not acceptance. It
cannot accept a Torana operation or mutate configuration.

Until a conversation receives its first `PostModelSwitch` event, Torana keeps
the request-model fallback for recording accepted advice. Enabling the flag
alone, or installing only Stop, does not disable that fallback. After a real
switch event, the adapter owns provenance for that conversation.

Matching is exact: use the model name Claude reports as the first alias of each
decision-router ladder step. A different alias or full model ID will not count
as accepting the advice. Torana does not guess equivalence between custom models.

The host never reads `transcript_path` or `cwd`. Identity is the same hashed
Claude session identity used by routed requests. The endpoints require the
loopback/origin guard, explicit local-request header and current bearer token.

## Optional switch-cost warning

Also set `suggestions.claude_code.pre_model_switch` to `true` and add a
`PreModelSwitch` HTTP hook with the same headers and a **1-second timeout**,
using `http://127.0.0.1:8080/_torana/hooks/claude-code/pre-model-switch`.
This is a separate opt-in, not installed by default.

To install it through the CLI instead, use
`torana harness hooks setup claude-code --pre-model-switch`. Running setup
without that flag removes a previously owned PreModelSwitch group while keeping
Stop and PostModelSwitch. The preview and prompt call out the timeout risk.

The warning uses Claude's supplied context size and estimated cache-write cost,
without loading suggestions, reading transcripts or calling another model.
Authentication still checks the current durable MCP token. It returns only an
informational `systemMessage`: no allow/deny/ask decision, so Claude's normal
confirmation and noninteractive switch behavior remain unchanged.

Disabled flags, stale tokens, rate limits and malformed or unsupported payloads
silently return `200 {}` for this endpoint. Only authenticated, valid events
show the warning; Stop and PostModelSwitch retain strict errors. Loopback and
origin protections still apply. Manual hook entries must include
`X-Torana-Local-Request: 1` and use a loopback URL/Host. Missing the header or
failing the loopback/origin guard can still fail the hook before this handler,
and may block a switch. The CLI-generated settings include the required header.

**Claude blocks a switch if a PreModelSwitch hook times out**, even though
Torana's response never blocks it. Leave this hook out if you don't want Torana
availability to affect switching. Remove the settings entry to disable the
dependency; turning off the Torana flag alone leaves Claude calling the URL.

An opt-in code check exercises real Haiku Stop and resume callbacks:

```bash
TORANA_LIVE_CLAUDE_HOOKS=1 go test ./internal/proxy -run TestLiveClaudeHookStopAndResume -count=1
```

It uses the locally logged-in Claude CLI, a temporary project and private hook
fixture, with tools disabled. Resume must use the saved model: an explicit
`--model` override can prevent the model-restore event. Interactive `/model`
switch acceptance and visibility of the hint in the terminal UI remain separate
walkthrough checks.
See [Claude's hook reference](https://code.claude.com/docs/en/hooks).
