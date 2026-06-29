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

# Re-enforce the UPDATE gate against the LIVE catalogue before touching any org
# repo: a new id is a first publish; an existing id must increase the version and
# (pointer path) be signed by the owning publisher key. This is the same gate the
# PR check runs — defense in depth at publish time, since the catalogue PR for
# this version has not merged yet, so the live catalogue still holds the prior
# owner+version. (No-ops harmlessly for a brand-new app.)
echo "==> update gate (same publisher key + higher version)"
"${PILOT_APP_BIN:-pilot-app}" verify-update "$DIR"

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
# `bundles` is a v2-OPTIONAL field, NOT a version bump. pilotctl's loadCatalogue
# fail-closes on any version != 1 && != 2, so emitting version 3 would make every
# client reject the whole catalogue. Per the catalogue's own design (optional
# fields, forward+backward compatible), keep version 2 and add bundles as a field.
CATVER=2

BUNDLE_BYTES="$(wc -c < "$DIR/$PRIMARY_FILE" | tr -d ' ')"

# Pin the publisher into the catalogue entry. v1.12.3's catalogue anchor
# fail-closes any entry without a `publisher` pin, so every published app MUST
# carry it. Source of truth is the bundle's signed manifest (store.publisher) —
# NOT metadata.json, whose publisher_pubkey can be a placeholder/stale.
PUBLISHER="$(tar -xzOf "$DIR/$PRIMARY_FILE" ./manifest.json 2>/dev/null | jq -r '.store.publisher // empty')"
[ -n "$PUBLISHER" ] || echo "WARNING: no store.publisher in $PRIMARY_FILE manifest — catalogue entry will be UNPINNED (refused on v1.12.3+ hosts)"

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
     --arg src "$SOURCE" --arg mu "$META_URL" --arg ms "$META_SHA" --arg pub "$PUBLISHER" \
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
      + (if $pub != "" then {publisher: $pub} else {} end)
    )]
  ' "$CAT" > "$CAT.tmp" && mv "$CAT.tmp" "$CAT"
  git add "$CAT" "$APPDIR/metadata.json"
else
  echo "warning: no metadata.json in submission — writing a basic entry (no rich store page)"
  jq --arg id "$ID" --arg v "$VERSION" --arg d "$DESC" --arg u "$BUNDLE_URL" --arg s "$SHA" \
     --argjson ver "$CATVER" --argjson bundles "$BUNDLES_JSON" --arg pub "$PUBLISHER" '
    (.version = ([(.version // 0), $ver] | max)) |
    .apps = ((.apps // []) | map(select(.id != $id))) + [(
      {id: $id, version: $v, description: $d, bundle_url: $u, bundle_sha256: $s}
      + (if ($bundles | length) > 0 then {bundles: $bundles} else {} end)
      + (if $pub != "" then {publisher: $pub} else {} end)
    )]
  ' "$CAT" > "$CAT.tmp" && mv "$CAT.tmp" "$CAT"
  git add "$CAT"
fi

# Sign catalogue.json so the PR is born verifiable. pilotctl fail-closes on an
# unsigned/invalid catalogue, so WITHOUT this the catalogue PR can't merge until
# someone re-signs offline (the old #289/#306 roadblock). CATALOG_SIGN_KEY is the
# hex-encoded 64-byte ed25519 catalogue key (must match the embedded catalogtrust
# pubkey); held as a repo secret and injected by publish-on-merge.yml.
CATABS="$(pwd)/$CAT"
if [ -n "${CATALOG_SIGN_KEY:-}" ]; then
  echo "==> signing $CAT (autonomous — no manual re-sign needed)"
  cat > /tmp/catsign.go <<'GOSIGN'
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"os"
)

func main() {
	key, err := hex.DecodeString(os.Getenv("CATALOG_SIGN_KEY"))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		os.Stderr.WriteString("CATALOG_SIGN_KEY must be a hex-encoded 64-byte ed25519 private key\n")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	sig := ed25519.Sign(ed25519.PrivateKey(key), data)
	if err := os.WriteFile(os.Args[1]+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}
GOSIGN
  # Run outside the pilotprotocol module dir so its go.mod doesn't shadow the helper.
  ( cd /tmp && go run /tmp/catsign.go "$CATABS" )
  git add "$CAT.sig"
else
  echo "WARNING: CATALOG_SIGN_KEY not set — catalogue PR will need a MANUAL re-sign before it can merge"
fi

git commit -m "catalogue: ${ID} v${VERSION}"
git push -u origin "$BRANCH"
gh pr create -R "$PLATFORM_REPO" --base main --head "$BRANCH" \
  --title "catalogue: ${ID} v${VERSION}" \
  --body "Automated catalogue update for ${ID} v${VERSION}. Primary bundle: ${BUNDLE_URL} (sha256 ${SHA}). Platforms: $(jq -r 'keys | join(\", \")' <<<\"$BUNDLES_JSON\"). CI verifies; human approves per APP-PUBLISHING-SPEC §7.2."
echo "==> done: $ID v$VERSION ($CATVER, ${#FILES[@]} asset(s))"
