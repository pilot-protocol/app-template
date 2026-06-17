# Native (binary-delivery) apps — design

> Status: DESIGN + TODO. Native/CLI apps are **Coming soon** — blocked at the
> wizard's type step; only HTTP (translation-only) apps ship today. Decision
> (2026-06-17): native apps deliver the real binary via a **customer-hosted URL +
> per-OS/arch sha256**, pinned in the signed manifest and **fetched + verified +
> staged by the daemon at install**. We never store the bytes. The "Platform
> changes" sequence below is the build plan; `TODO(native-apps)` markers across
> the repo point here (grep `TODO(native-apps)`).

## Two archetypes

1. **API app** (translation-only). The adapter forwards each method to a remote
   HTTP API; the real app runs on the vendor's servers. We build + sign the
   adapter; nothing is downloaded to the host. (cosift, most SaaS.)
2. **Native app** (translation + delivered binary). The real program runs **on
   the host** (e.g. a CLI like agentphone). The store must **deliver** that
   binary — assuming it's pre-installed is wrong; delivering it is the point of a
   store. The adapter is the thin layer that drives the delivered binary.

## Why "deliver by reference", not upload or embed

- The customer **does not upload** their (often proprietary) binary to us — we
  can't compile or inspect it, and we shouldn't become its distributor.
- It is **not assumed present** on the host — the store installs it.
- So the manifest **references** the binary: a download URL + sha256, per
  OS/arch. The daemon fetches it at install, verifies the sha, and stages it.
  Integrity comes from the sha being **inside the signed manifest** — tampering
  with the URL or bytes fails verification, even though we never hold the bytes.

## Manifest schema (added to `app-store/pkg/manifest`)

```jsonc
"assets": [
  { "role": "binary",                       // the program the adapter drives
    "os": "linux", "arch": "amd64",         // host match; daemon picks the right one
    "url": "https://dl.agentphone.ai/cli/v2/agentphone-linux-amd64",
    "sha256": "<64-hex>",
    "exec_path": "bin/agentphone",          // where it's staged under $APP, + chmod +x
    "size": 18923456 },
  { "role": "binary", "os": "darwin", "arch": "arm64", "url": "...", "sha256": "...", "exec_path": "bin/agentphone" }
]
```
- The `assets[].sha256` values are folded into the manifest **signing payload**
  (backward-compatible: when `assets` is empty the payload is unchanged, so
  existing API-app signatures still verify).
- `exec_path` is validated to stay under `$APP` (no traversal), same as `binary.path`.

The adapter (our signed Go binary) is still `binary` in the bundle; `assets` are
the *extra* files the daemon stages beside it. The adapter execs the asset from
`$APP/<exec_path>`.

## Agentic install → use flow (must be zero-human, via pilotctl)

The whole flow stays **discover → install → call** — no new steps for the agent:

```
pilotctl appstore catalogue                 # discover (unchanged)
pilotctl appstore install io.pilot.<app>    # daemon: fetch adapter bundle (verify sha+sig)
                                            #   → read manifest.assets, pick os/arch match
                                            #   → fetch each asset, verify sha256, stage to $APP/<exec_path> (chmod +x)
                                            #   → spawn adapter → ready
pilotctl appstore call io.pilot.<app> <m> '{...}'   # adapter execs $APP/<exec_path> (unchanged surface)
```

Requirements for the agentic UX:
- **One command installs everything** (adapter + delivered binary) — the agent
  never fetches a binary itself.
- **Clear, structured errors**: no asset for this os/arch → "no binary for
  linux/arm64; available: linux/amd64, darwin/arm64"; sha mismatch → name both
  hashes; download fail → the URL + HTTP status. Agents act on these.
- **`<ns>.help`** continues to describe the method surface; add a top-level
  `kind: api|native` so an agent can see what it installed.

## Platform changes (the build, in order)

1. **`app-store/pkg/manifest`**: `Asset` type + `Assets []Asset`; validate
   (known os/arch, sha256 hex, exec_path under $APP, https url); fold asset shas
   into `signingPayload` (empty-safe). New module version. *(foundation — PR 1)*
2. **`app-store/pkg/assets`** (new): `Select(assets, GOOS, GOARCH)` + `FetchVerifyStage(asset, appDir)` (HTTP GET, sha256 check, write `exec_path`, chmod 0755). Pure, table-tested. *(PR 1)*
3. **pilotctl install** (`TeoSlayer/pilotprotocol` `cmd/pilotctl/appstore.go`):
   after staging the bundle + verifying the manifest, call `assets.Select` +
   `FetchVerifyStage` for each; structured errors; record staged assets in the
   install audit. *(PR 2 — this is the agent-facing path)*
4. **supervisor** (`app-store/plugin/appstore`): nothing new — it execs the
   adapter; the adapter execs `$APP/<exec_path>`. Re-verify asset sha on spawn
   (cheap defence) is optional hardening.
5. **scaffold** (`app-template`): native adapter template execs `$APP/<exec_path>`;
   `pilot.app.yaml` gains `assets:`; generator emits manifest `assets`. *(PR 3)*
6. **GUI** (`publish-server`): the CLI/native step collects per-arch URL+sha256 +
   exec command + method→args; review table shows the assets. *(PR 3)*

## Security / review

- Integrity: sha256 in the signed manifest; HTTPS-only URLs; daemon re-checks on
  fetch. We never hold or re-sign the bytes.
- Human review (the catalogue gate) scrutinizes the asset URL + source + that the
  app genuinely needs a native binary (vs an API). Native apps are higher-scrutiny.
- Availability: customer URL must stay up; a dead URL fails install with a clear
  error. (Optional later: opt-in mirroring to `pilot-protocol/catalog`.)
