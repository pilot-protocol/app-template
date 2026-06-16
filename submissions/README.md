# Submissions — the single front door for publishing an app

This is the one repo you touch to publish a Pilot app. You never need push access
to any org repo. Flow:

```
go install github.com/pilot-protocol/app-template/cmd/pilot-app@latest

pilot-app example > pilot.app.yaml      # describe your existing API
$EDITOR pilot.app.yaml
pilot-app init -o ./my-app
cd my-app
make gen-key                            # one-time ed25519 publisher key (keep it safe)
make package                            # build -> sha-pin -> sign -> tarball
pilot-app verify io.pilot.<name>-<ver>.tar.gz   # optional: run the gate locally

# fork this repo, then:
pilot-app submit -C . --prepare /path/to/your/app-template-fork
cd /path/to/your/app-template-fork
$EDITOR submissions/io.pilot.<name>/submission.json   # set the description
git add submissions/io.pilot.<name> && git commit -m "submit io.pilot.<name> v<ver>"
gh pr create     # against pilot-protocol/app-template
```

## What a submission contains

`submissions/<app-id>/`:
- `<app-id>-<version>.tar.gz` — the signed bundle (`manifest.json` + `bin/<binary>`).
- `submission.json` — `{id, version, namespace, description, bundle, bundle_sha256}`.

## What happens next

1. **CI** (`submission-validate`) runs `pilot-app verify` on your bundle: tarball
   sha, manifest validates + signature verifies, binary sha pinned, a `<ns>.help`
   method is exposed, id/version consistent.
2. **A maintainer** reviews per the PR checklist (grant proportionality, publisher
   identity, description accuracy).
3. **On merge**, automation publishes your bundle as a release on
   `pilot-protocol/catalog` and opens the catalogue-index PR on
   `TeoSlayer/pilotprotocol`. Once that merges, anyone can:
   ```
   pilotctl appstore install io.pilot.<name>
   ```

See `../docs/APP-PUBLISHING-SPEC.md` for the full standard flow.
