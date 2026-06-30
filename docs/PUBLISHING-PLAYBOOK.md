# Publishing Playbook — from any app to the Pilot app-store

The end-to-end runbook for getting an app live on the Pilot app-store, for **every**
backend and auth mode. It ties together the focused docs
([`PUBLISHING.md`](PUBLISHING.md), [`CLI-ADAPTER.md`](CLI-ADAPTER.md),
[`NATIVE-APPS.md`](NATIVE-APPS.md), [`R2-ARTIFACT-REGISTRY.md`](R2-ARTIFACT-REGISTRY.md),
[`MANAGED-KEY.md`](MANAGED-KEY.md), [`CI-AB-REPORT.md`](CI-AB-REPORT.md),
[`UPDATING.md`](UPDATING.md), [`APP-PUBLISHING-SPEC.md`](APP-PUBLISHING-SPEC.md)) into one
checklist. If you only read one doc, read this one and follow the links when a step
needs detail.

## Mental model

A Pilot app is a **thin, signed adapter + a manifest**. The daemon fetches the bundle
from the catalogue, **re-verifies its signature and binary sha256 on every spawn**, hands
it a unix socket, and brokers JSON-in/JSON-out IPC. Your real backend (an HTTP API, or a
CLI tool) lives where it already lives; the adapter just forwards each method to it.

Three repos, up to three PRs:

| Repo | What you add | Who merges |
|---|---|---|
| `pilot-protocol/app-template` | the **submission** (`submissions/<id>/`) | maintainer (CI-gated) |
| `pilot-protocol/pilotprotocol` | the **catalogue entry** (signed `catalogue.json` + `metadata.json`) | release pipeline / maintainer |
| `pilot-protocol/website` | the **store-page card** (optional but expected) | maintainer |

Everything below is driven from **one spec file** (`pilot.app.yaml`) plus the
`pilot-app` tool. Author once; the same inputs flow to the website publish-form path and
the PR path identically (see [`PUBLISHING.md`](PUBLISHING.md)).

## Step 0 — Pick your backend and auth (decision table)

Two orthogonal choices. Get these right first; everything else follows.

**Backend** (`backend.type`):

| Your app is… | `backend.type` | Notes |
|---|---|---|
| an HTTP API you host | `http` | each method → an endpoint (GET→query, POST→JSON body) |
| a CLI tool **already on every host** | `cli` | methods → subprocess argv |
| a CLI tool **not** on the host | `cli` + `assets[]` | the adapter fetches the binary from the R2 registry at install ([`NATIVE-APPS.md`](NATIVE-APPS.md)) |

**Auth** (`backend.auth`) — only relevant when the backend needs a key:

| Key situation | `backend.auth` | What ships |
|---|---|---|
| no key needed | omit | adapter calls the backend directly |
| **each user has their own key** | `byo` (default) | keyless bundle; user supplies `${TOKEN}` at install (env or `$APP/secrets.json`); the key is never baked in |
| **one shared partner key, metered per user** | `managed` | keyless bundle pointing at the Pilot **broker**; the broker holds the master key, verifies the caller, enforces per-user quota, injects the key, forwards ([`MANAGED-KEY.md`](MANAGED-KEY.md)) |

> **Edge case — an API key is required.** Never bake a key into a bundle (it is public and
> sha-pinned). Choose `byo` if every user brings their own account, or `managed` if the
> partner gives Pilot **one** key to meter. Managed apps additionally require an ops step at
> go-live: register the app + master key with the broker and SIGHUP it (Step 7.5). Until
> that is done the managed app installs but every call 5xx's at the broker.

## Step 1 — Author `pilot.app.yaml`

```bash
go build -o pilot-app ./cmd/pilot-app
./pilot-app example > pilot.app.yaml     # fully annotated starter (http, cli, assets, auth)
$EDITOR pilot.app.yaml
./pilot-app validate                     # fast schema feedback
```

Required for every app: `id` (`io.pilot.<name>` or `io.<vendor>.<name>`), `app_version`
(**must equal the upstream tool/release version for a wrapped tool**), `description`,
`backend`, `methods[]`, `listing{}`. Each method: `name` (namespaced `<ns>.*`), `summary`,
`duration` (`fast|med|slow`), and a route (`http:{verb,path}` or `cli:{args|passthrough}`)
+ `params`. **Do not declare `<ns>.help`** — it is auto-generated as the discovery contract.

Curate a small set of named methods for the common operations, then add:
- one **passthrough** method (`cli:{passthrough:true}`, payload `{args:[…],stdin?}`) that
  fronts the whole tool — the escape hatch for anything not curated;
- a **`<ns>.cli_help`** method returning the tool's full `--help`;
- a **`<ns>.version`** method.

`listing` carries the store page: `display_name`, `tagline`, `app_description` (long-form
markdown — include a **bulleted feature list** and embed the tool's full `--help`),
`license`, `homepage`, `source_url`, `categories`, `keywords`.

## Step 2 — (CLI + not-on-host only) Deliver the native binary from R2

Skip this whole step for `http` apps and for CLI tools already present on hosts.

See [`NATIVE-APPS.md`](NATIVE-APPS.md) + [`R2-ARTIFACT-REGISTRY.md`](R2-ARTIFACT-REGISTRY.md).
The adapter fetches the asset matching the host's os/arch at install, **verifies its
sha256**, unpacks it under `$APP`, and execs the staged path.

1. **Source per-platform binaries** for `darwin/{arm64,amd64}` and `linux/{amd64,arm64}`.
   - Some tools ship official precompiled per-platform binaries (download them).
   - Otherwise build relocatable binaries from conda-forge with micromamba
     (`micromamba create --platform <plat> -p <prefix> -c conda-forge <pkg>`), as the
     PostgreSQL and Redis apps do.
2. **Relocation.** conda binaries usually find their libs via `$ORIGIN`/`@loader_path`
   rpaths and just work from any path. If a tool hard-codes its compile-time prefix (e.g.
   to find a `share/` data dir), ship a tiny wrapper that recreates that prefix as a
   **symlink to the staged root** at runtime (pattern: the PostgreSQL `pg` wrapper). Verify
   relocation by extracting to a throwaway path and running — on Linux too, not just macOS.
3. **Wrapper.** For a multi-binary tool, ship a `<cmd>` dispatcher wrapper as
   `backend.command[0]`; its basename must equal `command[0]`. If the tool colorizes
   `--help` with ANSI codes, have the wrapper serve a captured, ANSI-stripped help file for
   `-help/--help` so `<ns>.cli_help` returns clean text.
4. **Package** each platform as `<id>-<ver>-<os>-<arch>.tar.gz`. On macOS, tar with
   `COPYFILE_DISABLE=1 tar --no-mac-metadata` (and `xattr -cr` first) so no `._*`
   AppleDouble junk leaks into the tarball. sha256 each.
5. **Upload** to the R2 artifact registry under `<id>/<ver>/<os>-<arch>/` and confirm each
   public URL serves bytes whose sha matches. Reference each as an `assets[]` entry
   (`os, arch, url|file, sha256, unpack, exec_path, order`). Give `file:` (filename only) +
   `artifact_base:` so URLs **derive from `app_version`** and a version bump moves them all.

> **Edge case — single-platform bundles are rejected.** Every app must cross-compile/ship
> the full `darwin × linux × arm64 × amd64` set (or a true universal binary). A lone
> `linux/amd64` bundle is a build-host accident that refuses to spawn elsewhere.

## Step 3 — Scaffold, key, build, sign

```bash
./pilot-app init -o ./my-app && cd my-app
make gen-key          # one-time ed25519 publisher key — gitignored, NEVER committed
make package          # build → sha256-pin the binary → sign the manifest → tar
../pilot-app verify io.pilot.<id>-<ver>.tar.gz   # run the catalogue gate locally
```

- The **publisher key is stable forever** for this app id. Every future version must be
  signed by the **same** key — the update gate proves ownership by possession of it
  ([`UPDATING.md`](UPDATING.md)). Back it up.
- `make package` builds **one** platform. For the full 4-platform set (what the catalogue
  needs) use the canonical builder `internal/publish.BuildBundle(cfg, priv)`, which
  cross-compiles all of `DefaultPlatforms`, sha-pins, signs, and self-verifies each. Note
  `make package`'s single-platform tarball omits `install.json`; `BuildBundle`'s does not.
- The signed `manifest.json` carries `store.publisher` (= the catalogue's `publisher` pin)
  and the grants the app needs (for native delivery: `proc.exec`, `fs.read $APP/install.json`,
  `fs.write $APP`, `net.dial <asset-host>`).

## Step 4 — Test before you PR

Do all of these; do not skip the runtime tests just because the build passed.

1. **Gate:** `pilot-app verify-submission submissions/<id>/submission.json` — scaffolds +
   cross-compiles all 4 platforms and runs the review gate. Needs Go, no network.
2. **Socket mode (host OS):** run the built adapter directly
   (`./bin/<adapter> --socket app.sock --manifest manifest.json`), which stages the binary
   from R2, then call **every** method with real inputs via `cmd/ipc-call`.
3. **The other OS:** exercise the binary on Linux (e.g. a microVM) by pulling the real R2
   artifact and running the real-data suite — catches relocation/glibc issues.
4. **Real install:** `pilotctl appstore install <id>` from a signed local catalogue
   (`PILOT_APPSTORE_CATALOG_URL=file://…`), wait for the daemon to supervise the app, then
   `pilotctl appstore call <id> <method> …`. This exercises catalogue-sig verify → bundle
   fetch → sha verify → spawn → IPC, end to end.
5. **A/B report** ([`CI-AB-REPORT.md`](CI-AB-REPORT.md)): `scripts/ab_report.py` runs the
   same commands as the vanilla CLI and as the adapter and diffs output/exit. For CI's A/B
   job to run (not skip), **commit the `linux/amd64` adapter bundle** into `submissions/<id>/`
   — the workflow globs for a tarball there.
6. **Managed apps:** also run the broker e2e (`make e2e-managed`) — submit → build →
   register → broker → meter → rate-limit.

## Step 5 — Submission PR (`pilot-protocol/app-template`)

Two equally-valid shapes; CI picks the check by whether `submission.json` has a `bundle`
key:

- **Rich** (`submission.json` with `backend`/`methods`/`listing`/`artifacts`, no committed
  tarball): CI runs `verify-submission` and builds all platforms fresh. Used by the
  PostgreSQL/DuckDB apps.
- **Pointer** (`submission.json` with `bundle`/`bundle_sha256` + the committed signed
  tarball(s) + `metadata.json`): CI runs `verify <bundle>`. This is what
  `pilot-app submit --prepare <fork>` writes, and the **only** shape `publish-on-merge.yml`
  can auto-release.

```bash
git switch -c submit/<id>
# add submissions/<id>/{submission.json, ab-commands.json, [committed linux/amd64 bundle]}
git commit && git push && gh pr create -R pilot-protocol/app-template
```

Required check: **`validate`** (build/vet/test the tool + verify the bundle + the update
gate). Also green before merge: `build-test`, `lint`, `e2e-broker`, `e2e-managed`,
`ab-report`, `security/snyk`. On merge of a **pointer** submission, `publish-on-merge.yml`
releases the bundles and opens the signed catalogue PR automatically; a **rich** submission
does not auto-publish — do Step 6 by hand.

## Step 6 — Catalogue PR (`pilot-protocol/pilotprotocol`)

Makes the app installable. The catalogue index is **signed**; the daemon and `pilotctl`
fail-closed on an unsigned/invalid catalogue, so this PR must carry a valid
`catalogue.json.sig`.

1. Upload the 4 signed adapter bundles to the **prod** R2 `bundles/<id>/<ver>/`; record each
   `bundle_url` + `bundle_sha256` (+ size).
2. Add `catalogue/apps/<id>/metadata.json` (the v2 store-page record) and reference it by
   `metadata_url` (raw GitHub) + `metadata_sha256`.
3. Insert the app entry into `catalogue/catalogue.json` (keep `version: 2`): `id`, `version`,
   `description`, `display_name`, `vendor`, `license`, `source_url`, `bundle_url` +
   `bundle_sha256` (the `linux/amd64` primary), a `bundles{}` map of all four platforms,
   `categories`, `metadata_url` + `metadata_sha256`, and **`publisher`** (taken from the
   bundle manifest's `store.publisher` — the catalogue fail-closes any entry without a
   publisher pin).
4. **Sign** `catalogue.json` with the release key (held only by the release pipeline as a CI
   secret — `scripts/publish-submission.sh` does this automatically on the pointer/auto
   path). Commit `catalogue.json` + `catalogue.json.sig` + `metadata.json`, open the PR.

> Keep `version: 2`. New fields (`bundles`, `publisher`, …) are v2-**optional**; bumping to 3
> makes every client reject the whole catalogue.

## Step 7 — Website card (`pilot-protocol/website`)

Add a card to the app-store page (`src/pages/app-store.astro`) mirroring an existing card.
Keep its plain-text twin (`src/pages/plain/app-store.astro`) in sync (re-stamp the coverage
sha; `npm run build` enforces it). Use a logo **mark that is visible on both light and dark**
backgrounds (not a dark wordmark). Card fields: name (no version unless intended; the version
lives in the footer badge), short description, `pilotctl appstore install <id>`, method count
/ size / category, and a consistent footer link label.

## Step 7.5 — Managed apps only: broker go-live (ops)

For `auth: managed`, after the catalogue is live, an operator must register the app + master
key with the broker and reload it (see [`MANAGED-KEY.md`](MANAGED-KEY.md) and `deploy/`). The
app is keyless and metered per caller; nothing about the key ever reaches the user.

## Step 8 — Updates

Bump with `pilot-app update --bump patch|minor|major` (single source of truth:
`app_version`), re-upload native assets if their URLs derive from the version, re-sign with
the **same publisher key**, and open the same two PRs. The update gate rejects a downgrade or
a different signing key. Full detail in [`UPDATING.md`](UPDATING.md).

## Pre-flight checklist

- [ ] `app_version` == upstream tool version (for a wrapped tool)
- [ ] backend + auth chosen correctly; **no API key baked into any bundle**
- [ ] managed apps: broker registration planned for go-live
- [ ] native delivery: all 4 platforms built, relocation verified **on Linux too**, no `._*`
      junk in tarballs, R2 URLs serve matching shas
- [ ] every method tested with real data in socket mode **and** via `pilotctl install`
- [ ] A/B report run; `linux/amd64` bundle committed so CI's A/B runs
- [ ] `verify-submission` / `verify` is green
- [ ] catalogue entry: `bundles{}` for all 4 platforms, `publisher` pin matches the manifest,
      `metadata_sha256` matches, `catalogue.json.sig` valid, `version` still 2
- [ ] website card: logo visible on dark, plain twin in sync, build green
- [ ] publisher key backed up (you need the same one for every future version)
