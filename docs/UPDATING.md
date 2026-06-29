# Updating a published app — ship a new version

Publishing your app the first time is covered by [`PUBLISHING.md`](PUBLISHING.md).
This is the other half: shipping **v1.0.1** of an app that already lives in the
catalogue. The rules are designed so the only thing you ever change by hand is the
**version**, and so that **only you** — the original publisher — can update your app.

## The one rule: bump `app_version`

`app_version` in `pilot.app.yaml` is the **single source of truth**. Every other
version in the system is derived from it or gate-checked to equal it:

```
app_version (pilot.app.yaml)          ← you edit ONLY this
  → manifest.json app_version          (baked + signed)
  → install.json version               (asset staging spec)
  → asset URLs  …/<id>/<app_version>/<os>-<arch>/<file>   (DERIVED)
  → submission.json version            (written by pilot-app)
  → catalogue entry version            (gate: must == manifest)
```

Because asset URLs are **derived** from `app_version` (you give `file:`, not a full
`url:`), the artifact path can never drift from the adapter version. A registry
`url:` whose version segment disagrees with `app_version` is rejected by the gate.

## The easy path (PR to this repo)

```sh
# 1. bump the single source of truth (and re-scaffold so any changed
#    methods/entrypoints/backend regenerate):
pilot-app update -c pilot.app.yaml --bump patch -o ./my-app
#    (or --bump minor|major, or --set-version 1.2.0)

# 2. if your app ships native binaries, upload the new ones for this version
#    (see docs/R2-ARTIFACT-REGISTRY.md). The derived URLs already point at the
#    new <app_version>/ prefix — write-once, so a new version is a new prefix.

cd ./my-app
make package        # rebuild EVERY platform, re-signed with your EXISTING key
pilot-app submit -C . --prepare /path/to/your/app-template-fork

# 3. commit + PR exactly like the first publish:
cd /path/to/your/app-template-fork
git add submissions/io.pilot.<name> && git commit -m "update io.pilot.<name> v<ver>"
gh pr create        # against pilot-protocol/app-template
```

That's it. Merging the PR opens the catalogue PR automatically, the same as a first
publish — you do not touch the catalogue repo.

## What the CI gate enforces (so your PR "just works")

The required `submission-validate` check runs `pilot-app verify-update` against the
**live, signature-verified catalogue**. For an app id that already exists it
requires, with clear errors if not:

1. **The version didn't go backwards.** A downgrade is rejected; a same-version
   re-publish by the owner is allowed (idempotent). Normally you bump it.
2. **The same publishing key.** Your bundle must be signed by the **same ed25519
   key** that owns the app (the `publisher` pin in the catalogue). A bundle signed
   by any other key is rejected:
   > `io.pilot.foo is owned by ed25519:AAA…; this bundle is signed by ed25519:BBB…
   > — updates must be signed by the original publisher key`
3. **No artifact drift.** Every registry asset URL embeds `app_version`.

There is **no password or stored secret** for this. Ownership is proven simply by
signing the bundle with the key whose public half is already pinned in the
catalogue — keep your `*-publisher.key` safe; it *is* your update credential.

## The form path (`pilotprotocol.network/publish`)

The website calls `POST /api/update` (the update counterpart of `/api/submit`).
Form-path bundles are signed by the **Pilot platform key**, so updates to a
form-published app re-sign with the same platform key and pass automatically. The
server runs the same ownership gate, so the form can **never** override an app that
was published by a third party with their own key (and vice versa).

## Ownership: the key that first publishes owns all updates

- **Pilot-owned apps** are published via the form → owned by the platform key →
  updated via the form.
- **Third-party apps** should be published via the **PR path with your own key**
  from day one, so you — not the platform — own all future updates.

An app's update path is fixed to whichever key first published it. Moving an app
between key owners (key loss, or a handoff) is an **admin-gated rotation**: from the
publish-server admin page (`/admin?token=…`) the **Rotate a publisher key** form
(or `POST /admin/rotate-key` with `{id, new_publisher}`) opens a re-signed catalogue
PR that re-points the `publisher` pin. Holding the admin token authorizes rotating
any app's key. **After that PR merges, the new owner must publish an update signed
with the new key** before existing installs re-validate against the new pin (the
runtime trust anchor follows the pin). Server needs `CATALOG_PUBLISH_TOKEN` +
`CATALOG_SIGN_KEY`.

## Clients pick up the new version

```sh
pilotctl appstore outdated            # lists installed apps with a newer catalogue version
pilotctl appstore upgrade io.pilot.<name>   # re-installs the current version (verified)
pilotctl appstore upgrade --all
```

`upgrade` re-runs the same verified install (catalogue-sha + manifest-sha +
publisher trust-anchor); the supervisor then applies the on-disk version bump
(refusing any downgrade) and restarts the app.
