# Managed-key apps — one master key, metered per user

Most app-store adapters are **bring-your-own-key (BYO)**: each user supplies
their own API key at install (`${TOKEN}` headers, `$APP/secrets.json`). That's
perfect when the user has their own account with the partner.

But some partners give **Pilot** one big key and expect Pilot to share it across
all users — Sixtyfour AI is the first. We can't ship that key inside the adapter
(every install would leak it). The **managed-key broker** solves this: Pilot
holds the one master key centrally, and every call is metered to the user who
made it.

Set `backend.auth: managed` in `pilot.app.yaml`. Everything else about
publishing stays identical.

## Explain it like I'm five

Imagine a candy shop (the partner API) that gave Pilot **one golden ticket**
(the master key) that works forever. We want every kid (user) to get candy, but:

- We can't photocopy the golden ticket and hand one to each kid — they'd all run
  off with it.
- We need to know **which kid** took how much candy, so the bill is fair.

So Pilot builds a **counter** (the broker). The golden ticket stays behind the
counter. Each kid wears a **wristband only they can sign** (their ed25519
identity). To get candy, a kid signs a slip ("it's me, I want candy"), hands it
across the counter, and the counter:

1. **checks the wristband signature** — is this really that kid? (no faking)
2. **checks the menu** — is this candy on the allowed list? (no sneaking into the
   back room)
3. **checks their tab** — are they under their limit? (no one empties the shop)
4. **uses the golden ticket itself** to get the candy, and
5. **writes the cost on that kid's tab.**

The kid never sees the golden ticket. The counter never lets an unsigned slip
through. That's the whole system.

## The shape

```
  ┌─────────────┐   signed request    ┌──────────────────────────┐   master key   ┌────────────┐
  │   adapter   │ ──────────────────► │         broker           │ ─────────────► │  partner   │
  │ (on a user  │  X-Pilot-Caller     │  verify → allow → quota  │  x-api-key     │   API      │
  │   host,     │  X-Pilot-Timestamp  │  → inject key → meter    │ ◄───────────── │ (sixtyfour)│
  │  keyless)   │ ◄────────────────── │                          │   response     └────────────┘
  └─────────────┘   JSON response     └──────────────────────────┘
       ▲ signs with the per-app                  │ holds SIXTYFOUR_MASTER_KEY,
       │ ed25519 identity the daemon             │ one durable usage row per (app, caller)
       │ provisions (--identity)                 ▼
                                          /gw/usage  → per-(app,caller) calls + cents
```

This is the **Pilot service-agent pattern** (a host fronts a capability and the
identity of each caller is authenticated) realized over signed HTTPS instead of
the overlay, so the broker is a plain, deployable web service that holds the key.

## Why it's safe — the caller can't be spoofed

The prototype trusted an `X-Pilot-Caller` header. Anyone could set it and bill
someone else. The production broker **verifies a signature** instead.

Each request carries three headers:

| Header | Meaning |
|---|---|
| `X-Pilot-Caller` | the caller's ed25519 public key (base64) — their identity |
| `X-Pilot-Timestamp` | unix seconds — bounds replay |
| `X-Pilot-Signature` | ed25519 signature over the canonical request |

The signed bytes (identical in the adapter and the broker) are:

```
METHOD \n PATH \n TIMESTAMP \n base64(sha256(BODY))
```

Binding the method, path, timestamp, and a hash of the body means a captured
signature can't be replayed against a different call, a different app, or a
tampered body. The broker (`internal/broker/identity.go`) re-derives those bytes
and checks the signature against the claimed public key; the adapter
(`internal/scaffold/templates/signer.go.tmpl`) produces them with the per-app
ed25519 key the daemon hands it via `--identity`. A golden test
(`canonical_golden_test.go`) and a template string-match assertion lock the two
copies together so they can't drift.

```go
// broker side — verify (simplified)
caller, err := verify.Verify(r.Header.Get, r.Method, r.URL.Path, body)
if err != nil { http 401 }              // missing / stale / tampered / forged

// adapter side — sign (simplified, generated)
ts  := strconv.FormatInt(time.Now().Unix(), 10)
sig := ed25519.Sign(priv, canonical(method, path, ts, body))
req.Header.Set("X-Pilot-Caller",    base64(pub))
req.Header.Set("X-Pilot-Timestamp", ts)
req.Header.Set("X-Pilot-Signature", base64(sig))
```

## The five gates on every call

`internal/broker/broker.go` runs the same pipeline for every managed app:

1. **Identity** — verify the signature → `401` if missing/stale/forged.
2. **App** — look up the app id in the registry → `404` if unknown.
3. **Allow-list** — the method path must be declared → `403` otherwise. No open
   proxy onto the master key.
4. **Breaker + quota** — if the partner is flapping, fail fast (`503`); else the
   per-caller quota is checked-and-counted atomically → `429` if over.
5. **Forward + meter** — build a *fresh* request (caller headers are never
   carried over), inject the master key, forward, then add the partner-reported
   cost to that caller's tab.

## How an app becomes managed (end to end)

1. **Author** sets `auth: managed` in `pilot.app.yaml`. `pilot-app init`
   generates a **keyless** adapter: it points at `https://broker.pilotprotocol.network/<app-id>`,
   carries no secret, is granted `key.sign`, and signs every request.
2. **Publish** as usual (one repo, same flow). The submission carries
   `backend.auth: managed`.
3. **On approval**, the publish-server derives a broker registry entry from the
   submission (`internal/publish/broker_register.go`) and writes the broker's
   `apps.json` (`BROKER_REGISTRY`). It logs the env var name for the master key
   (e.g. `SIXTYFOUR_MASTER_KEY`).
4. **Ops** sets that env var on the broker and reloads it (`kill -HUP`). The app
   is now live and metered.
5. **A user** installs the app like any other. They bring nothing. Every call is
   verified as them and metered to them.

## The registry (`apps.json`)

One entry per managed app — adding an app is config, not code:

```json
[{
  "id": "io.pilot.sixtyfour",
  "upstream": "https://api.sixtyfour.ai",
  "key_env": "SIXTYFOUR_MASTER_KEY",
  "auth_header": "x-api-key",
  "allow": ["/enrich", "/find-email"],
  "quota": 0,
  "cost_field": "cost_cents",
  "timeout_ms": 60000,
  "breaker_threshold": 5,
  "breaker_cooldown_ms": 30000
}]
```

The master key is **never** in this file — only the name of the env var that
holds it.

## Auth styles

Partners authenticate differently; the broker injects the master key per the
entry's `auth_style` (`internal/broker/inject.go`):

- `header` (default) — `auth_header` + optional `auth_scheme` (`x-api-key: <key>`
  or `Authorization: Bearer <key>`)
- `query` — `auth_param` (`?apikey=<key>`)
- `basic` — `auth_user` (HTTP Basic; key-as-username by default)

## Operating the broker

```bash
# durable usage store + one master key per app:
BROKER_DB=/data/usage.db SIXTYFOUR_MASTER_KEY=sk-... \
  broker -registry /registry/apps.json -addr :8099

curl localhost:8099/gw/health    # liveness
curl localhost:8099/gw/usage     # per-(app,caller) calls + cents
kill -HUP <pid>                  # reload the registry with no downtime
```

See [`deploy/docker`](../deploy/docker) for the prod-like local stack and
[`scripts/e2e-broker.sh`](../scripts/e2e-broker.sh) for a real-process,
multi-user end-to-end test.

## What's deliberately simple (and what's deferred)

See [`CAVEATS.md`](../CAVEATS.md) for the honest list — durable store scaling,
rate-limiting vs. quota, the daemon identity-file contract, and per-method (vs.
per-app) timeouts.
