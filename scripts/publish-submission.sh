#!/usr/bin/env bash
# publish-submission.sh <submissions/<id>-dir>
# Releases a merged submission's bundle on pilot-protocol/catalog and opens a
# catalogue.json PR on TeoSlayer/pilotprotocol. Invoked by publish-on-merge.yml
# with GH_TOKEN = CATALOG_PUBLISH_TOKEN. Idempotent on the release tag.
set -euo pipefail

DIR="$1"
META="$DIR/submission.json"
test -f "$META" || { echo "no submission.json in $DIR"; exit 1; }

ID="$(jq -r .id "$META")"
VERSION="$(jq -r .version "$META")"
NS="$(jq -r .namespace "$META")"
DESC="$(jq -r .description "$META")"
BUNDLE="$DIR/$(jq -r .bundle "$META")"
SHA="$(jq -r .bundle_sha256 "$META")"
test -f "$BUNDLE" || { echo "missing bundle $BUNDLE"; exit 1; }

# Re-run the review gate before touching any org repo — the platform repo must
# never receive a catalogue entry for a bundle that doesn't pass verification.
echo "==> verifying $BUNDLE before publish"
"${PILOT_APP_BIN:-pilot-app}" verify "$BUNDLE"

CATALOG_REPO="pilot-protocol/catalog"
PLATFORM_REPO="TeoSlayer/pilotprotocol"
TAG="${NS}-v${VERSION}"
ASSET="${ID}-${VERSION}.tar.gz"
BUNDLE_URL="https://github.com/${CATALOG_REPO}/releases/download/${TAG}/${ASSET}"

echo "==> releasing $ID v$VERSION on $CATALOG_REPO ($TAG)"
cp "$BUNDLE" "/tmp/$ASSET"
if gh release view "$TAG" -R "$CATALOG_REPO" >/dev/null 2>&1; then
  gh release upload "$TAG" "/tmp/$ASSET" -R "$CATALOG_REPO" --clobber
else
  gh release create "$TAG" "/tmp/$ASSET" -R "$CATALOG_REPO" -t "$ID v$VERSION" -n "Pilot app-store bundle for $ID"
fi

echo "==> updating catalogue.json on $PLATFORM_REPO via PR"
WORK="$(mktemp -d)"
gh repo clone "$PLATFORM_REPO" "$WORK/platform" -- --depth 1 >/dev/null 2>&1
cd "$WORK/platform"
git config user.name "Alex Godoroja"
git config user.email "alex@vulturelabs.io"
BRANCH="catalogue/${ID}-${VERSION}"
git checkout -b "$BRANCH"

CAT="catalogue/catalogue.json"
jq --arg id "$ID" --arg v "$VERSION" --arg d "$DESC" --arg u "$BUNDLE_URL" --arg s "$SHA" '
  .apps = ((.apps // []) | map(select(.id != $id))) + [{
    id: $id, version: $v, description: $d, bundle_url: $u, bundle_sha256: $s
  }]
' "$CAT" > "$CAT.tmp" && mv "$CAT.tmp" "$CAT"

git add "$CAT"
git commit -m "catalogue: ${ID} v${VERSION}"
git push -u origin "$BRANCH"
gh pr create -R "$PLATFORM_REPO" --base main --head "$BRANCH" \
  --title "catalogue: ${ID} v${VERSION}" \
  --body "Automated catalogue update for ${ID} v${VERSION}. Bundle: ${BUNDLE_URL} (sha256 ${SHA}). CI verifies; human approves per APP-PUBLISHING-SPEC §7.2."
echo "==> done: $ID v$VERSION"
