# Updating an app — shipping the new adapters and bundles

[`UPDATING.md`](UPDATING.md) covers the *rules* of an update: bump `app_version`,
re-sign with the same publisher key, don't go backwards. This is the other half —
the **mechanics of producing and publishing the artifacts** that update actually
ships, and the order the steps have to happen in.

Read this when you are shipping a new version of an app that is already in the
catalogue. It ends where the downstream steps begin: merging the submission PR
opens the catalogue entry, and the store page follows from that. Those are
covered in [`PUBLISHING-PLAYBOOK.md`](PUBLISHING-PLAYBOOK.md) and are not repeated
here.

## The one thing to get right

**Upload the new bundles to the artifact registry *before* the submission PR
merges.** Everything else in this document is detail; this is the ordering rule
that decides whether your update lands or strands.

For a rich submission (a `backend`/`methods` spec with no committed tarball), the
catalogue entry is derived from **whatever is already live on the registry** for
that `id@version`. Merge with nothing uploaded and the publish job fails loudly:
the app-template PR is merged, the catalogue still points at the old version, and
your app looks shipped while every user is still on the previous release. Recover
by uploading and re-running the publish job, but it is easier to just do it in
order.

```
build all 4 → verify each → upload → verify the public URLs → runtime e2e → merge
```

## Step 0 — know which spec shape you have

Two shapes exist, and the update path differs:

| Shape | What you edit | How you build |
|---|---|---|
| **`pilot.app.yaml` project** | `pilot.app.yaml` | `pilot-app update --bump …` then the canonical builder |
| **Rich `submission.json`** | `submissions/<id>/submission.json` directly | the canonical builder, straight from the submission |

A rich submission has no `pilot.app.yaml` — the submission **is** the spec. The
`pilot-app update` flow in [`UPDATING.md`](UPDATING.md) does not apply to it;
bump `version` in `submission.json` by hand and build from that.

Either way, the version you bump is the single source of truth: the manifest, the
asset URLs and the catalogue entry are all derived from or gate-checked against it.

## Step 1 — build all four platforms with **your** key

```
darwin/arm64   darwin/amd64   linux/arm64   linux/amd64
```

A single-platform bundle is rejected — it refuses to spawn on every other host.
The adapter is pure Go with `CGO_ENABLED=0`, so all four cross-compile from any
one machine; there is no reason to ship fewer.

> **`make package` builds ONE platform.** It is the quickstart convenience, not
> the release builder, and its tarball also omits `install.json`. The canonical
> builder is `internal/publish.BuildBundle(cfg, priv)`, which cross-compiles every
> target in `DefaultPlatforms`, sha-pins each binary, signs each manifest and
> self-verifies each result against the catalogue gate. Use it for anything you
> intend to publish.

Sign with the **same ed25519 publisher key that first published the app**. That
key is the only proof of ownership; there is no password and no stored secret to
fall back on. Before you build, confirm the key you are about to use derives the
`publisher` pin already in the catalogue — a mismatch is a rejected PR, and
finding out at build time is cheaper than finding out at merge time.

Keep the key out of the repository and out of the bundle. Bundles are public and
sha-pinned; an app that needs an API key uses `byo` or `managed` auth, never a
baked-in key.

## Step 2 — verify each bundle locally

```sh
pilot-app verify <id>-<ver>-<os>-<arch>.tar.gz    # once per platform
```

This is the same gate the catalogue and every client run: bundle layout, manifest
schema, the binary sha256 pin, the signature, the `<ns>.help` discovery contract,
and id/version agreement. Run it on all four, not just the one you built first.

Also run the submission-level gate, which scaffolds and cross-compiles from the
spec and catches anything the spec itself gets wrong:

```sh
pilot-app verify-submission submissions/<id>/submission.json
pilot-app verify-update     submissions/<id>/submission.json   # ownership + no downgrade
```

Note that `verify-submission` signs its throwaway build with an **ephemeral** key —
it proves the spec builds, not that you hold the publishing key. Step 1 is what
proves that.

## Step 3 — upload to the artifact registry

Adapter bundles live under a **different prefix** from native tool assets. Both
are on the same registry; do not confuse them:

| What | Prefix |
|---|---|
| adapter bundles (every app) | `bundles/<id>/<version>/<id>-<version>-<os>-<arch>.tar.gz` |
| native tool assets (`cli` apps that ship a binary) | `<id>/<version>/<os>-<arch>/<file>` |

The registry is **write-once**: a key that already exists is refused. A new
version is a new prefix, so an update is always additive and the previous version
stays intact and installable. That is deliberate — it means an update can never
corrupt the release people are currently running, and a bad update can be backed
out by pointing the catalogue back at the old prefix.

If your app ships native binaries, upload those for the new version too. Their
URLs are *derived* from `app_version`, so a bump moves all of them at once and
they will 404 until the new prefix exists.

Publishers without direct registry credentials should use the presigned upload
flow described in [`R2-ARTIFACT-REGISTRY.md`](R2-ARTIFACT-REGISTRY.md) — the
upload key is computed server-side from `{id, version, os, arch, file}`, so an
upload can only ever land under its own version's prefix.

## Step 4 — verify the public URLs, not your upload

Fetch each artifact back over its **public URL** and check the sha256 matches what
you built. An upload that reported success and a URL that serves the right bytes
are different claims, and the catalogue pins the second one.

```sh
curl -fsS -o dl.tar.gz "<public-base>/bundles/<id>/<ver>/<id>-<ver>-linux-amd64.tar.gz"
shasum -a 256 dl.tar.gz          # must equal the sha you recorded at build time
pilot-app verify dl.tar.gz       # re-run the gate on the bytes actually served
```

Do this for all four. Record each sha — the catalogue entry pins them.

## Step 5 — runtime e2e against the published artifact

A build that compiles is not an update that works. Run the real thing, on both
operating systems, against the artifact you just published:

1. **Unpack the published bundle** — not your local build directory. This is what
   users will get.
2. **Run the adapter in socket mode** with the bundle's own manifest, and call
   **every** method with real inputs.
3. **Repeat on the other OS.** Cross-compiled binaries that have never been
   executed on Linux are exactly where relocation and libc problems hide. Pull the
   real artifact on a Linux host and run the same suite.
4. **Check what the update *removed*, not only what it added.** If methods were
   retired, confirm they are gone from `<ns>.help` and no longer answer.
5. **Check `<ns>.help`** reflects the new surface. It is the discovery contract;
   an agent that reads a stale help calls methods that no longer exist.

For a `managed` app this path also proves the broker leg: keyless adapter → broker
→ partner API. A method that returns an auth error or a 403 "method not allowed"
is telling you about Step 6, not about your bundle.

## Step 6 — if the update changed HTTP routes, the broker must change with it

**This is the step most likely to be forgotten, and it fails silently in the worst
direction.**

A `managed` app forwards through the broker, which holds the master key and
allow-lists the method paths it will proxy. That allow-list is **not** derived
from your submission at call time — it is registered configuration. If an update
retires `POST /v1/old` in favour of `POST /v1/new`, and the allow-list still names
the old path, then:

- the new method is refused by the broker before it ever reaches the partner, and
- the retired one is happily forwarded to an endpoint that no longer exists.

Both directions are broken, and neither shows up in a build or in the review gate.
Hand your reviewer the new method-path list as part of the update, and treat the
broker registration as a release-blocking step, not a follow-up. See
[`MANAGED-KEY.md`](MANAGED-KEY.md) for the design.

A useful sanity check before you ship: call the retired route directly against the
partner API. If it 404s, every existing install of your app is already failing,
which raises the urgency of the update and tells you the allow-list swap has to
land with it.

## Step 7 — merge, then downstream

With the artifacts live and verified, merge the submission PR. That drives the
catalogue entry, and the store page follows from the catalogue. Both are covered
in [`PUBLISHING-PLAYBOOK.md`](PUBLISHING-PLAYBOOK.md); nothing about them changes
what you built here.

Once the catalogue is live, clients pick the update up with:

```sh
pilotctl appstore outdated
pilotctl appstore upgrade <id>
```

## Gotchas

Things that are true, non-obvious, and have cost real time.

- **`make package` is single-platform.** Covered above, repeated here because the
  failure is invisible until someone on a different OS tries to install.

- **A submission cannot carry a changelog.** There is no `changelog` field on a
  submission's `listing`, and it is not mapped through to the generated store
  page — `scaffold.Listing.Changelog` is reachable only from a `pilot.app.yaml`.
  A rich submission therefore publishes with a generated placeholder
  (`Released v<version>`) unless the notes are supplied separately. If your update
  has a story worth telling — and a breaking change always does — write the notes
  out explicitly and make sure they reach the store page.

- **A republish reuses the existing store page.** For an app that already has one,
  the publish path refreshes the runtime facts and the demo and next-steps graph,
  but **keeps the existing description, tagline, method list and keywords**. For a
  routine version bump that is correct and avoids churn. For a pivot it ships a
  half-updated page: a new demo sitting beside prose describing methods that no
  longer exist. If your update changes what the app *is*, say so on the PR so the
  page is regenerated rather than refreshed.

- **`managed` apps get a `<ns>.balance` method injected automatically.** It is
  wired to the broker's credit-ledger route, which only answers for apps that
  carry a credit block. A managed app **without** one ships a `balance` method
  that always returns 403 — exposed in the manifest and listed in `<ns>.help`.
  Nothing in the build or the review gate catches it. If your app has no per-user
  budget, know that the method is there and that it does not work.

- **Retired upstream routes fail quietly.** When a partner API retires an endpoint,
  the app in the catalogue keeps installing, keeps spawning and keeps passing every
  signature and sha check. Only the calls fail. Nothing in the publishing pipeline
  watches partner endpoints, so this is on you to notice.

- **The registry is write-once, and that is a feature.** You cannot fix a bad
  upload in place. Bump the version. The upside is that the previous release stays
  byte-identical and installable, so a rollback is a catalogue change rather than a
  rebuild.

## Pre-flight checklist

- [ ] version bumped in the single source of truth for your spec shape
- [ ] the key you are signing with derives the `publisher` pin already in the catalogue
- [ ] all four platforms built with the canonical builder, not `make package`
- [ ] `pilot-app verify` green on **each** of the four bundles
- [ ] `verify-submission` and `verify-update` green
- [ ] bundles uploaded under `bundles/<id>/<version>/`, plus any native assets under
      their own version prefix
- [ ] every public URL fetched back and sha-matched, and the gate re-run on the
      served bytes
- [ ] every method exercised against the **published** artifact, on the host OS and
      the other one
- [ ] retired methods confirmed gone from `<ns>.help` and no longer answering
- [ ] `managed` apps: new method paths handed over for the broker allow-list, and
      the swap landing with this release
- [ ] changelog notes written out, since a submission cannot carry them
- [ ] if the app pivoted, the store page flagged for regeneration rather than refresh
- [ ] publisher key still backed up — every future version needs this same key
