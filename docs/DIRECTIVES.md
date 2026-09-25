# Torana commands in a conversation

You can manage a pending suggestion without leaving a chat. Put a Torana command at the start of a new user message, on its own line:

```text
torana> status
torana> accept <code>
torana> dismiss <code>
torana> help
```

These commands are off by default. To enable them, export the running settings, set `directives.enabled` to `true` inside the `config` object, and apply the edited snapshot:

```bash
torana config get > settings.json
# Edit settings.json: set config.directives.enabled to true.
torana config apply --file settings.json --yes
```

Each command belongs to the current conversation. `status` lists its pending suggestions; `accept` and `dismiss` use the four-character code shown with a suggestion. A code can be used once and cannot act on another conversation. `help` shows the available commands.

Send a command **by itself**. Torana answers it locally without calling your model. If you mix a command with an ordinary request, Torana sends neither; it asks you to send the command alone, then resend the other text. This avoids accidentally losing part of your request.

Torana removes command lines and its own signed local replies from later conversation history before forwarding it. A line beginning exactly `torana> ` is treated as a command even when Torana does not recognize the verb; put examples inside a fenced code block if you want the model to read them as ordinary text.

You can always manage suggestions with the [CLI](SUGGESTIONS.md) instead.
