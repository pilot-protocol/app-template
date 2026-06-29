# Publishing an app — what's required, what's optional, and the two paths

This is the field-level companion to [`APP-PUBLISHING-SPEC.md`](APP-PUBLISHING-SPEC.md)
(the end-to-end flow). It answers the two questions a publisher actually asks:

1. **Which fields do I *have* to provide, and which are nice-to-have?**
2. **The website form and a PR to this repo — are they the same?** (Yes. See
   [Two paths, one result](#two-paths-one-result).)

Everything below is checked against the code: the submission schema is
[`internal/publish/submission.go`](../internal/publish/submission.go)
(`Submission.Validate()` is server-authoritative), the build pipeline is
[`internal/publish/build.go`](../internal/publish/build.go), and the form/API is
[`cmd/publish-server`](../cmd/publish-server). If a doc and the code disagree,
the code wins — file a bug.

---

## Two paths, one result

There are exactly two ways to publish, and **they are at parity** — same required
fields, same validation, same generated adapter, same bundle shape:

| | A. Website form | B. PR to `app-template` |
|---|---|---|
| Where | `pilotprotocol.network/publish` | fork → `submissions/<id>/` → PR |
| You send | the **rich `Submission` JSON** (the form fields) | a **signed bundle** + a small pointer `submission.json` |
| Who scaffolds + builds the adapter | the **publish-api** does it server-side (`BuildBundle`) | **`pilot-app`** does it on your machine (`init` + `make package`) |
| Validation | `Submission.Validate()` in the API | the *same* spec validation in `pilot-app`, then `pilot-app verify` on the bundle, then CI re-runs `pilot-app verify` |
| Output | one signed, cross-compiled bundle per platform | the same — `make package` cross-compiles every platform |

The **only** difference is *where the scaffold pipeline runs* (our server vs. your
laptop). The inputs that describe the app — id, version, backend, methods,
listing, vendor — are identical, and both end at the same place: a release on
`pilot-protocol/catalog` + a one-line `catalogue.json` entry. Pick whichever path
you like; you are not choosing a different product.

> **The adapter is always scaffolded by the pipeline — never hand-built.**
> Whether the form builds it or `pilot-app` builds it, the adapter Go code, the
> manifest, and the cross-compiled binaries are **generated from your spec**. Do
> not write your own adapter and submit a pre-built, single-platform binary as if
> it were the bundle — see [the artifact rule](#artifacts-the-binary-your-app-needs).

---

## The `Submission` — required vs. optional

This is the schema the **website form** collects and the **publish-api** receives
(`POST /api/submit`). It is the single source of the app's identity, surface, and
store card. (`Submission` in `internal/publish/submission.go`.)

### Top level

| Field | Req? | Rule (from `Validate()`) |
|---|:---:|---|
| `id` | **required** | `io.pilot.<name>`, lowercase, reverse-DNS prefix mandatory |
| `version` | **required** | semver, e.g. `0.1.0` |
| `description` | **required** | one accurate line — what the app does |
| `email` | **required** | valid address; used for submit/decision notifications |
| `backend` | **required** | see [backend](#backend) |
| `methods[]` | **required** | at least one; see [methods](#methods) |
| `listing` | optional* | store-card fields; omit and the app renders a bare card |
| `vendor` | optional* | publisher info + the two reviewer free-text sections |

\* `listing`/`vendor` are not enforced by `Validate()`, but a thin listing means a
thin store page and a slower human review. Treat them as **strongly recommended**.

### `backend`

`backend.type` picks the data plane the generated adapter forwards to.

| Field | Req? | Applies to | Rule |
|---|:---:|---|---|
| `type` | **required** | both | `http` (default if empty) or `cli` |
| `base_url` | **required for http** | http | absolute `http(s)://…`; baked in as the default |
| `auth` | optional | http | `byo`/empty (each user brings a key) or `managed` (Pilot holds one master key — see [`MANAGED-KEY.md`](MANAGED-KEY.md)) |
| `headers[]` | optional | http (byo) | auth/extra headers; values may use `${TOKEN}`, resolved at install — never baked in |
| `quota` | optional | http (managed) | per-caller call cap the broker enforces (0 = unlimited) |
| `command[]` | **required for cli** | cli | base argv the adapter execs, e.g. `["gh"]` or `["python","-m","tool"]` |
| `env_passthrough[]` | optional | cli | host env vars the fronted CLI may see, on top of the scrubbed baseline (`PATH`/`HOME`/locale/`TMPDIR`) |

### `methods[]`

At least one method is required. Every app *also* auto-exposes `<ns>.help` — the
generator adds it; you do not declare it.

| Field | Req? | Rule |
|---|:---:|---|
| `name` | **required** | `<ns>.<verb>`, must be prefixed with the id's namespace (`io.pilot.weather` → `weather.*`); unique |
| `description` | **required** | full text shown in `<ns>.help` |
| `latency` | **required** | `fast` (<5s) \| `med` (≤15s) \| `slow` (≤60s) |
| `timeout` | optional | Go duration overriding the latency-class default, e.g. `"280s"` |
| `http` | **required for http** | `{verb: GET\|POST, path: /…}`; path must start with `/` (GET → query string, POST → JSON body) |
| `cli` | **required for cli** | one of `args[]`, `params_as_flags`, or `passthrough` — see [CLI worked example](#cli-app-worked-example) |
| `params[]` | optional | each `{name, type, required, description}`; `type ∈ string\|int\|bool\|number` |

### `listing` (store card — recommended)

`display_name`, `tagline`, `app_description` (long-form markdown), `license`,
`homepage`, `source_url`, `categories[]`, `keywords[]`. All optional; richer is
better. (`requires_binary`/`binary_url` relate to native binary delivery — see
[`NATIVE-APPS.md`](NATIVE-APPS.md).)

### `vendor` (publisher — recommended)

`name`, `url`, `contact`, and the two free-text sections the human reviewer reads:
`agent_usage` ("how will autonomous agents use this?") and `capabilities`. These
are review-only — they don't change the built adapter, but they speed approval.

---

## Artifacts: the binary your app needs

Every published app ships as **one signed bundle per OS/arch**, cross-compiled by
the pipeline from a single scaffold. The targets are fixed
(`DefaultPlatforms` in `build.go`):

```
linux/amd64   linux/arm64   darwin/arm64   darwin/amd64
```

The adapter is pure Go (`CGO_ENABLED=0`), so **all four cross-compile from any one
build host** — both the form (`BuildBundle`) and `make package` produce the full
per-platform set automatically. You do nothing extra.

**The rule:** an app's binaries must be **either**

- a **per-platform bundle** — one artifact per `os/arch` across
  `darwin × linux` and `arm64 × amd64` (the default, generated for you), **or**
- a **single universal binary** that genuinely runs on every target,

**never a single-platform binary.** A lone `linux/amd64` bundle is a *build-host
accident*, not a valid app — it would refuse to spawn on every other host. (This
is exactly the bug to avoid: do not hand-build one platform's adapter and submit
it as the app.)

For **native apps** that deliver a real customer binary (a CLI like `agentphone`)
the same per-os/arch discipline applies to the delivered binary via the signed
manifest `assets[]` (download URL + per-os/arch sha256). See
[`NATIVE-APPS.md`](NATIVE-APPS.md). You still never hand-build the *adapter* — the
pipeline scaffolds it; `assets[]` only references your program.

---

## CLI app worked example

A `cli` app fronts a local command-line tool instead of an HTTP API. You provide
**three things** and the pipeline does the rest:

1. `backend.type: cli` + `backend.command` (the base argv),
2. `methods[]` — curated routes and/or one passthrough, and
3. the CLI binary as an `assets[]` artifact (per os/arch, or universal) — so the
   store *delivers* the tool, per [`NATIVE-APPS.md`](NATIVE-APPS.md).

The pipeline then **scaffolds and cross-compiles the adapter** (`exec.go`,
subprocess runner, manifest with the `proc.exec` grant) and generates
`install.json`/`install.sh`. You do **not** write the adapter or ship a
pre-built one.

```yaml
id: io.pilot.toolx
app_version: 0.1.0
description: "Front the toolx CLI for agents."
backend:
  type: cli
  command: ["toolx"]              # base argv; method args are appended
  env_passthrough: [TOOLX_TOKEN]  # host env the child may see; else scrubbed
methods:
  # (1) Enumerated — a curated, named subcommand. ${field} comes from the payload.
  - name: toolx.status
    summary: "Repository status."
    duration: fast
    cli:
      args: ["status", "--short"]
  - name: toolx.lookup
    summary: "Look up an item by id."
    duration: fast
    params: {id: "string (required)"}
    cli:
      args: ["lookup", "${id}"]
  # (2) Passthrough — front the WHOLE tool: one method, every subcommand reachable.
  - name: toolx.exec
    summary: "Run any toolx subcommand. Payload {\"args\":[...]}"
    duration: med
    params: {args: "verbatim argv forwarded to toolx"}
    cli:
      passthrough: true
```

```
pilotctl appstore call io.pilot.toolx toolx.status '{}'                 # toolx status --short
pilotctl appstore call io.pilot.toolx toolx.lookup '{"id":"42"}'        # toolx lookup 42
pilotctl appstore call io.pilot.toolx toolx.exec   '{"args":["log","--oneline","-n","5"]}'
```

Each `cli` method must set **exactly one** of `args`, `params_as_flags`, or
`passthrough` (passthrough takes its argv from the call, so it can't also carry
baked `args`). argv is exec'd directly — no shell — so payload values can never be
re-parsed as shell metacharacters. Full design + hardening:
[`CLI-ADAPTER.md`](CLI-ADAPTER.md).

> **Platform status:** the `cli` archetype scaffolds and compiles today, and is
> the right shape to submit. Installing it through the catalogue needs the
> `proc.exec` capability the platform is rolling out
> ([`CLI-ADAPTER.md`](CLI-ADAPTER.md), SPEC §8/G6). Until that lands on your
> target hosts, a CLI can also ship *today* fronted by a tiny HTTP shim published
> as an `http` adapter.

---

## What the PR-path `submission.json` actually contains

The **rich `Submission`** above is what the *form* sends. On the **PR path**, the
heavy lifting already happened on your machine: `pilot-app submit --prepare` ran
the same spec validation, scaffolded + built + signed the bundle, and wrote a
small **pointer** record into `submissions/<id>/submission.json`:

```json
{
  "id": "io.pilot.toolx",
  "version": "0.1.0",
  "namespace": "toolx",
  "description": "<one accurate line — edit me>",
  "bundle": "io.pilot.toolx-0.1.0.tar.gz",
  "bundle_sha256": "<tarball sha>"
}
```

This is **not a second, looser schema** — it's the post-build receipt. The app's
full surface (backend, methods, grants) is already baked, signed, and pinned
*inside* the bundle this points at, having passed the same `Submission`-equivalent
validation locally. CI (`submission-validate`) re-verifies the bundle end to end:
tarball sha, manifest validates + signature verifies, binary sha pinned, a
`<ns>.help` method is exposed, id/version consistent. That's the parity guarantee
— the form validates the spec *before* building; the PR path validates the *built
result* — both gate on the same facts.

Required in the pointer: `id`, `version`, `namespace`, `description`, `bundle`,
`bundle_sha256` — all written by `pilot-app submit`; you only edit `description`
to one accurate line. The `metadata.json` (catalogue store card) is emitted
alongside from your `listing:` block.

---

## Quick checklist

- [ ] `id` is `io.pilot.<name>` (lowercase, reverse-DNS).
- [ ] `version` is semver; bumped for any new binary. (Shipping a **new version**
      of an already-published app? See [`UPDATING.md`](UPDATING.md) — one command,
      same key, same PR flow.)
- [ ] `description` + `email` set.
- [ ] `backend.type` chosen; `base_url` (http) **or** `command` (cli) provided.
- [ ] ≥1 `method`, each with `name` (namespace-prefixed), `description`, `latency`,
      and an `http`/`cli` route.
- [ ] Adapter is **scaffolded by the pipeline** — you did **not** hand-write or
      hand-build it.
- [ ] Binaries are the **full per-platform set** (or a true universal binary) —
      **not** a single-platform build.
- [ ] `listing` + `vendor` filled in for a real store card and faster review.
