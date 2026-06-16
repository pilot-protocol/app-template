#!/usr/bin/env bash
# validate-catalogue.sh — objective half of the app-store review gate (SPEC §7.1).
# Runs in CI on any PR that touches catalogue/catalogue.json. Builds pilot-app and
# verifies every entry: bundle reachable, sha matches, manifest valid + signed,
# binary sha pinned, <ns>.help present, id/version consistent, no downgrade.
#
# Usage: ci/validate-catalogue.sh [catalogue.json] [base-ref-catalogue.json]
set -euo pipefail

CATALOGUE="${1:-catalogue/catalogue.json}"
BASE="${2:-}"

# Build the verifier from the app-template tool. In the platform repo CI, fetch
# it; locally, point PILOT_APP_BIN at a prebuilt binary to skip the build.
if [[ -n "${PILOT_APP_BIN:-}" ]]; then
  BIN="$PILOT_APP_BIN"
else
  echo "building pilot-app verifier..."
  TMP="$(mktemp -d)"
  git clone --depth 1 https://github.com/pilot-protocol/app-template "$TMP/app-template"
  ( cd "$TMP/app-template" && go build -o "$TMP/pilot-app" ./cmd/pilot-app )
  BIN="$TMP/pilot-app"
fi

echo "verifying $CATALOGUE"
if [[ -n "$BASE" ]]; then
  PILOT_CATALOGUE_BASE="$BASE" "$BIN" verify "$CATALOGUE"
else
  "$BIN" verify "$CATALOGUE"
fi
