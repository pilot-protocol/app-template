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
- **`cli`** — generates a working subprocess adapter, but installing it needs a
  `proc.exec` capability the platform doesn't ship yet. See
  [docs/CLI-ADAPTER.md](docs/CLI-ADAPTER.md). Until then, front a CLI with a
  small HTTP shim and publish it as an `http` adapter.

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
  (e.g. Sixtyfour AI).

```yaml
backend:
  type: http
  base_url: https://api.sixtyfour.ai   # registered with the broker, not shipped to users
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
internal/scaffold     pilot.app.yaml -> adapter project (templates/)
internal/publish      submission, build, sign, case store, broker registration
internal/broker       identity verify, registry, auth inject, store, breaker
internal/catalogue    review-gate checks (SPEC §7.1)
deploy/               GCE startup script + docker/ (prod-like broker + publish stack)
docs/                 publishing spec, managed-key design, adapter archetypes
scripts/              e2e-broker.sh, install-git-hooks.sh
```

## Verified end to end

The generated adapter has been installed against the real pilot daemon (via a
`file://` catalogue), auto-spawned, and called against the **live** cosift
backend — `help`, `health`, and `search` all return correct results. The
generated `cli` archetype produces valid, compiling Go (covered by tests).
