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

`agy`'s gRPC is local-only (CLI ⇄ an internal language server); its Google-facing
traffic is plain HTTPS + SSE to `cloudcode-pa.googleapis.com`. The stripped Go
binary ignores endpoint env vars but **honors `HTTPS_PROXY` and a custom CA via
`SSL_CERT_FILE`**, with no cert pinning. So Torana terminates TLS for the Code
Assist hosts, routes chat calls through the plugin pipeline, and tunnels
everything else (login, telemetry) untouched. `agy`'s own Google OAuth bearer is
forwarded upstream — **Torana injects no auth**.

### 1. Configure

```json
{
  "providers": {
    "antigravity":       { "url": "https://cloudcode-pa.googleapis.com",       "format": "gemini-codeassist" },
    "antigravity-daily": { "url": "https://daily-cloudcode-pa.googleapis.com", "format": "gemini-codeassist" }
  },
  "mitm": {
    "enabled": true,
    "listen": "127.0.0.1:8099",
    "ca_dir": "./local/mitm",
    "hosts": {
      "cloudcode-pa.googleapis.com":       "antigravity",
      "daily-cloudcode-pa.googleapis.com": "antigravity-daily"
    }
  },
  "plugins": {
    "dir": "./plugins",
    "order": ["intent", "keyword_compactor"],
    "config": {
      "keyword_compactor": {
        "tool_policies": [
          {"match": "read*", "mode": "exact"},
          {"match": "grep*", "mode": "keyword"}
        ]
      }
    }
  }
}
```

### 2. Start Torana

```bash
TORANA_BIND=127.0.0.1 TORANA_CONFIG=config.json ./torana
```

On first boot it generates the CA and prints exactly how to point the client:

```
mitm: CA ready at ./local/mitm — point the client at HTTPS_PROXY=http://127.0.0.1:8099 SSL_CERT_FILE=./local/mitm/bundle.pem
mitm: CONNECT proxy on 127.0.0.1:8099; intercepting 2 host(s)
```

`bundle.pem` = the system CA roots **plus** Torana's CA, so `agy` validates
Torana's leaf for the intercepted hosts *and* the real Google certs for the
tunneled ones.

### 3. Point `agy` at it

```bash
export HTTPS_PROXY=http://127.0.0.1:8099
export SSL_CERT_FILE=/abs/path/to/local/mitm/bundle.pem
# some builds also read: export NODE_EXTRA_CA_CERTS=$SSL_CERT_FILE

agy                                                   # interactive
agy --mode=plan --print "Summarize this repo's architecture"   # headless
```

> `--print` takes the prompt as its **value** — put the prompt immediately after
> it (`agy --mode=plan --print "…"`), not `agy --print --mode …` (that consumes
> `--mode` as the prompt).

### 4. Verify

```bash
curl -s localhost:8080/stats   # { compactions, bytes_saved, total_tokens_in, … }
```

The proxy log shows the routing and clean upstream status:

```
mitm: routed daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent via /provider/antigravity-daily
Upstream returned 200
```

### How it works

```
[agy] --HTTPS_PROXY--> [Torana MITM :8099] --TLS terminate--> classify path:
        chat  (:streamGenerateContent / :generateContent) → /provider/antigravity* → plugin pipeline → real Google
        other (loadCodeAssist, oauth2, telemetry, …)       → opaque TLS tunnel                        → real Google
```

### Notes & gotchas (from dogfooding)

- **Only the `cloudcode-pa` hosts are decrypted.** OAuth token exchange
  (`oauth2.googleapis.com`), login, and telemetry are opaquely tunneled — Torana
  never sees your credential exchange.
- **The CA private key stays in `ca_dir`** (gitignored). It's trusted only by
  `agy` via `SSL_CERT_FILE`. **Never** add it to the system trust store or commit
  it.
- **Two release channels.** `agy` may call `daily-cloudcode-pa` (dev build) or
  `cloudcode-pa` (prod). Map both in `mitm.hosts`.
- **Auth is `agy`'s own Google OAuth session** — Torana forwards the bearer and
  injects nothing. Running many rapid *automated* `agy` sessions through a proxy
  can trip Google's re-auth (a security response); if `agy` asks you to log in
  again, just re-run its sign-in flow.
- **Keep `listen` on localhost** — the ingress decrypts caller traffic.
- **Intent + compaction:** `agy`'s tool calls already carry a goal-tied intent.
  Keep `intent` before one compactor and configure explicit policies; source
  reads remain exact for three later assistant turns and unmatched tools remain
  exact. See [COMPACTION.md](COMPACTION.md).
