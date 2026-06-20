#!/usr/bin/env bash
# Full publish-to-usable e2e for a MANAGED app (Partner). Proves the whole
# chain the admin board triggers when you press "publish":
#
#   POST /api/submit  → the server BUILDS the keyless translation-layer adapter
#   POST /admin/approve → REGISTERS the app with the broker (routable + metered)
#   run the REAL built adapter → it signs with a daemon-format identity.json,
#     the broker verifies the caller, injects the master key, forwards to the
#     partner, meters, and enforces the per-caller rate limit.
#
# Mock partner by default. To hit the real Partner API:
#   MANAGED_REAL=1 PARTNER_API_KEY=sk-... ./scripts/e2e-partner.sh
set -euo pipefail
cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$WORK"' EXIT
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

MOCK=127.0.0.1:8231
BROKER=127.0.0.1:8230
PUB=127.0.0.1:8232
MASTER="MASTER-123"

echo "building pilot-app, publish-server, broker, broker-sign, ipc-call…"
go build -o "$WORK/publish-server" ./cmd/publish-server
go build -o "$WORK/broker"         ./cmd/broker
go build -o "$WORK/broker-sign"    ./cmd/broker-sign
go build -o "$WORK/ipc-call"       ./cmd/ipc-call

# ── partner API ────────────────────────────────────────────────────────────
if [ "${MANAGED_REAL:-0}" = "1" ]; then
  UPSTREAM="https://api.example.com"; MASTER="${PARTNER_API_KEY:?set PARTNER_API_KEY for real mode}"
  echo "using REAL Partner at $UPSTREAM"
else
  cat >"$WORK/partner.go" <<'GO'
package main
import ("net/http";"os")
func main() {
  http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    if r.Header.Get("x-api-key") != "MASTER-123" { w.WriteHeader(401); w.Write([]byte(`{"error":"bad key"}`)); return }
    w.Header().Set("Content-Type","application/json")
    w.Write([]byte(`{"email":"ceo@acme.com","cost_cents":2}`))
  })
  http.ListenAndServe(os.Getenv("ADDR"), nil)
}
GO
  ADDR=$MOCK go run "$WORK/partner.go" & sleep 1
  UPSTREAM="http://$MOCK"
fi

# ── 1. admin board: submit (builds the keyless adapter) ─────────────────────
ADMIN_TOKEN=dev-admin BROKER_REGISTRY="$WORK/apps.json" ALLOWED_ORIGINS="*" \
  "$WORK/publish-server" -addr "$PUB" -store "$WORK/store" -key "$WORK/platform.key" & sleep 1

cat >"$WORK/submission.json" <<JSON
{
  "id":"io.pilot.partner","version":"0.1.0",
  "description":"Partner enrichment via the Pilot managed key.",
  "email":"alex@vulturelabs.io",
  "backend":{"base_url":"$UPSTREAM","auth":"managed","quota":1,"headers":[{"name":"x-api-key","value":"managed"}]},
  "methods":[
    {"name":"partner.find-email","description":"Find an email by domain.","latency":"med","http":{"verb":"GET","path":"/find-email"},"params":[{"name":"domain","type":"string","required":true}]},
    {"name":"partner.enrich","description":"Enrich a person or company.","latency":"slow","http":{"verb":"POST","path":"/enrich"}}
  ],
  "listing":{"display_name":"Partner","app_description":"Enrichment.","license":"MIT"},
  "vendor":{"name":"Partner"}
}
JSON

resp=$(curl -s -X POST "http://$PUB/api/submit" -H 'Content-Type: application/json' --data @"$WORK/submission.json")
case_id=$(echo "$resp" | sed -n 's/.*"case_id":"\([^"]*\)".*/\1/p')
[ -n "$case_id" ] && pass "admin submit built the bundle (case $case_id)" || fail "submit failed: $resp"

# ── 2. admin board: approve (registers with the broker) ─────────────────────
# (publish trigger no-ops without a token; registration happens regardless.)
curl -s -X POST "http://$PUB/admin/approve" \
  --data-urlencode "id=$case_id" --data-urlencode "guide=Search 'Partner' in the store" \
  --data-urlencode "token=dev-admin" >/dev/null

grep -q '"id": "io.pilot.partner"' "$WORK/apps.json" \
  && pass "admin approve registered the app with the broker" || fail "broker registry not written: $(cat "$WORK/apps.json" 2>/dev/null)"
grep -q '"quota": 1' "$WORK/apps.json" && pass "publish-time rate limit recorded (quota 1)" || fail "quota not registered"

# ── 3. extract the REAL built adapter from the approved bundle ───────────────
bundle=$(find "$WORK/store" -name '*.tar.gz' | head -1)
[ -n "$bundle" ] || fail "no bundle produced by the admin board"
mkdir -p "$WORK/app" && tar -xzf "$bundle" -C "$WORK/app"
adapter=$(find "$WORK/app" -type f -perm -u+x -name '*-app' | head -1)
manifest=$(find "$WORK/app" -name manifest.json | head -1)
[ -n "$adapter" ] && pass "extracted the built adapter binary ($(basename "$adapter"))" || fail "no adapter binary in bundle"
grep -q 'broker.pilotprotocol.network' "$manifest" && pass "adapter manifest dials the broker, not the partner" || fail "manifest not broker-pointed"

# ── 4. boot the broker on the admin-written registry ────────────────────────
PARTNER_MASTER_KEY="$MASTER" "$WORK/broker" -registry "$WORK/apps.json" -addr "$BROKER" & sleep 1
curl -fsS "http://$BROKER/gw/health" >/dev/null && pass "broker up on the registered app" || fail "broker health failed"

# ── 5. run the real adapter; it signs with a daemon-format identity.json ─────
"$WORK/broker-sign" -gen-identity "$WORK/identity.json"
sock="$WORK/app.sock"
PARTNER_BACKEND_URL="http://$BROKER/io.pilot.partner" \
  "$adapter" --socket "$sock" --manifest "$manifest" --identity "$WORK/identity.json" & sleep 1
[ -S "$sock" ] && pass "adapter spawned and serving IPC" || fail "adapter socket not up"

# 1st call: real adapter → broker (verifies signature) → partner → metered.
out=$("$WORK/ipc-call" -socket "$sock" -method partner.find-email -args '{"domain":"acme.com"}' 2>&1) || true
echo "$out" | grep -q 'ceo@acme.com' \
  && pass "managed call works end-to-end (adapter signs → broker → partner): $out" \
  || fail "first call failed: $out"

# 2nd call: same caller, quota 1 → rate-limited by the broker.
out2=$("$WORK/ipc-call" -socket "$sock" -method partner.find-email -args '{"domain":"acme.com"}' 2>&1) || true
echo "$out2" | grep -qiE 'quota|429' \
  && pass "rate limit enforced on the 2nd call ($out2)" \
  || fail "rate limit NOT enforced, got: $out2"

# 6. metering recorded for the caller.
curl -s "http://$BROKER/gw/usage" | grep -q '"calls":1' \
  && pass "broker metered exactly 1 successful call" || fail "metering wrong: $(curl -s http://$BROKER/gw/usage)"

echo
echo "partner e2e: publish → build → register → sign → broker → partner → meter + ratelimit ✓"
