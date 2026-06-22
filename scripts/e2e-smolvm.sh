#!/usr/bin/env bash
# e2e-smolvm.sh — end-to-end proof of native-app delivery from the Pilot R2
# artifact registry, using a real, complex CLI: smolvm (smol-machines/smolvm), a
# microVM runtime shipped as a tar.gz (wrapper script + binary + libs + images).
#
# Flow (mirrors what a publisher + a host actually do):
#   1. download smolvm's release tarball for THIS host                  (publisher has the artifact)
#   2. sha256 it and upload it to the R2 artifact registry (dev bucket) (the publish form's Artifacts step)
#   3. run the scaffold runtime e2e: build the generated cli adapter,   (pilotctl appstore install + call)
#      let it fetch+verify+extract the artifact from R2 and exec it
#
# Requirements: bash, curl/tar, aws CLI, go, and R2 S3 credentials in the env:
#   AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, R2_ENDPOINT, R2_BUCKET, R2_PUBLIC_BASE
# Sensible defaults target the pilot-artifacts-dev bucket.
set -euo pipefail

SMOLVM_VERSION="${SMOLVM_VERSION:-1.2.0}"
R2_ENDPOINT="${R2_ENDPOINT:?set R2_ENDPOINT to your account S3 endpoint}"
R2_BUCKET="${R2_BUCKET:-pilot-artifacts-dev}"
R2_PUBLIC_BASE="${R2_PUBLIC_BASE:-https://pub-2328865fa11041b8a5efba00b940ec14.r2.dev}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}"

if [[ -z "${AWS_ACCESS_KEY_ID:-}" || -z "${AWS_SECRET_ACCESS_KEY:-}" ]]; then
  echo "error: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (R2 S3 keys) in the env" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# --- 1. host platform → smolvm asset name + pilot os/arch tuple ---------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"      # darwin | linux
machine="$(uname -m)"
case "$machine" in
  arm64|aarch64) smol_arch=arm64; pilot_arch=arm64 ;;
  x86_64|amd64)  smol_arch=x86_64; pilot_arch=amd64 ;;
  *) echo "unsupported arch: $machine" >&2; exit 2 ;;
esac
dirname="smolvm-${SMOLVM_VERSION}-${os}-${smol_arch}"
tarball="${dirname}.tar.gz"
echo "==> host ${os}/${pilot_arch}; smolvm asset ${tarball}"

# --- 2. fetch the release tarball ---------------------------------------------
echo "==> downloading smolvm ${SMOLVM_VERSION}"
if command -v gh >/dev/null 2>&1; then
  gh release download "v${SMOLVM_VERSION}" --repo smol-machines/smolvm --pattern "$tarball" --dir "$work" --clobber
else
  curl -fsSL "https://github.com/smol-machines/smolvm/releases/download/v${SMOLVM_VERSION}/${tarball}" -o "$work/$tarball"
fi

sha="$(shasum -a 256 "$work/$tarball" | awk '{print $1}')"
echo "==> sha256=${sha}"

# --- 3. upload to the R2 artifact registry (the Artifacts step) ---------------
key="io.pilot.smolvm/${SMOLVM_VERSION}/${os}-${pilot_arch}/${tarball}"
echo "==> uploading to s3://${R2_BUCKET}/${key}"
aws s3 cp "$work/$tarball" "s3://${R2_BUCKET}/${key}" --endpoint-url="$R2_ENDPOINT" >/dev/null
public_url="${R2_PUBLIC_BASE}/${key}"

# verify the public URL serves the exact bytes we uploaded
echo "==> verifying public URL integrity"
got="$(curl -fsSL "$public_url" | shasum -a 256 | awk '{print $1}')"
[[ "$got" == "$sha" ]] || { echo "public URL sha mismatch: $got != $sha" >&2; exit 1; }
echo "    ok: ${public_url}"

# --- 4. run the install+call e2e against the live R2 object -------------------
echo "==> running adapter delivery e2e (build → fetch from R2 → verify → extract → exec)"
cd "$repo_root"
PILOT_E2E_ASSET_URL="$public_url" \
PILOT_E2E_ASSET_SHA256="$sha" \
PILOT_E2E_ASSET_EXECPATH="${dirname}/smolvm" \
PILOT_E2E_ASSET_CALLARG="--version" \
PILOT_E2E_ASSET_EXPECT="$SMOLVM_VERSION" \
  go test ./internal/scaffold/ -run TestR2AssetDeliveryE2E -v -count=1

echo "==> e2e OK: smolvm ${SMOLVM_VERSION} delivered from R2 and executed via the pilot cli adapter"
