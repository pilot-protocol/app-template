#!/usr/bin/env bash
# publish-submission.sh <submissions/<id>-dir>
# Releases a merged submission's per-platform bundles on pilot-protocol/catalog
# and opens a catalogue.json PR on pilot-protocol/pilotprotocol. Invoked by
# publish-on-merge.yml with GH_TOKEN = CATALOG_PUBLISH_TOKEN. Idempotent on tag.
set -euo pipefail

DIR="$1"
META="$DIR/submission.json"
test -f "$META" || { echo "no submission.json in $DIR"; exit 1; }

ID="$(jq -r .id "$META")"
VERSION="$(jq -r .version "$META")"
NS="$(jq -r .namespace "$META")"
DESC="$(jq -r .description "$META")"

CATALOG_REPO="pilot-protocol/catalog"
PLATFORM_REPO="pilot-protocol/pilotprotocol"
TAG="${NS}-v${VERSION}"
REL_BASE="https://github.com/${CATALOG_REPO}/releases/download/${TAG}"

# The linux/amd64 primary backs the top-level bundle_url (what pre-v3 clients
# fetch). Newer submissions also carry a .bundles map of every platform's
# {file,sha256}; older ones have only .bundle/.bundle_sha256 (single platform).
PRIMARY_FILE="$(jq -r .bundle "$META")"
SHA="$(jq -r .bundle_sha256 "$META")"
BUNDLE_URL="${REL_BASE}/${PRIMARY_FILE}"

# Every asset to release: the .bundles map's files, or just .bundle when absent.
mapfile -t FILES < <(jq -r 'if (.bundles // {}) | length > 0 then (.bundles | to_entries[] | .value.file) else .bundle end' "$META")

# Re-run the review gate on every platform bundle before touching any org repo —
# the platform repo must never get an entry for a bundle that fails verification.
for f in "${FILES[@]}"; do
  test -f "$DIR/$f" || { echo "missing bundle $DIR/$f"; exit 1; }
  echo "==> verifying $f before publish"
  "${PILOT_APP_BIN:-pilot-app}" verify "$DIR/$f"
  cp "$DIR/$f" "/tmp/$f"
done

echo "==> releasing $ID v$VERSION on $CATALOG_REPO ($TAG) — ${#FILES[@]} platform asset(s)"
if gh release view "$TAG" -R "$CATALOG_REPO" >/dev/null 2>&1; then
  for f in "${FILES[@]}"; do gh release upload "$TAG" "/tmp/$f" -R "$CATALOG_REPO" --clobber; done
else
  ASSET_PATHS=(); for f in "${FILES[@]}"; do ASSET_PATHS+=("/tmp/$f"); done
  gh release create "$TAG" "${ASSET_PATHS[@]}" -R "$CATALOG_REPO" -t "$ID v$VERSION" -n "Pilot app-store bundle for $ID"
fi

# Catalogue bundles map: platform -> {bundle_url, bundle_sha256} (release URLs).
BUNDLES_JSON="$(jq -c --arg base "$REL_BASE" '
  (.bundles // {}) | to_entries
  | map({key: .key, value: {bundle_url: ($base + "/" + .value.file), bundle_sha256: .value.sha256}})
  | from_entries
' "$META")"
CATVER=2; [ "$(jq -r 'length' <<<"$BUNDLES_JSON")" -gt 0 ] && CATVER=3

BUNDLE_BYTES="$(wc -c < "$DIR/$PRIMARY_FILE" | tr -d ' ')"
MDSRC="$DIR/metadata.json"   # the v2 store-page record, emitted by `pilot-app submit`

echo "==> updating catalogue (v$CATVER) on $PLATFORM_REPO via PR"
WORK="$(mktemp -d)"
gh repo clone "$PLATFORM_REPO" "$WORK/platform" -- --depth 1 >/dev/null 2>&1
cd "$WORK/platform"
git config user.name "Alex Godoroja"
git config user.email "alex@vulturelabs.io"
# gh sets origin to a plain https URL with no creds, so the raw `git push` below
# can't authenticate. Embed the token (GH_TOKEN = CATALOG_PUBLISH_TOKEN).
git remote set-url origin "https://x-access-token:${GH_TOKEN}@github.com/${PLATFORM_REPO}.git"
BRANCH="catalogue/${ID}-${VERSION}"
git checkout -b "$BRANCH"

CAT="catalogue/catalogue.json"
APPDIR="catalogue/apps/${ID}"
META_URL="https://raw.githubusercontent.com/${PLATFORM_REPO}/main/${APPDIR}/metadata.json"

if [ -f "$MDSRC" ]; then
  # v2/v3 listing: publish the per-app metadata.json + a fully-populated entry.
  mkdir -p "$APPDIR"
  cp "$MDSRC" "$APPDIR/metadata.json"
  META_SHA="$(shasum -a 256 "$APPDIR/metadata.json" | awk '{print $1}')"
  DISPLAY="$(jq -r '.display_name // ""' "$MDSRC")"
  VENDOR="$(jq -r '.vendor.name // ""' "$MDSRC")"
  LICENSE="$(jq -r '.license // ""' "$MDSRC")"
  SOURCE="$(jq -r '.source_url // ""' "$MDSRC")"
  jq --arg id "$ID" --arg v "$VERSION" --arg d "$DESC" --arg u "$BUNDLE_URL" --arg s "$SHA" \
     --argjson sz "$BUNDLE_BYTES" --arg dn "$DISPLAY" --arg ven "$VENDOR" --arg lic "$LICENSE" \
     --arg src "$SOURCE" --arg mu "$META_URL" --arg ms "$META_SHA" \
     --argjson ver "$CATVER" --argjson bundles "$BUNDLES_JSON" \
     --slurpfile md "$MDSRC" '
    (.version = ([(.version // 0), $ver] | max)) |
    .apps = ((.apps // []) | map(select(.id != $id))) + [(
      {
        id: $id, version: $v, description: $d, bundle_url: $u, bundle_sha256: $s,
        display_name: $dn, vendor: $ven, categories: ($md[0].categories // []),
        bundle_size: $sz, source_url: $src, license: $lic,
        metadata_url: $mu, metadata_sha256: $ms
      }
      + (if ($bundles | length) > 0 then {bundles: $bundles} else {} end)
    )]
  ' "$CAT" > "$CAT.tmp" && mv "$CAT.tmp" "$CAT"
  git add "$CAT" "$APPDIR/metadata.json"
else
  echo "warning: no metadata.json in submission — writing a basic entry (no rich store page)"
  jq --arg id "$ID" --arg v "$VERSION" --arg d "$DESC" --arg u "$BUNDLE_URL" --arg s "$SHA" \
     --argjson ver "$CATVER" --argjson bundles "$BUNDLES_JSON" '
    (.version = ([(.version // 0), $ver] | max)) |
    .apps = ((.apps // []) | map(select(.id != $id))) + [(
      {id: $id, version: $v, description: $d, bundle_url: $u, bundle_sha256: $s}
      + (if ($bundles | length) > 0 then {bundles: $bundles} else {} end)
    )]
  ' "$CAT" > "$CAT.tmp" && mv "$CAT.tmp" "$CAT"
  git add "$CAT"
fi

git commit -m "catalogue: ${ID} v${VERSION}"
git push -u origin "$BRANCH"
gh pr create -R "$PLATFORM_REPO" --base main --head "$BRANCH" \
  --title "catalogue: ${ID} v${VERSION}" \
  --body "Automated catalogue update for ${ID} v${VERSION}. Primary bundle: ${BUNDLE_URL} (sha256 ${SHA}). Platforms: $(jq -r 'keys | join(\", \")' <<<\"$BUNDLES_JSON\"). CI verifies; human approves per APP-PUBLISHING-SPEC §7.2."
echo "==> done: $ID v$VERSION ($CATVER, ${#FILES[@]} asset(s))"
