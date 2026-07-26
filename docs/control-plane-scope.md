# Torana Control Plane — Scope

_Source of truth for the management-layer work. Supersedes prior ad-hoc plans._

## What Torana is (the engine — already built, not in scope here)
A proxy that sits between an AI client (Claude Code, Antigravity, …) and AI
providers (Anthropic, OpenAI, DeepSeek, …). Traffic flows through a **pipeline of
plugins** (compaction, PII, intent, telemetry, …) plus built-ins (cache, rate
limits, failover, offload). The engine works. This document is only about **how a
person operates it** — the management layer / web control plane at `/_torana/`.

## The 4 jobs of the management layer
1. **Configure** the engine without editing files (providers, plugins, global knobs).
2. **Observe** it (live request feed, traffic/cost stats).
3. **Operate safely** (who can access, how secrets are handled, how changes apply).
4. **Extend** it (installing plugins; each plugin's own settings screen).

## Two products, phased
Customers range from a solo dev on a laptop to an enterprise team. Rather than
half-build both, we phase:

- **Phase 1 — the local single-operator tool.** One person, on their machine,
  localhost-only, no login, secrets stay in the environment. Every customer needs
  this on day one. **This is the current scope.**
- **Phase 2 — the shared/hosted service.** Remote access, user accounts, roles,
  centrally-stored encrypted secrets, audit logs, plugin registry. Explicitly
  **deferred** (listed at the bottom so nothing is lost).

---

## PHASE 1 — Definition of Done
Goal: a solo dev can run Torana and do **everything** through the dashboard —
never opening a config file.

### 1. Configure (no file editing)
- **Providers**: add / edit / remove upstreams (URL, wire format, failover order).
  API keys are referenced by **environment-variable name**, never typed as raw
  secrets (see Safety). — _mostly done today._
- **Plugins**:
  - Enable / disable / reorder (drag). — _done today._
  - Configure each plugin through a **real form**, not a raw-JSON box. A plugin
    declares its settings; the dashboard renders the form. — _to build._
  - Install = drop a plugin into the plugins folder; it appears as "available."
    — _works today._
- **Global knobs**: rate limits, offload, cache backend, listen port — all editable
  in Settings. — _rate limits/offload done; cache/port to add._

### 2. Observe
- Live request feed. — _done._
- Traffic stats: tokens, cache hit-rate, latency, wire bytes, per-plugin savings.
  — _done._ (FinOps framed as a plugin concern, not a core Torana feature.)

### 3. Operate safely (local edition)
- **Access**: localhost-only, no login. It's your machine. — _done (guard exists)._
- **Secrets**: raw API keys **are editable in the UI**. They are encrypted at rest
  with a local machine key (AES-GCM, `0600` key file) — never stored as plaintext,
  never echoed back to the UI, never logged. An env-var reference remains supported
  as an alternative. — _to build (`internal/secret`)._
- **Applying changes**: **everything applies live — no service restart.** Providers,
  limits, offload and plugins already hot-reload; the boot-time blocks (**cache, port,
  MITM**) are reconfigured in-process via drain-and-swap. The UI shows a brief
  "applying…" state, never a restart. — _to build (live reconfig)._

### 4. Extend (plugin config UX)
- **Default**: a plugin declares a small settings schema (field, type, label,
  default, options); the dashboard auto-renders a styled form. No per-plugin web
  code needed. — _to build._
- **Override**: a plugin that needs something richer (e.g. a telemetry dashboard)
  can serve its own page, embedded in the dashboard. — _mechanism exists (otel)._
- **Fallback**: the raw-JSON editor survives only as an "Advanced" option for a
  plugin that declares no schema and serves no page. — _to demote._

### Explicitly NOT in Phase 1
Multi-user, login/auth, remote access, roles/permissions, a **shared/central** secret
service, audit logs, plugin registry/marketplace, and live cache-backend swap **without**
the brief in-process drain. All of these are Phase 2. (Local encrypted secret storage and
live reconfig-with-drain **are** in Phase 1.)

---

## Current status vs. Phase 1 (the delta to close)
**Already working:** dashboard shell, live feed, traffic stats, provider settings,
rate-limit/offload settings, plugin enable/disable/reorder, localhost guard, new logo.

**Left to build for Phase 1:**
1. **Per-plugin config forms** — schema field on plugins + host-rendered forms;
   demote the raw-JSON box to "Advanced"; add a per-plugin save path.
2. **Boot-time settings in the UI** — add cache / port / MITM to Settings, with a
   persist-then-self-restart flow.
3. **Polish for "customer-ready"** — validation, empty/error states, a clear
   "needs restart" indicator, and confirming the config file round-trips cleanly.

## Phase 2 — parked (captured, not forgotten)
Remote + multi-user access, accounts & login, roles/permissions, encrypted
central secret store, audit logging, plugin registry/marketplace, live cache-backend
swap, and any hosting/SaaS concerns.
