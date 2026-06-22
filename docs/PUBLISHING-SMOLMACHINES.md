# Publishing Smol Machines (io.pilot.smolmachines) to the Pilot app store

End-to-end runbook: build every platform artifact, host them in the R2 registry,
produce the catalogue entry, and land it. The app is a passthrough cli adapter
over the `smolvm` binary (no enumerated methods).

## 0. Identity & descriptions (fixed)

- **id:** `io.pilot.smolmachines`  ·  **version:** `1.2.0`  ·  **namespace/method prefix:** `smolmachines`
- **command:** `smolvm`  ·  **method:** `smolmachines.exec` (passthrough) + auto `smolmachines.help`
- **short** (catalogue / `appstore list`): the one-liner.
- **long** (`appstore view` → metadata `description_md`): the full bullet description.
- See `submissions/io.pilot.smolmachines/submission.json` for the exact text.

## 1. Build all platform artifacts (the binaries the publisher uploads)

smolvm releases per-platform tarballs. Re-host them in the registry under the app id:

```bash
for p in "darwin arm64 arm64" "linux arm64 arm64" "linux amd64 x86_64"; do
  set -- $p; OS=$1 PARCH=$2 SARCH=$3
  T=smolvm-1.2.0-$OS-$SARCH.tar.gz
  gh release download v1.2.0 --repo smol-machines/smolvm --pattern "$T" --clobber
  aws s3 cp "$T" "s3://pilot-artifacts-prod/io.pilot.smolmachines/1.2.0/$OS-$PARCH/$T" \
    --endpoint-url=https://<ACCOUNT_ID>.r2.cloudflarestorage.com
done
```
Record each `sha256` (computed in the browser at upload time, or `shasum -a 256`).
The artifact for each platform is the **tar.gz** with `unpack: tar.gz`,
`exec_path: smolvm-1.2.0-<os>-<arch>/smolvm`, `order: 1`.

> Note: smolvm needs the whole tarball (wrapper + `smolvm-bin` + `lib/` + sparse VM
> images), not just one file — hence `unpack: tar.gz`.

## 2. Submit + build the adapter bundle

POST the submission to the publish-server (or drive the website Artifacts step):

```bash
curl -X POST $PUBLISH_API/api/submit -H 'Content-Type: application/json' \
  --data @submissions/io.pilot.smolmachines/submission.json   # → {case_id, status:submitted}
# admin builds (per platform): scaffold adapter → sign manifest → emit install.json/install.sh → self-verify
```
`BuildBundle` cross-compiles the adapter for all four targets and **self-verifies each
through the catalogue gate** — a green build IS the §7.1 preflight passing.

Each bundle contains: signed `manifest.json` (`proc.exec→smolvm`, `net.dial <r2-host>`,
`fs.write $APP`, `fs.read $APP/install.json`, `protection: guarded`), `bin/smolvm-app`,
`install.json` (prod R2 URLs + shas), `install.sh`.

## 3. Host the bundles + metadata, build the catalogue entry

```bash
# bundles
for PLAT in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
  aws s3 cp io.pilot.smolmachines-1.2.0-$PLAT.tar.gz \
    s3://pilot-artifacts-prod/bundles/io.pilot.smolmachines/1.2.0/ --endpoint-url=$EP
done
# rich metadata.json (carries the LONG description_md)
aws s3 cp metadata.json s3://pilot-artifacts-prod/catalogue/apps/io.pilot.smolmachines/metadata.json --endpoint-url=$EP
```

Catalogue v2 entry (the line that lands in the platform catalogue):
```json
{ "id":"io.pilot.smolmachines", "version":"1.2.0",
  "description":"<SHORT one-liner>",
  "display_name":"Smol Machines", "vendor":"smol machines", "license":"Apache-2.0",
  "source_url":"https://github.com/smol-machines/smolvm",
  "bundle_url":"<prod R2>/bundles/.../io.pilot.smolmachines-1.2.0-linux-amd64.tar.gz",
  "bundle_sha256":"<linux/amd64 tarball sha>",
  "bundles": { "darwin/arm64":{...}, "darwin/amd64":{...}, "linux/arm64":{...}, "linux/amd64":{...} },
  "metadata_url":"<prod R2>/catalogue/apps/io.pilot.smolmachines/metadata.json",
  "metadata_sha256":"<metadata.json sha>" }
```
- `description` (short) → `appstore list`.  `metadata.description_md` (long) → `appstore view`.

## 4. Sign + land the catalogue entry

The catalogue is signature-gated; pilotctl verifies `<catalogue>.sig` against the
**embedded release catalogue key**. In production this is done by the publish
automation (app-template#28 auto-signs with the `CATALOG_SIGN_KEY` CI secret) when
you **Approve** the case — it opens the one-line catalogue PR on the platform repo
(`TeoSlayer/pilotprotocol` → `catalogue/catalogue.json`). Merge that PR and hosts
pick it up on next `pilotctl appstore catalogue`.

Manual/local signing (testing only) requires a pilotctl built with your key:
`pilotctl appstore sign-catalogue --key <key> catalogue.json`.

## 5. Install + verify on a host

```bash
pilotctl appstore catalogue | grep smolmachines           # short description shows here
pilotctl appstore view io.pilot.smolmachines              # long description_md shows here
pilotctl appstore install io.pilot.smolmachines           # fetch+verify+stage from R2
pilotctl appstore call io.pilot.smolmachines smolmachines.exec \
  '{"args":["machine","run","--net","--image","alpine","--","echo","hi"]}'
```

## Prerequisites (must be deployed first — see R2-PREDEPLOY-REPORT.md)

1. Daemon on the **proc.exec** app-store version (pilotprotocol#317 → app-store#24).
2. pilotctl carries `install.json`/`install.sh` on install + daemon wires
   `TrustedPublishers` (pilotprotocol#318). Without #2 the trust anchor rejects every app.
3. R2 bucket **CORS** for browser uploads; publish-server R2 env set.
