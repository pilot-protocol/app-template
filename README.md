# pilot-app — publish an existing API or CLI on the Pilot app store

`pilot-app` turns a declarative `pilot.app.yaml` into a complete, signed,
publishable Pilot Protocol app-store adapter — the same shape as the
hand-written reference app `io.pilot.cosift`, generated in seconds.

You bring an existing HTTP API (or a CLI); you describe its methods in one YAML
file; `pilot-app` emits a buildable Go adapter, a `manifest.json` with the right
grants, a `Makefile` that builds → sha256-pins → signs → packages, and the exact
release + catalogue-PR steps. No hand-written Go, no IPC boilerplate.

## Why an adapter at all

A Pilot app is a **thin, stateless adapter** + a **signed manifest**. The daemon
fetches the bundle from the catalogue, re-verifies its signature and binary
sha256 on every spawn, hands it a unix socket, and brokers JSON-in/JSON-out IPC
calls. The heavy backend (your API) lives wherever it already lives; the adapter
just forwards each IPC method to it. That forwarding layer is ~99% boilerplate —
which is exactly what this generates.

## Quickstart

```bash
go build -o pilot-app ./cmd/pilot-app

go install github.com/pilot-protocol/app-template/cmd/pilot-app@latest

pilot-app example > pilot.app.yaml      # starter spec, fully annotated
$EDITOR pilot.app.yaml                  # point it at your API, list your methods
pilot-app validate                      # catch spec errors early
pilot-app init -o ./my-app              # scaffold the adapter project

cd my-app
make gen-key                            # one-time ed25519 publisher key (gitignored)
make package                            # build -> pin -> sign -> tarball
pilot-app verify io.pilot.<id>-<ver>.tar.gz   # optional: run the gate locally

# fork this repo, then publish through the single front door:
pilot-app submit -C . --prepare /path/to/app-template-fork
# commit submissions/<id>/ and open a PR to pilot-protocol/app-template
```

CI verifies the bundle and a maintainer reviews; on merge, automation releases it
on `pilot-protocol/catalog` and opens the `catalogue.json` data entry on
`TeoSlayer/pilotprotocol`. Then anyone can `pilotctl appstore install <your.id>`.
You only ever touch **one repo** (`app-template`); see
[`submissions/README.md`](submissions/README.md) and
[`docs/APP-PUBLISHING-SPEC.md`](docs/APP-PUBLISHING-SPEC.md).

> **New to publishing?** Start with [`docs/PUBLISHING.md`](docs/PUBLISHING.md) —
> the field-level **required-vs-optional** reference, the two publish paths and
> why they're at parity, and a `cli`-app worked example.

### Two ways to publish — same required fields, same result

You can publish either way; the inputs and the output are identical:

1. **The website form** at `pilotprotocol.network/publish`, which POSTs the rich
   `Submission` JSON to the **publish-api** ([`cmd/publish-server`](cmd/publish-server)).
   The server scaffolds + cross-compiles + signs the adapter for you.
2. **A PR to this repo** with a `submissions/<id>/` produced by
   `pilot-app submit --prepare` — `pilot-app` scaffolds + builds the same bundle
   on your machine, and you commit the signed bundle + a small pointer
   `submission.json`.

Both run the **same scaffold pipeline** and the **same validation**; the only
difference is *where* the build runs (our server vs. your laptop). Field-by-field
parity is in [`docs/PUBLISHING.md`](docs/PUBLISHING.md).

**Two rules that trip people up:**

- **The adapter is scaffolded by the pipeline — never hand-built.** Do not write
  your own adapter and submit a pre-built binary as the bundle; the pipeline
  generates the adapter Go, manifest, and binaries from your spec.
- **Binaries must be the full per-platform set or a true universal binary —
  never single-platform.** Every app cross-compiles to
  `darwin × linux × arm64 × amd64` automatically; a lone `linux/amd64` bundle is a
  build-host accident that refuses to spawn elsewhere.

## The spec (`pilot.app.yaml`)

```yaml
id: io.pilot.weather
app_version: 0.1.0
description: "Current conditions and forecasts."
backend:
  type: http
  base_url: https://weather.example.com   # baked in as the default; no config needed
methods:
  - name: weather.current
    summary: "Current conditions for a lat/lon."
    duration: fast                         # fast|med|slow -> per-call timeout + help class
    http: {verb: GET, path: /current}      # GET: payload -> query string
    params: {lat: "string (required)", lon: "string (required)"}
  - name: weather.report
    summary: "Synthesized briefing."
    duration: slow
    http: {verb: POST, path: /report}      # POST: payload -> JSON body
```

`pilot-app example` prints the full annotated version, including the `cli`
backend form. Run `pilot-app validate` for fast feedback.

## What gets generated

```
my-app/
  cmd/<binary>/main.go        # six lifecycle flags, socket serve loop, dispatcher,
                              # per-method handlers, config resolution, <ns>.help
  internal/backend/client.go  # tuned HTTP client (http backend)
  internal/backend/exec.go    # subprocess runner   (cli backend)
  manifest.json               # id, exposes (every method + <ns>.help), grants, store
  Makefile                    # gen-key / bundle / package / verify / publish-help
  README.md  go.mod  .gitignore
```

The generated adapter has **zero dependencies** beyond the pinned
`app-store/pkg/ipc`, ships pointing at production (no config step for users),
and auto-exposes a `<namespace>.help` discovery method describing every method's
params, kind, and latency class.

## Backends

- **`http`** — works on the platform today. Maps each method to a backend HTTP
  endpoint (GET → query string, POST → JSON body). This is what the reference
  app `io.pilot.cosift` does, by hand; `pilot-app` generates the equivalent.
- **`cli`** — generates a working subprocess adapter (curated routes and/or a
  passthrough that fronts the whole tool). Provide `backend.command` + `methods` +
  the CLI binary as `assets[]`; the pipeline scaffolds and cross-compiles the
  adapter. Installing it through the catalogue needs a `proc.exec` capability the
  platform is rolling out — see [docs/CLI-ADAPTER.md](docs/CLI-ADAPTER.md) and the
  worked example in [docs/PUBLISHING.md](docs/PUBLISHING.md#cli-app-worked-example).
  Until `proc.exec` lands on your hosts, a CLI can also ship today fronted by a
  small HTTP shim published as an `http` adapter.

## Authentication: BYO key vs. managed key

`backend.auth` chooses how the adapter authenticates to the API:

- **`byo`** (default) — each user supplies their **own** API key at install via
  `${TOKEN}` headers (env or `$APP/secrets.json`). The key is never baked into
  the bundle. Use this when each user has their own account with the partner.
- **`managed`** — Pilot holds **one master key** and meters it per user. The
  generated adapter is **keyless**: it points at the Pilot broker, signs each
  request with the per-app identity the daemon provisions, and the broker
  verifies the caller, enforces a per-user quota, injects the master key, and
  forwards to the partner. Use this when the partner gives Pilot one shared key
  (e.g. an enrichment or data partner).

```yaml
backend:
  type: http
  base_url: https://api.example.com   # registered with the broker, not shipped to users
  auth: managed
```

Publishing is **identical** either way — same `pilot.app.yaml`, same one-repo
flow. The full design (security model + an ELI5) is in
[docs/MANAGED-KEY.md](docs/MANAGED-KEY.md). The broker lives in this repo
(`cmd/broker`, `internal/broker`); run the prod-like stack from
[`deploy/docker`](deploy/docker).

## Repository layout

```
cmd/pilot-app         the scaffolder CLI (init / validate / verify / submit)
cmd/publish-server    submission API + admin dashboard (the VM service)
cmd/broker            the managed-key gateway (holds master keys, meters per user)
cmd/broker-sign       dev/ops helper: sign a broker request as a caller
cmd/ipc-call          dev/ops helper: call a running adapter over its IPC socket
internal/scaffold     pilot.app.yaml -> adapter project (templates/)
internal/publish      submission, build, sign, case store, broker registration
internal/broker       identity verify, registry, auth inject, store, breaker
internal/catalogue    review-gate checks (SPEC §7.1)
deploy/               GCE startup script (publish + broker units) + docker/ stack
docs/                 publishing spec, managed-key design, adapter archetypes
scripts/              e2e-broker.sh, e2e-managed.sh, install-git-hooks.sh
```

## Architecture

Two flows, one repo. **Publish** (build + sign + register an app) and **runtime**
(a user calls a managed app). The website form is the only piece outside this
repo; everything else — scaffold, build, sign, broker — lives here.

```
 PUBLISH  (the admin board triggers everything)
 ─────────────────────────────────────────────────────────────────────────────
   developer          website form           publish-server  (cmd/publish-server)
   ┌────────┐  POST   ┌──────────┐  /api/submit ┌───────────────────────────────┐
   │ author │ ──────► │  form    │ ───────────► │ Validate → scaffold.Generate  │
   └────────┘         └──────────┘              │ → go build → sign → bundle    │
                                                │            (case: pending)     │
                       admin clicks Approve     │                                │
                      ───────────────────────►  │ /admin/approve:                │
                                                │  1. register managed app  ─────┼──► apps.json
                                                │     with the broker  (route)   │   (BROKER_REGISTRY)
                                                │  2. TriggerPublish (catalog) ──┼──► pilot-protocol/catalog
                                                └───────────────────────────────┘   + catalogue.json PR
                                                                                      → pilotctl install

 RUNTIME  (managed app: one master key, metered per user)
 ─────────────────────────────────────────────────────────────────────────────
   user's pilot daemon        broker  (cmd/broker, internal/broker)        partner
   ┌───────────────────┐      ┌───────────────────────────────────┐      ┌─────────┐
   │ keyless adapter   │ sign │ 1 verify caller (ed25519)  → 401   │ key  │  API    │
   │  (generated)      │ ───► │ 2 known app?               → 404   │ ───► │         │
   │  signs with the   │ HTTP │ 3 method allow-listed?     → 403   │ ◄─── │         │
   │  --identity key   │ ◄─── │ 4 breaker + per-caller quota → 429 │ resp └─────────┘
   └───────────────────┘ JSON │ 5 inject master key, forward, METER│
        ▲ daemon brokers IPC  └───────────────────────────────────┘
        │ from the calling app          │ durable per-(app,caller) usage → /gw/usage
   ┌────┴─────┐
   │  agent   │  pilotctl appstore call io.pilot.<app> <method> '{...}'
   └──────────┘
```

BYO-key apps skip the broker entirely: the adapter calls the partner directly
with the user's own `${TOKEN}`. Same scaffold, same publish flow — only
`backend.auth` differs. Deep dive: [docs/MANAGED-KEY.md](docs/MANAGED-KEY.md).

## Verified end to end

The generated adapter has been installed against the real pilot daemon (via a
`file://` catalogue), auto-spawned, and called against the **live** cosift
backend — `help`, `health`, and `search` all return correct results. The
generated `cli` archetype produces valid, compiling Go (covered by tests).

The **managed-key** path is covered by a real-process end-to-end
([`scripts/e2e-managed.sh`](scripts/e2e-managed.sh)): it drives the admin board
to build + register a managed app, runs the actual generated adapter binary
(signing with a daemon-format `identity.json`), and asserts the call flows
through the broker to the partner, is metered, and is rate-limited — plus a
multi-user broker e2e ([`scripts/e2e-broker.sh`](scripts/e2e-broker.sh)).
