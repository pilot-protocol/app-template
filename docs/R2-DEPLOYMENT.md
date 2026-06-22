# R2 Artifact Registry — Deployment Plan

Ship native-CLI app delivery: a publisher uploads platform binaries through the
publish form, they land in a Pilot-run Cloudflare R2 registry, and a Pilot user
runs `pilotctl appstore install <id>` to fetch + verify + stage + run them. This
plan lists every change, in dependency order, plus the infra and the validated
prerequisites surfaced by the end-to-end run.

## 1. Cloudflare R2

| Item | Value / action |
|---|---|
| Account | `ef9da13de5572ea8482b2921770fa0e3` (S3 endpoint `https://<acct>.r2.cloudflarestorage.com`) |
| Buckets | `pilot-artifacts-dev`, `pilot-artifacts-prod` (created) |
| Object layout | `<app-id>/<version>/<os>-<arch>/<file>` (binaries); `bundles/<app-id>/<version>/<tarball>` (signed bundles) |
| Public read (dev) | r2.dev managed URL `https://pub-2328865fa11041b8a5efba00b940ec14.r2.dev` |
| Public read (prod) | r2.dev managed URL `https://pub-f09f9a4ea848491198d48e329ba030e3.r2.dev` |
| **CORS (required)** | `PUT,GET,HEAD` from the website origins — **without this the browser upload fails** (preflight blocked). Applied to both buckets via `put-bucket-cors`. |
| Production hardening | swap the r2.dev URL for a custom domain (`artifacts.pilotprotocol.network`) once a Cloudflare API token with R2+DNS scope is available; update `R2_PUBLIC_BASE`. |

CORS config applied (keep the website origins current):
```json
{"CORSRules":[{"AllowedOrigins":["https://pilotprotocol.network","https://www.pilotprotocol.network"],
  "AllowedMethods":["GET","PUT","HEAD"],"AllowedHeaders":["*"],"ExposeHeaders":["ETag"],"MaxAgeSeconds":3600}]}
```

## 2. publish-server env (the VM)

```
R2_ENDPOINT=https://ef9da13de5572ea8482b2921770fa0e3.r2.cloudflarestorage.com
R2_BUCKET=pilot-artifacts-prod
R2_PUBLIC_BASE=https://pub-f09f9a4ea848491198d48e329ba030e3.r2.dev   # or the custom domain
AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY   # R2 S3 keys (scoped to the artifacts buckets)
PUBLISH_SELF_URL=https://publish-api.pilotprotocol.network            # for the signing-proxy fallback
```
Adds: `POST /api/artifact/presign` (browser → direct R2 upload) and the signing-proxy
`GET /artifact/...`. Unset = uploads return 503 (graceful).

## 3. The PRs (dependency order)

| # | Repo | PR | What | Status |
|---|---|---|---|---|
| 1 | app-store | **#24** | `proc.exec` capability + hardened target | open (existing) |
| 2 | app-template | **#31** | CLI adapter + publish-api + proc.exec scaffolding | open (existing) |
| 3 | app-template | **NEW: R2 artifact registry** | `assets`/`artifacts` schema (os/arch/url/sha/unpack/exec_path/**deps**/order/args), `install.json` + `install.sh` generation, `stage.go` staging runtime, manifest delivery grants, `/api/artifact/presign` + signing proxy + R2 SigV4 client, full test suite + smolvm e2e | branch `feat/r2-artifacts-on-cli` |
| 4 | website | **#44** | CLI publish-form path | open (existing) |
| 5 | website | **NEW: Artifacts step** | wizard step: per-platform upload via presign, install order, dependencies, install args; `artifacts[]` in the submit payload | branch `cli-publish-form` + Artifacts commit |
| 6 | pilotprotocol | **#317** | daemon dep bump to the proc.exec app-store version | open (existing) |
| 7 | pilotprotocol | **NEW: install + trust wiring** | `pilotctl install` carries `install.json`/`install.sh` into `$APP`; daemon populates `manifest.TrustedPublishers` (from the publisher registry) and honors `PILOT_APPSTORE_ROOT` | patch ready (this run) |

### Why #7 is a hard blocker (surfaced by the e2e)
- `pilotctl appstore install` only staged `manifest.json` + the binary, **dropping `install.json`** — so the adapter had nothing to stage from. Fixed: carry the install spec files.
- app-store **#23 enforces the trust anchor** (non-sideloaded installs), but **nothing populated `manifest.TrustedPublishers`** — so the proc.exec daemon skips **every** catalogue app (cosift/sixtyfour included), not just new ones. Fixed: wire `TrustedPublishers` from the reviewed publisher registry. This MUST ship with #6 or the app store breaks on upgrade.

## 4. Autonomous publish flow (unchanged shape, now with artifacts)

```
website form (Artifacts step → presign → R2 upload)
  → POST /api/submit {Submission + artifacts[]}      (CORS-locked)
  → admin Build → BuildBundle: scaffold adapter + sign manifest + emit install.json/install.sh
                  → self-verify through the catalogue gate (per platform)
  → admin Approve → release bundles + open the one-line catalogue.json PR (signed)
  → catalogue merge → pilotctl appstore install <id>
```
- **Correct catalogue entry**: v2 with `bundle_url`+`bundle_sha256` (primary) and a per-platform
  `bundles` map; signed with the embedded catalogue key (pilotctl verifies the `.sig` on load).
- **Correct manifest**: `proc.exec → <command>`, `fs.write $APP`, `net.dial <r2-host>`,
  `fs.read $APP/install.json`, `protection: guarded`; binary sha pinned + ed25519-signed.
- **Install script**: `install.json` (machine-readable, consumed by `stage.go`) + `install.sh`
  (standalone POSIX, dependency-ordered) ship in every bundle, both tested.

## 5. Pre-deploy validation (this run)

- ✅ R2 dev+prod buckets, public URLs, CORS — browser upload of the real smolvm tarball succeeded (sha computed client-side, presigned PUT, served back).
- ✅ Full website form (Astro, PR #44 + Artifacts step) drove submit → API → build end-to-end.
- ✅ `pilotctl appstore catalogue/install/list/call` against a proc.exec daemon: install fetched the right platform bundle from R2, sha-verified, staged install.json, daemon spawned the adapter (full grants), the adapter fetched + verified + extracted smolvm from R2 and exec'd it.
- ✅ `install.sh` generation + dependency ordering: unit + golden tested.
- See `R2-PREDEPLOY-REPORT.md` for the smol-machines vanilla-vs-pilot task comparison + preflight.
