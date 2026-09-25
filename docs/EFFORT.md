# Let a plugin suggest reasoning effort

Torana can translate a plugin's portable effort choice into the native setting of the model endpoint you configured. This is optional. With the default settings, or when a plugin does not request an effort level, your harness's own setting passes through unchanged.

An operator enables this in two steps:

1. Declare the levels a particular model accepts under that provider's `models` entry. Torana uses an **exact model name**; it does not guess capabilities from a provider name or a wildcard.
2. Set `effort.enabled` to `true` in the running configuration and approve `env.route_request.effort` for a plugin you want to manage effort. Ordinary `env.route_request` approval alone does not grant this.

Export the configuration with `torana config get > settings.json`, edit the `config` object, then apply with `torana config apply --file settings.json --yes`. For example, the relevant settings for one Anthropic model could look like:

```json
{
  "effort": {"enabled": true},
  "providers": {
    "anthropic": {
      "models": {
        "YOUR_EXACT_MODEL_ID": {
          "effort": {"levels": ["low", "medium", "high"]}
        }
      }
    }
  }
}
```

This is a fragment to merge into your exported settings, not a complete configuration. Keep the provider URL, authentication, and other existing fields. For Gemini models, also map each declared portable level to that model's native `thinking_level` or `thinking_budget` under `effort.gemini`; different model generations can use different controls.

When a permitted plugin asks for a level, Torana chooses the nearest level in that model's declaration (preferring the lower one on a tie) and writes it into the upstream API shape. The same plugin can work with Anthropic, OpenAI Chat, OpenAI Responses, Gemini, and Gemini Code Assist endpoints without constructing provider-specific request fields itself. If the model has no usable declaration, Torana leaves the request's effort setting alone.

After the response, the plugin can inspect `_route_applied` in the response metadata. Its `effort` is the selected portable level, and `effort_status` is `applied`, `clamped`, `omitted`, or `unchanged`. That outcome is also available for completed streams and upstream errors, where the after-response hook is observational and has no assistant message to rewrite. The same metadata names the route the plugin chose, any refusal, and the provider/model that actually served the request after failover.

See [running plugins](PLUGINS.md) for reviewing permissions and [protocol bridges](PROTOCOL_BRIDGES.md) for cross-shape routing.
