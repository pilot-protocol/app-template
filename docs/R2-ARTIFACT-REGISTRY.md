# R2 Artifact Registry — native binary delivery for cli apps

> Status: IMPLEMENTED (RC). Lets the Pilot app store **host** publisher-supplied,
> platform-specific, versioned, signed binaries in Cloudflare R2 and install them
> — in a declared order, with optional install args — via the generated cli
> adapter. Builds on the cli-app support (proc.exec + CLI adapter) from
> app-store#24 / app-template#31. **Supersedes** the "deliver by reference, never
> store the bytes" stance in `NATIVE-APPS.md`: we now store the bytes in R2.

## Why

`NATIVE-APPS.md` / `CLI-ADAPTER.md` shipped the *translation* half (a cli adapter
that execs a local command under `proc.exec`) but assumed the binary was already
on the host. Delivering it is the point of a store. This adds the *delivery*
half: the publisher uploads per-OS/arch binaries to a Pilot-run R2 registry, and
the adapter fetches + verifies + stages them at install.

## The flow

```
PUBLISH FORM (Artifacts step)            BUILD (publish-api)                 INSTALL (host)
upload binaries → R2          ─────▶  generate adapter + install.json  ─▶  adapter staging (stage.go)
set install order + args              fold into the bundle tarball          fetch R2 → verify sha → stage
                                      (sha-pinned in the catalogue)         → run install args (order)
                                                                            → exec the staged command
```

1. **Artifacts step** (publish form, website). The publisher uploads each
   platform binary (or `.tar.gz`) to the R2 registry and sets, per artifact:
   target `os`/`arch`, `exec_path`, install `order`, optional install `args`, and
   `unpack` for archives. The form submits a JSON `Submission` carrying
   `artifacts[]` (R2 url + sha256 + order + args — never the bytes).
2. **Submit** (`POST /api/submit`). `Submission.Validate()` checks the artifacts
   (cli-only, known os/arch, https URL, 64-hex sha, relative `exec_path` under
   `$APP`, per-platform-unique order). The sha is the integrity anchor.
3. **Build** (`/admin/build` → `BuildBundle`). In addition to the signed adapter,
   the build emits **`install.json`** (the staging spec, from `cfg.Assets`) into
   every platform tarball, and the manifest gains the delivery grants
   (`proc.exec`, `fs.write $APP`, `net.dial <r2-host>`). The whole tarball is
   sha-pinned in the catalogue, so `install.json` (and the expected asset shas)
   can't be altered undetected.
4. **Install + call** (host). The generated cli adapter calls `StageAssets($APP)`
   on first spawn (`internal/backend/stage.go`): read `install.json` → select the
   asset(s) for `runtime.GOOS/GOARCH` → in ascending `order`, fetch from R2,
   verify sha256, stage under `$APP` (single file, or `tar.gz` extracted via the
   host `tar`), run any install `args` — then exec the staged `exec_path` per call.

## R2 layout

```
s3://pilot-artifacts-{dev,prod}/<app-id>/<app-version>/<os>-<arch>/<filename>
  io.pilot.smolvm/1.2.0/darwin-arm64/smolvm-1.2.0-darwin-arm64.tar.gz
```
Write-once (a new app version = a new prefix). **Single source of truth:** in
`pilot.app.yaml` an asset gives only `file:` (the filename) and the URL is
*derived* as `<artifact_base>/<id>/<app_version>/<os>-<arch>/<file>`, so the
artifact path's version always tracks `app_version` — bump it once (or
`pilot-app update --bump`) and every asset URL follows. An explicit `url:` is an
escape hatch — a native tool may carry its own version (e.g. an adapter at `0.1.0`
delivering a CLI at `0.10.0`) — accepted as-is, with the `sha256` as the integrity
anchor. See [`UPDATING.md`](UPDATING.md). Buckets `pilot-artifacts-dev` and
`pilot-artifacts-prod` exist on the Pilot R2 account. **Public read** is served by
an r2.dev managed URL (dev: `https://pub-2328865fa11041b8a5efba00b940ec14.r2.dev`);
production should attach a custom domain (e.g. `artifacts.pilotprotocol.network`).
Generated install scripts reference the public base URL.

## Schema

`pilot.app.yaml` / `scaffold.Config` gains `assets[]` (see `example.pilot.app.yaml`);
the publish `Submission` gains `artifacts[]`. Both map to:

| field | meaning |
|---|---|
| `role` | `binary` (chmod +x, default) \| `data` |
| `os` / `arch` | host match: `linux`/`darwin`, `amd64`/`arm64` |
| `url` | https R2 public URL of the artifact |
| `sha256` | 64-hex of the uploaded object; verified after download |
| `unpack` | `""` (single file) \| `tar.gz` (extract under `$APP`) |
| `exec_path` | dest under `$APP`, or the path inside the extracted tree |
| `order` | ascending install sequence (unique per platform) |
| `args` | optional post-stage invocation (e.g. a one-time setup) |

## Integrity & security

- **sha256** on every asset, checked after download; mismatch refuses to install.
- The **bundle tarball is sha-pinned** in the catalogue, so `install.json` is
  tamper-evident transitively (no app-store manifest-schema change needed).
- **`proc.exec`** (app-store#24) authorizes the exec; **`fs.write $APP`** and
  **`net.dial <r2-host>`** authorize staging. cli apps ship `protection: guarded`.
- Archive extraction uses the host `tar` (handles GNU/sparse artifacts Go's
  `archive/tar` rejects) **after** a name-scan that rejects absolute paths and
  `..` traversal (zip-slip defence).

## E2E

`scripts/e2e-smolvm.sh` + `internal/scaffold/r2_e2e_test.go` (`TestR2AssetDeliveryE2E`):
download smolvm (`smol-machines/smolvm`, a real multi-file microVM CLI: wrapper +
binary + libs + sparse disk images) for the host, upload it to `pilot-artifacts-dev`,
then build the generated adapter and let it fetch+verify+extract from R2 and exec
it — asserting `smolvm --version → "smolvm 1.2.0"`. The Go test is env-gated
(`PILOT_E2E_ASSET_URL/_SHA256/_EXECPATH/...`) so CI needs no live bucket; the
script wires it up against the real registry.

## Build / repo coordination

| Repo | Role |
|---|---|
| **app-template** (this) | schema, build-time `install.json` gen, staging runtime, manifest grants, e2e — the bulk |
| **app-store** #24 | `proc.exec` capability (reused as the exec permission) |
| **pilotprotocol** #317 | daemon dep bump so it accepts `proc.exec` |
| website #44 | publish wizard cli path; **TODO**: add the Artifacts step (uploads + order/args) as a thin client over a presign endpoint |

## Upload: presign endpoint (implemented)

`POST /api/artifact/presign` (publish-server) issues a short-lived presigned PUT
URL so the website's Artifacts step uploads straight to R2 — no S3 keys on the
client, no manual `aws s3 cp`.

```
POST /api/artifact/presign
  {"id":"io.pilot.toolx","version":"1.2.3","os":"linux","arch":"amd64","file":"toolx"}
→ {"method":"PUT","put_url":"https://…(presigned, 15 min)…",
   "public_url":"https://artifacts.pilotprotocol.network/io.pilot.toolx/1.2.3/linux-amd64/toolx",
   "key":"io.pilot.toolx/1.2.3/linux-amd64/toolx","file":"toolx","expires_in":900}
```

- The **key is computed server-side** from `{id,version,os,arch,file}` — the client
  never controls the prefix, so an upload can only land under its own
  `<id>/<version>/<os>-<arch>/`.
- **Write-once:** a key that already exists is refused (409) — bump the version to
  upload new binaries. This is what guarantees an artifact can't drift from its
  adapter version.
- `public_url` is exactly what the scaffold *derives* from `app_version`, so the
  `file:` you drop into `pilot.app.yaml` resolves to the bytes you just uploaded.
- Config (server env): `R2_ACCOUNT_ID` (or `R2_ENDPOINT`), `R2_BUCKET`,
  `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_REGION` (default `auto`),
  `R2_PUBLIC_BASE`. Unset ⇒ the endpoint returns 503 (uploads disabled). The SigV4
  presigner is dependency-free and verified against AWS's reference vector.

## Follow-ups

- **Signing-proxy `GET /artifact/...`** so installs can run off a stable proxy URL
  where a public domain isn't configured.
- **Server-side re-verify** of each artifact sha against the stored R2 object at
  submit time.
- Production **custom domain** for `pilot-artifacts-prod` (needs a Cloudflare API
  token with R2 + DNS scope; the S3 keys can't enable public access).
