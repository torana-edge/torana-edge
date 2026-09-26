# Suggestions from plugins

A plugin can notice something useful about a conversation and suggest a next step without silently changing your setup. For example, a routing plugin might suggest a different model when a task gets stuck. The suggestion is advice: you choose whether to act on it.

Suggestions are off by default. To turn them on, export the running configuration, set `suggestions.enabled` to `true` inside its `config` object, and apply the edited snapshot:

```bash
torana config get > settings.json
# Edit settings.json: set config.suggestions.enabled to true.
torana config apply --file settings.json --yes
```

The revision in `settings.json` protects you from overwriting a newer settings change. If the apply reports a conflict, export a fresh snapshot and reapply your edit.

## See what a plugin suggested

Use `torana conversations` to find a recent conversation ID, then:

```bash
torana suggestions list --conversation <conversation-id>
torana suggestions show <suggestion-id> --conversation <conversation-id>
```

The list includes each suggestion's reason and status. A suggestion is tied to its conversation; an ID from another conversation cannot be accepted here.

To act on one, use its ID:

```bash
torana suggestions accept <suggestion-id> --conversation <conversation-id> --yes
# Or leave the current setup as it is:
torana suggestions dismiss <suggestion-id> --conversation <conversation-id> --yes
```

For a confirmed built-in plugin change, accepting applies the change and returns its execution result. A model-switch suggestion does not automatically change your harness's model: switch models in the harness yourself, and Torana can recognize that switch. Suggestion IDs are single-use for accepting or dismissing a pending suggestion.

Some harnesses can also show a short, signed Torana notice at the end of a completed assistant reply. This display channel is opt-in for specific verified harness identity sources. It is never added to tool-calling or incomplete replies, and Torana removes its own notice from later requests before a plugin or model sees the history. If your harness is not allowlisted, use the CLI to see suggestions; the suggestion itself is still available there.

The display settings live under `config.suggestions.notice.harnesses`, keyed by the named harness identity source Torana recognized (for example, `claude-code-session` or `codex-thread`). A generic thread header or content-derived identity cannot enable notices. Leave the map empty unless you have checked that the harness preserves and replays the signed markers. For that check, `config.suggestions.notice.probe: true` appends a fixed, non-actionable test notice on completed replies for allowlisted sources. Turn the probe off after testing; it does not create a suggestion or accept code. A strip failure disables later notices for that conversation, while the CLI remains available.

Connect Torana's MCP server to let your harness discover and request operations. Pending confirmations remain available in Torana's UI and CLI. Notices are informational: they never contain confirmation codes or chat commands.

## Undo a confirmed plugin change

List the conversation's change history, then use the change ID to restore the prior plugin setup:

```bash
torana changes list --conversation <conversation-id>
torana changes undo <change-id> --conversation <conversation-id> --yes
```

Undo checks that the configuration and installed plugin still match the recorded change. If either has changed, review the current setup instead of overwriting newer work. A change can only be undone once.

## Upgrading older settings

The former in-chat command channel has been removed; use MCP or the CLI. Legacy `directives`, `assistant`, and `harness.setup_from_directive` settings are ignored when loading an older configuration. Existing signed Responses reply IDs are decoded for one release so resumed conversations can continue.
