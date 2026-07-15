#!/usr/bin/env bash
# publish-rich-from-r2.sh <submissions/<id>-dir>
#
# Publishes a RICH submission (backend/methods spec, no pre-built .bundle
# pointer) whose per-platform bundles have ALREADY been built + published to R2
# by the publisher's own toolchain (`pilot-app submit` / publish-api). This is
# the path publish-on-merge.yml takes for rich submissions instead of silently
# no-op'ing them (the #289/#306-era gap that stranded five merged apps off the
# catalogue since ~Jul 7).
#
# It DERIVES the catalogue entry from what is live on R2:
#   - per-platform bundle_url + bundle_sha256 (+ size) by fetching each artifact,
#   - the publisher pin from the primary bundle's signed manifest (store.publisher),
#   - a v2 metadata.json (store page) synthesised from submission.json,
# then SIGNS catalogue.json with CATALOG_SIGN_KEY (held as a CI secret / on the
# publish VM — the same key publish-submission.sh uses) and opens the catalogue
# PR on pilot-protocol/pilotprotocol. The signature is produced where the key
# lives; nothing is published unsigned.
#
# If NO platform bundle is on R2 for this id@version, it FAILS LOUDLY (a red
# check + ::error:: annotation + job-summary line) rather than exiting 0 — a
# merged rich app that never got built must be visible, not silently dropped.
#
# Env:
#   GH_TOKEN           CATALOG_PUBLISH_TOKEN (contents+pull_requests on pilotprotocol)
#   CATALOG_SIGN_KEY   hex ed25519 catalogue key (pub half == embedded trust anchor)
#   R2_PUBLIC_BASE     override the R2 read base (default: prod bucket)
#   PILOT_APP_BIN      pilot-app binary (default: pilot-app)
#   DRY_RUN=1          construct + print the entry; skip signing, git, and PR
set -euo pipefail

DIR="${1%/}"
META="$DIR/submission.json"
test -f "$META" || { echo "no submission.json in $DIR"; exit 1; }

ID="$(jq -r '.id // empty' "$META")"
VERSION="$(jq -r '.version // empty' "$META")"
DESC="$(jq -r '.description // empty' "$META")"
[ -n "$ID" ] || { echo "ERROR: $META has no .id"; exit 1; }
[ -n "$VERSION" ] || { echo "ERROR: $META ($ID) has no .version"; exit 1; }

R2_BASE="${R2_PUBLIC_BASE:-https://pub-f09f9a4ea848491198d48e329ba030e3.r2.dev}"
R2_BASE="${R2_BASE%/}"
PLATFORM_REPO="pilot-protocol/pilotprotocol"

# ── loud-notify helper: surfaces on the GitHub run (annotation + job summary) ──
notify_fail() {
  local msg="$1"
  echo "::error title=catalogue publish blocked::$msg"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "### ❌ catalogue NOT updated for \`$ID\` v$VERSION" >> "$GITHUB_STEP_SUMMARY"
    echo "" >> "$GITHUB_STEP_SUMMARY"
    echo "$msg" >> "$GITHUB_STEP_SUMMARY"
  fi
  echo "ERROR: $msg" >&2
}

# ── 1. discover which platform bundles are live on R2 ────────────────────────
PLATFORMS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)
WORKDL="$(mktemp -d)"
trap 'rm -rf "$WORKDL"' EXIT

# Records file: one "plat<TAB>url<TAB>sha<TAB>size" line per live platform, in
# PLATFORMS order (portable — no bash-4 associative arrays needed).
RECS="$WORKDL/records.tsv"; : > "$RECS"
for plat in "${PLATFORMS[@]}"; do
  dash="${plat/\//-}"                       # linux/amd64 -> linux-amd64
  file="${ID}-${VERSION}-${dash}.tar.gz"
  url="${R2_BASE}/bundles/${ID}/${VERSION}/${file}"
  code="$(curl -fsS -o "$WORKDL/$file" -w '%{http_code}' "$url" 2>/dev/null || true)"
  if [ "$code" = "200" ] && [ -s "$WORKDL/$file" ]; then
    sha="$(shasum -a 256 "$WORKDL/$file" | awk '{print $1}')"
    size="$(wc -c < "$WORKDL/$file" | tr -d ' ')"
    printf '%s\t%s\t%s\t%s\n' "$plat" "$url" "$sha" "$size" >> "$RECS"
    echo "==> found $plat  (${size} bytes, sha ${sha:0:12}…)"
  fi
done

found="$(wc -l < "$RECS" | tr -d ' ')"
if [ "$found" -eq 0 ]; then
  notify_fail "rich submission ${ID} v${VERSION} was merged but NO platform bundle is on R2 (${R2_BASE}/bundles/${ID}/${VERSION}/). Its bundles must be built + uploaded by the publisher toolchain (pilot-app submit / publish-api) before it can enter the catalogue. Publish it, then re-run this workflow (workflow_dispatch: submission=${DIR})."
  exit 1
fi

# Primary = linux/amd64 if present (pre-v3 clients read the top-level bundle_url),
# else the first live platform.
prim_line="$(awk -F'\t' '$1=="linux/amd64"' "$RECS" | head -1)"
[ -n "$prim_line" ] || prim_line="$(head -1 "$RECS")"
PRIMARY_PLATFORM="$(printf '%s' "$prim_line" | cut -f1)"
BUNDLE_URL="$(printf '%s' "$prim_line" | cut -f2)"
SHA="$(printf '%s' "$prim_line" | cut -f3)"
BUNDLE_BYTES="$(printf '%s' "$prim_line" | cut -f4)"
PRIMARY_FILE="${ID}-${VERSION}-${PRIMARY_PLATFORM/\//-}.tar.gz"

# ── 2. publisher pin from the primary bundle's signed manifest ───────────────
PUBLISHER="$(tar -xzOf "$WORKDL/$PRIMARY_FILE" ./manifest.json 2>/dev/null | jq -r '.store.publisher // empty' || true)"
[ -n "$PUBLISHER" ] || PUBLISHER="$(tar -xzOf "$WORKDL/$PRIMARY_FILE" manifest.json 2>/dev/null | jq -r '.store.publisher // empty' || true)"
if [ -z "$PUBLISHER" ]; then
  notify_fail "could not read store.publisher from ${PRIMARY_FILE} manifest — a catalogue entry without a publisher pin is refused by the v1.12.3+ catalogue anchor. Aborting rather than shipping an unpinned entry."
  exit 1
fi

# ── 3. defense-in-depth: run the same verify gate CI/clients run ─────────────
if command -v "${PILOT_APP_BIN:-pilot-app}" >/dev/null 2>&1; then
  echo "==> verifying $PRIMARY_FILE"
  "${PILOT_APP_BIN:-pilot-app}" verify "$WORKDL/$PRIMARY_FILE"
fi

# ── 4. bundles map for the catalogue entry (primary facts set in §1) ─────────
BUNDLES_JSON="$(
  while IFS="$(printf '\t')" read -r plat url sha size; do
    [ -n "$plat" ] || continue
    jq -cn --arg k "$plat" --arg u "$url" --arg s "$sha" \
      '{key:$k, value:{bundle_url:$u, bundle_sha256:$s}}'
  done < "$RECS" | jq -cs 'from_entries'
)"

# ── 5. synthesise a v2 metadata.json store page from submission.json ─────────
# display_name: submission.display_name, else the vendor name, else the last
# dotted id segment title-cased. categories/license/source_url fall back sanely.
# NB: vendor.name is the PUBLISHER ("Pilot Protocol" for wrapped tools), not the
# app, so it is a poor display name. Prefer an explicit display_name, else derive
# from the id's last dotted segment (title-cased) — predictable and app-specific.
DISPLAY="$(jq -r '.display_name // (.id | split(".") | last | (.[0:1]|ascii_upcase) + .[1:])' "$META")"
VENDOR="$(jq -r '.vendor.name // ""' "$META")"
LICENSE="$(jq -r '.license // ""' "$META")"
SOURCE="$(jq -r '.source_url // "https://github.com/pilot-protocol/app-template/tree/main/submissions/'"$ID"'"' "$META")"
CATEGORIES_JSON="$(jq -c '.categories // []' "$META")"

# NB: jq's object-construction value grammar is stricter than a full pipe, and
# some jq builds reject `key: (A) + {..}` / `key: A // {..}` inline. Precompute
# the composite sub-objects with plain jq and pass them in as --argjson so every
# value below is a single term — portable across jq 1.6/1.7.
VENDOR_JSON="$(jq -c --arg dn "$DISPLAY" --arg pub "$PUBLISHER" \
  '((.vendor // {name: $dn}) + {publisher_pubkey: $pub})' "$META")"
METHODS_JSON="$(jq -c '[ (.methods // [])[] | {name: .name, summary: (.summary // .description // "")} ]' "$META")"
# The product demo (example-driven "Full usage demo") rides verbatim into
# metadata.json when the submission carries one; null → omitted. See
# docs/PRODUCT-DEMOS.md.
DEMO_JSON="$(jq -c '.product_demo // null' "$META")"
METADATA_JSON="$(jq -n \
  --arg id "$ID" --arg dn "$DISPLAY" --arg desc "$DESC" \
  --arg src "$SOURCE" --arg lic "$LICENSE" \
  --argjson cats "$CATEGORIES_JSON" --argjson bb "$BUNDLE_BYTES" \
  --arg ver "$VERSION" --argjson vendor "$VENDOR_JSON" --argjson methods "$METHODS_JSON" \
  --argjson demo "$DEMO_JSON" '
  {
    schema_version: 1,
    id: $id,
    display_name: $dn,
    tagline: ($desc[0:120]),
    description_md: $desc,
    vendor: $vendor,
    source_url: $src,
    license: $lic,
    categories: $cats,
    size: {bundle_bytes: $bb},
    methods: $methods,
    changelog: [ {version: $ver, notes: (["Published v " + $ver])} ]
  }
  | if $demo != null then . + {product_demo: $demo} else . end')"

# ── 6. open the catalogue PR on pilotprotocol ────────────────────────────────
CATVER=2
if [ "${DRY_RUN:-}" = "1" ]; then
  echo "── DRY RUN: catalogue entry that would be added for $ID v$VERSION ──"
  jq -n --arg id "$ID" --arg v "$VERSION" --arg d "$DESC" --arg u "$BUNDLE_URL" --arg s "$SHA" \
     --argjson sz "$BUNDLE_BYTES" --arg dn "$DISPLAY" --arg ven "$VENDOR" --arg lic "$LICENSE" \
     --arg src "$SOURCE" --arg pub "$PUBLISHER" --argjson bundles "$BUNDLES_JSON" --argjson cats "$CATEGORIES_JSON" '
     {id:$id, version:$v, description:$d, bundle_url:$u, bundle_sha256:$s,
      display_name:$dn, vendor:$ven, categories:$cats, bundle_size:$sz,
      source_url:$src, license:$lic, bundles:$bundles, publisher:$pub}'
  echo "── DRY RUN: metadata.json ──"
  echo "$METADATA_JSON" | jq '{schema_version,id,display_name,license,categories,methods:(.methods|length)}'
  exit 0
fi

: "${GH_TOKEN:?CATALOG_PUBLISH_TOKEN required to open the catalogue PR}"
WORK="$(mktemp -d)"
gh repo clone "$PLATFORM_REPO" "$WORK/platform" -- --depth 1 >/dev/null 2>&1
cd "$WORK/platform"
git config user.name "Alex Godoroja"
git config user.email "alex@vulturelabs.io"
git remote set-url origin "https://x-access-token:${GH_TOKEN}@github.com/${PLATFORM_REPO}.git"
BRANCH="catalogue/${ID}-${VERSION}"
git checkout -b "$BRANCH"

CAT="catalogue/catalogue.json"
APPDIR="catalogue/apps/${ID}"
mkdir -p "$APPDIR"
# Reuse an already-authored rich store page if one exists (e.g. a version bump of
# an app that already has catalogue/apps/<id>/metadata.json) — refreshing only
# the runtime facts (publisher pubkey + primary bundle size). Otherwise write the
# store page synthesised from submission.json.
if [ -f "$APPDIR/metadata.json" ]; then
  echo "==> reusing existing $APPDIR/metadata.json (refreshing publisher + size + demo)"
  # Backfill/refresh the product demo from the submission onto the existing store
  # page too, so already-published apps pick up their "Full usage demo".
  jq --arg pub "$PUBLISHER" --argjson bb "$BUNDLE_BYTES" --argjson demo "$DEMO_JSON" \
     '.vendor = ((.vendor // {}) + {publisher_pubkey: $pub}) | .size = ((.size // {}) + {bundle_bytes: $bb}) | (if $demo != null then .product_demo = $demo else . end)' \
     "$APPDIR/metadata.json" > "$APPDIR/metadata.json.tmp" && mv "$APPDIR/metadata.json.tmp" "$APPDIR/metadata.json"
else
  echo "$METADATA_JSON" | jq '.' > "$APPDIR/metadata.json"
fi
META_SHA="$(shasum -a 256 "$APPDIR/metadata.json" | awk '{print $1}')"
META_URL="https://raw.githubusercontent.com/${PLATFORM_REPO}/main/${APPDIR}/metadata.json"

jq --arg id "$ID" --arg v "$VERSION" --arg d "$DESC" --arg u "$BUNDLE_URL" --arg s "$SHA" \
   --argjson sz "$BUNDLE_BYTES" --arg dn "$DISPLAY" --arg ven "$VENDOR" --arg lic "$LICENSE" \
   --arg src "$SOURCE" --arg mu "$META_URL" --arg ms "$META_SHA" --arg pub "$PUBLISHER" \
   --argjson ver "$CATVER" --argjson bundles "$BUNDLES_JSON" --argjson cats "$CATEGORIES_JSON" '
  (.version = ([(.version // 0), $ver] | max)) |
  .apps = ((.apps // []) | map(select(.id != $id))) + [(
    {
      id: $id, version: $v, description: $d, bundle_url: $u, bundle_sha256: $s,
      display_name: $dn, vendor: $ven, categories: $cats,
      bundle_size: $sz, source_url: $src, license: $lic,
      metadata_url: $mu, metadata_sha256: $ms
    }
    + (if ($bundles | length) > 0 then {bundles: $bundles} else {} end)
    + {publisher: $pub}
  )]
' "$CAT" > "$CAT.tmp" && mv "$CAT.tmp" "$CAT"
git add "$CAT" "$APPDIR/metadata.json"

# Sign catalogue.json so the PR is born verifiable (pilotctl fail-closes on an
# unsigned/invalid catalogue). CATALOG_SIGN_KEY == the embedded catalogtrust key.
CATABS="$(pwd)/$CAT"
if [ -n "${CATALOG_SIGN_KEY:-}" ]; then
  echo "==> signing $CAT"
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
  ( cd /tmp && go run /tmp/catsign.go "$CATABS" )
  git add "$CAT.sig"
else
  notify_fail "CATALOG_SIGN_KEY not set — cannot sign the catalogue; the PR would be unmergeable. Configure the secret and re-run."
  exit 1
fi

git commit -m "catalogue: ${ID} v${VERSION} (rich, from R2)"
git push -u origin "$BRANCH"
gh pr create -R "$PLATFORM_REPO" --base main --head "$BRANCH" \
  --title "catalogue: ${ID} v${VERSION}" \
  --body "Automated catalogue update for **${ID} v${VERSION}** (rich submission, bundles already on R2). Primary: ${BUNDLE_URL} (sha256 ${SHA}). Platforms: $(jq -r 'keys | join(\", \")' <<<\"$BUNDLES_JSON\"). Publisher: ${PUBLISHER}. catalogue.json re-signed with CATALOG_SIGN_KEY. CI verifies; human approves per APP-PUBLISHING-SPEC §7.2."
echo "==> done: $ID v$VERSION ($found platform asset(s))"
