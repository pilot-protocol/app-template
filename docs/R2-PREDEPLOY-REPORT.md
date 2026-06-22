# R2 Artifact Registry — Pre-Deployment Report

Validated end-to-end on macOS/darwin-arm64, 2026-06-22, against live Cloudflare R2
and a locally-built proc.exec-aware pilot daemon. Test app: **smol machines**
(`smol-machines/smolvm` v1.2.0) — a real microVM CLI shipped as a multi-file
tar.gz (wrapper + binary + libs + sparse disk images).

## Verdict

**GO, conditional on the three daemon/pilotctl prerequisites below shipping together.**
Every layer of the publish→deliver→install→run path works; the conditions are
deployment wiring, not design gaps.

## The task, vanilla vs Pilot

Task: *run a command inside an ephemeral, isolated Alpine Linux microVM and capture
its output* (proves a real separate kernel, not a container on the host).

| | Vanilla | Pilot app store |
|---|---|---|
| Invocation | `smolvm machine run --net --image alpine -- sh -c "echo …; uname -a; cat /etc/alpine-release"` | `pilotctl appstore call io.pilot.smolvm smolvm.exec '{"args":["machine","run","--net","--image","alpine","--","sh","-c","…"]}'` |
| Exit | 0 | 0 |
| stdout | `hello from microVM` / `Linux 6.12.87 … aarch64` / `3.24.1` | `hello from microVM via pilot` / `Linux 6.12.87 … aarch64` / `3.24.1` |
| Binary | must be pre-installed | delivered from R2, sha-verified, staged, exec'd — host had nothing |
| Isolation | hypervisor microVM | identical microVM, plus `proc.exec`-guarded, scrubbed-env adapter |

Identical results. The Pilot path adds delivery + integrity + capability sandboxing
at zero behavioral cost.

## Preflight checks (all ✅)

| # | Check | Result |
|---|---|---|
| 1 | R2 dev+prod buckets exist | `pilot-artifacts-dev`, `pilot-artifacts-prod` |
| 2 | Public read URLs | dev `pub-2328865f…`, prod `pub-f09f9a4e…` → 200, byte-exact |
| 3 | Bucket CORS for browser upload | `PUT/GET/HEAD` from site origins — applied (required) |
| 4 | Presign endpoint | `POST /api/artifact/presign` → presigned PUT + public URL; live round-trip OK |
| 5 | Browser upload via the real form | 30 MB smolvm tarball uploaded; sha computed client-side; submit carried `artifacts[]` |
| 6 | Build emits install spec | `install.json` + `install.sh` in every platform bundle |
| 7 | Dependency ordering | topological (deps override raw order); unit + golden tested |
| 8 | Standalone `install.sh` | fetch → sha-verify → extract → staged binary runs (`smolvm 1.2.0`) |
| 9 | Manifest correctness | `proc.exec→smolvm`, `fs.write $APP`, `net.dial <r2-host>`, `fs.read $APP/install.json`, `guarded`, sha-pinned + signed |
| 10 | Catalogue entry | v2, per-platform `bundles` map, signed; `pilotctl appstore catalogue` lists it |
| 11 | `pilotctl appstore install` | fetched the correct os/arch bundle from R2, sha256 OK, extracted (with install.json) |
| 12 | Daemon spawn | proc.exec accepted, trust anchor satisfied, `sideloaded=false` (full grants) |
| 13 | Adapter staging from R2 | fetched + verified + extracted smolvm tree under `$APP` on first spawn |
| 14 | `pilotctl appstore call` | `smolvm.version` → `smolvm 1.2.0`; `smolvm.exec` booted a real microVM |
| 15 | Integrity negative path | sha mismatch refuses to stage (covered in stage.go + tests) |

## Prerequisites that MUST ship (surfaced by the run)

1. **Daemon upgraded to proc.exec** (`pilotprotocol#317` → app-store#24). The host's
   live daemon is **v1.12.2 and rejects `proc.exec`** — native CLI apps cannot install
   until it ships. *(Validated against a locally-built proc.exec daemon.)*
2. **pilotctl install must carry `install.json`/`install.sh`** into `$APP`. Stock
   install staged only `manifest.json` + the binary, so the adapter had nothing to
   stage from. *(Patched in the proposed pilotprotocol PR; verified.)*
3. **Daemon must populate `manifest.TrustedPublishers`.** app-store#23 enforces the
   trust anchor for catalogue installs, but nothing wired the list — so the proc.exec
   daemon **skips every catalogue app** (cosift/sixtyfour included), not just new ones.
   Wire it from the reviewed publisher registry. *(Patched + verified; this is the
   highest-risk item — shipping #1 without it bricks the existing app store.)*

## Infra to set (non-code)

- R2 CORS on both buckets (applied); production custom domain when a CF API token exists.
- publish-server env: `R2_ENDPOINT`, `R2_BUCKET`, `R2_PUBLIC_BASE`, R2 S3 keys, `PUBLISH_SELF_URL`.
- Daemon: `PILOT_TRUSTED_PUBLISHERS` (or registry-backed) = the platform publisher key.

## Notes / smaller findings

- smolvm ships **sparse** disk images; Go's `archive/tar` rejects them, so `stage.go`
  and `install.sh` extract via the host `tar` (with a path-safety name-scan first).
- The catalogue is signature-gated; pilotctl verifies the `.sig` against an embedded
  key. Test used a rebuilt pilotctl with an overridden catalogue key (the documented
  `-ldflags` path) — production signs with the real release key (already wired via
  app-template#28 auto-signing).
- Daemon overlay-reconnect churn degraded re-spawn after repeated reinstalls in the
  test; a clean restart recovered immediately. Worth a soak test, not a blocker.
