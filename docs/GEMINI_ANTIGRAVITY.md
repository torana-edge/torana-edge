# Gemini & the Antigravity CLI

Torana fronts Google's Gemini models two ways, depending on whether the tool can
be pointed at a base URL. Both speak the same Gemini `generateContent` content
model; they differ only in the request envelope and SSE framing, which the format
handles automatically.

| Format | Endpoint | How you connect |
|---|---|---|
| `gemini` | Public Gemini API / Vertex AI | Base URL + API key (like the openai providers) |
| `gemini-codeassist` | Code Assist API behind the Antigravity CLI (`agy`) | TLS-terminating MITM ingress (agy can't take a base URL) |

---

## A. Base-URL Gemini tools (public Gemini API / Vertex AI) — no MITM

Any tool that lets you set a base URL + API key (Cline, Aider, Continue, Zed,
opencode, …) is the easy case — identical to the openai/DeepSeek flow.

```json
{
  "providers": {
    "gemini": {
      "url": "https://generativelanguage.googleapis.com",
      "format": "gemini"
    }
  }
}
```

Point the tool at `http://localhost:8080/provider/gemini` and send
`GenerateContent` requests (e.g. `/v1beta/models/<model>:streamGenerateContent`).
The caller's API key is forwarded upstream; no MITM is involved.

---

## B. Antigravity CLI (`agy`) — via the MITM ingress

In the tested CLI, `agy`'s gRPC is local-only (CLI ⇄ an internal language server);
its Google-facing traffic is HTTPS + SSE to the Code Assist hosts below.
It **honors `HTTPS_PROXY` and a custom CA via `SSL_CERT_FILE`**. Torana terminates TLS for the Code
Assist hosts and routes recognized inference calls through the plugin pipeline.
Other paths on those same hosts are decrypted and forwarded as ordinary HTTP;
only unmapped hosts remain opaque tunnels. `agy`'s own Google OAuth bearer is
forwarded upstream — **Torana injects no auth**.

### 1. Configure

Already signed into `agy`? Keep that login. This example starts a separate
Torana evaluation instance from a new data directory. Save the following as
`config.json` in your Torana checkout. If you already run Torana on port 8080,
stop that instance first or choose another main port with `start --port 8081`; the TLS
proxy below separately uses port 8099. Do not overwrite an existing config.

```json
{
  "providers": {
    "antigravity": {
      "url": "https://cloudcode-pa.googleapis.com",
      "format": "gemini-codeassist",
      "auth": {"mode": "caller"}
    },
    "antigravity-daily": {
      "url": "https://daily-cloudcode-pa.googleapis.com",
      "format": "gemini-codeassist",
      "auth": {"mode": "caller"}
    }
  },
  "mitm": {
    "enabled": true,
    "listen": "127.0.0.1:8099",
    "ca_dir": "./local/mitm",
    "hosts": {
      "cloudcode-pa.googleapis.com": "antigravity",
      "daily-cloudcode-pa.googleapis.com": "antigravity-daily"
    }
  }
}
```

### 2. Start Torana

```bash
export TORANA_DATA_DIR="$PWD/local/agy-data"
TORANA_CONFIG=config.json ./torana start --bind 127.0.0.1
./torana status
```

Use a fresh name under the gitignored `local/` directory if `local/agy-data`
already exists. The seed is
imported on first start; editing it later does not update managed settings.
Keep the same `TORANA_DATA_DIR` for subsequent status, feed, and stop commands.

On first boot Torana generates the CA. The server log (path shown by `status`)
reports the bundle location and listener:

```
mitm: CA ready at ./local/mitm — point the client at HTTPS_PROXY=http://127.0.0.1:8099 SSL_CERT_FILE=./local/mitm/bundle.pem
mitm: CONNECT proxy on 127.0.0.1:8099; intercepting 2 host(s)
```

`bundle.pem` = the system CA roots **plus** Torana's CA, so `agy` validates
Torana's leaf for the intercepted hosts *and* the real Google certs for the
tunneled ones.

### 3. Point `agy` at it

For interactive use, apply routing only to this launch:

```bash
HTTPS_PROXY=http://127.0.0.1:8099 \
SSL_CERT_FILE=/absolute/path/to/local/mitm/bundle.pem \
agy
```

Replace the certificate path with the actual bundle reported by Torana. There
is no system trust-store change; a later plain `agy` launch uses its normal route.

For a single headless task, run from a directory containing a small,
non-sensitive `hello.txt`:

```bash
HTTPS_PROXY=http://127.0.0.1:8099 \
SSL_CERT_FILE=/absolute/path/to/local/mitm/bundle.pem \
agy -p 'Read ./hello.txt and reply with its contents.' \
  --model gemini-3.8-flash-high \
  --output-format stream-json --print-timeout=120s
```

Choose a model available to your account. `-p` takes the prompt immediately
after it. Start interactively to approve tool actions, or configure scoped
permissions for automation using the [headless guide](https://antigravity.google/docs/cli/headless/).

<details>
<summary>The headless task finishes without reading the file?</summary>

Check the streamed tool events for a permission denial. Antigravity can report
`SUCCESS` after explaining that it couldn't run a tool, so check the answer too.
Approve the needed action interactively or adjust the scoped permissions and retry.

Our isolated verification used `--dangerously-skip-permissions`. That flag
auto-approves tools, including shell commands; use it only when you intend
that access. Routing through Torana itself does not require it.

</details>

### 4. Verify

```bash
./torana status --json
./torana feed
```

The proxy log shows the routing and clean upstream status:

```
mitm: routed daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent via /provider/antigravity-daily
Upstream returned 200
```

Confirm the expected file contents in the final answer and completed file-read
tool events. Then install, approve, and enable
[usage_logger](https://github.com/torana-edge/torana-plugins/blob/main/plugins/usage_logger/README.md)
through the local UI, repeat the task, and inspect its local records. The
plugin's guide includes the CLI alternative and shell-native file reading.

This workflow was verified with Antigravity CLI 1.2.4 and Gemini 3.8 Flash High
on September 16, 2026: file read, tool-result follow-up, final answer, and local
usage records. Tool actions were explicitly auto-approved for that isolated run.

Try it with your own workflow. If you hit a rough edge,
[share the steps](https://github.com/torana-edge/torana-edge/issues) so we can
improve the integration. Keep credentials and private prompts out of reports.

When finished, exit the harness and run `./torana stop --yes` from the shell
with the same `TORANA_DATA_DIR`. Keep the evaluation state and CA private.

### How it works

1. The CONNECT host selects interception: an unmapped host uses an opaque TLS
   tunnel; a host listed in `mitm.hosts` is decrypted with the local CA.
2. On a mapped host, recognized inference paths enter the plugin pipeline.
3. Other paths on that mapped host are decrypted and forwarded as ordinary
   HTTP. They are **not** opaque tunnels merely because they are non-chat calls.

### What the listener is, stated plainly

The MITM ingress is a **CONNECT forward proxy for the local machine**. Hosts in
`mitm.hosts` are decrypted, with inference paths routed through the pipeline;
every other CONNECT
is tunnelled to whatever address it names, which is how the non-decrypted
traffic above reaches Google. That also means any local process pointed at
`HTTPS_PROXY` can reach any host through it, not only Google's.

It binds a literal loopback address and refuses anything else
(`MITMConfig.ValidateIngress`), so this is reachable only from the machine it
runs on — but it is a real surface and worth knowing before enabling the
ingress. Leave `mitm.enabled` off unless you are actually using a harness that
needs it; it is off by default.

Two lifetimes to be aware of:

- **Leaf certificates live 24 hours** and are re-minted automatically an hour
  before expiry, so a long-running proxy keeps working.
- **The generated CA lives one year** and has no automatic renewal. When it
  expires, Torana refuses to start the ingress and names the two files to
  delete; clients trusting the old CA must then trust the newly written bundle.

### Client setup and privacy boundaries

- **Only configured hosts are decrypted.** With the example host map,
  `oauth2.googleapis.com` is not intercepted. Adding a host to `mitm.hosts`
  makes all its HTTPS paths visible to Torana, not only inference calls.
- **The CA private key stays in `ca_dir`** (gitignored). Torana sets that
  directory to `0700` and the key to `0600` on Unix, refusing over-permissive
  key files there. On every platform it refuses partial, malformed, mismatched,
  symlinked, or expired CA material instead of silently rotating it. It's
  trusted only by `agy` via `SSL_CERT_FILE`. **Never** add it to the system trust
  store or commit it.
- **Two release channels.** `agy` may call `daily-cloudcode-pa` (dev build) or
  `cloudcode-pa` (prod). Map both in `mitm.hosts`.
- **Auth is `agy`'s own Google OAuth session** — Torana forwards the bearer and
  injects nothing. Running many rapid *automated* `agy` sessions through a proxy
  can trip Google's re-auth (a security response); if `agy` asks you to log in
  again, just re-run its sign-in flow.
- **`listen` must be a literal loopback address** (`127.0.0.1` or `::1`) — the
  ingress decrypts caller traffic, so Torana refuses wildcard, LAN, and
  hostname-based listeners. Configured host matching is DNS-case-insensitive;
  the TLS server name must match the CONNECT authority.
- **Plugins are optional.** Verify routing first. Then follow an individual
  [plugin setup guide](https://github.com/torana-edge/torana-plugins#choose-a-plugin)
  to configure, approve and enable a transformation.

## Code Assist provider-extension envelope (mandatory grammar)

The Code Assist wrapper and inner-request fields are REAL provider fields:
they live in `provider_extensions_json` (the normative "unparsed provider
fields" contract), NOT in hidden host state. The ABI object holds the
DELIBERATE two-scope envelope, which IS the original wire object with the
canonical ABI-owned members projected out:

- **envelope top level = outer-wrapper extras** (their exact wire
  position); the top-level `request` member is the STRUCTURAL container
  for the inner-request extras;
- **outer `model` is FORBIDDEN as an extra** — rebuilt from
  `ChatRequest.model` (canonical wins);
- **inner `systemInstruction`, `contents`, `tools`, `safetySettings` are
  FORBIDDEN as extras** — rebuilt from the canonical ABI fields;
- **inner `generationConfig` may exist ONLY as an object containing
  UNKNOWN sibling members**; its canonical members (`maxOutputTokens`,
  `temperature`, `topP`, `stopSequences`) are FORBIDDEN in the preserved
  extra object and rebuilt from the canonical ABI fields;
- unknown outer, inner, and generation siblings remain LOSSLESS
  (deterministic span operations preserve member order, whitespace,
  numeric lexemes, escapes, and nested bytes);
- absence vs empty-object is explicit and deterministic: an ABSENT
  envelope defaults to the empty envelope; a PRESENT envelope missing the
  structural `request` member is REFUSED; for `generationConfig`, input
  projection REMOVES the member when no unknown sibling remains after the
  canonical deletion (canonical-only input never leaks a derived `{}`),
  while an EXPLICITLY EMPTY wire object is a plugin/provider-authored fact
  and is preserved as `{}`.

**Variant ownership**: the Code-Assist-vs-bare fact is typed host-only
topology (`ChatRequest.CodeAssist`), never in the ABI; a plugin can
neither forge nor lose it (restored at the pipeline boundary).

**Grant ownership**: the envelope is inspectable and replaceable by a
plugin holding `ir.params.write`; the canonical members are NOT extras —
a replacement smuggling any canonical member through the extras path is
**plugin-output invalidity**, attributed to that exact plugin: `pass`
rolls back to the accepted input, `block` produces the plugin refusal.
The adapter marshal additionally refuses such an envelope defensively
(never silently ignored).

**Failure behavior**: malformed envelope shapes (`request`/`
generationConfig` null, arrays, scalars, malformed text) are classified
errors with no panic.

See the canonical plugin-author guidance (`PLUGINS.md`) for the
grant model; the Edge host owns this per-format grammar.
