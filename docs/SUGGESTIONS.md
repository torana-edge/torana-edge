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

Accepting records your choice. It does not automatically change your harness's model. If the suggestion names another model, switch models in the harness yourself; Torana can then recognize that switch for the conversation. Codes and IDs are single-use for accepting or dismissing a pending suggestion.

Some harnesses can also show a short, signed Torana notice at the end of a completed assistant reply. This display channel is opt-in for specific verified harness identity sources. It is never added to tool-calling or incomplete replies, and Torana removes its own notice from later requests before a plugin or model sees the history. If your harness is not allowlisted, use the CLI to see suggestions; the suggestion itself is still available there.

See [Torana commands](DIRECTIVES.md) if you want to accept or dismiss a suggestion from inside a supported conversation.
