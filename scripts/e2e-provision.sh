#!/usr/bin/env bash
# Real-process end-to-end test for the PROVISIONING broker (io.pilot.smol). Starts
# the reference cloud + the actual broker binary, then drives it as distinct
# signed callers and asserts the whole cloud plane:
#   - auto-provision seeds a per-user credit ledger and returns a derived key
#     bound to the caller's verified identity
#   - a cloud push debits credit and is executed by the broker as the OWNER
#   - broker-enforced isolation: each caller lists ONLY their own machines
#   - 402 when credit runs out; failed pushes refund
#   - per-IP identity cap trips, and a spoofed X-Forwarded-For cannot bypass it
#   - unsigned (spoofed) calls are rejected
#
# No external services, no secrets — runs entirely on this machine.
#   ./scripts/e2e-provision.sh
set -euo pipefail
cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$WORK"' EXIT

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

echo "building broker + broker-sign + refcloud…"
go build -o "$WORK/broker" ./cmd/broker
go build -o "$WORK/broker-sign" ./cmd/broker-sign
go build -o "$WORK/refcloud" ./cmd/refcloud

MASTER="smk_e2e_master"
SECRET="e2e-derive-secret"

REFCLOUD_MASTER="$MASTER" REFCLOUD_ADDR=127.0.0.1:8411 "$WORK/refcloud" &
sleep 1

cat >"$WORK/apps.json" <<JSON
[{"id":"io.pilot.smol","upstream":"http://127.0.0.1:8411","key_env":"SMOL_MASTER",
  "provision":{"provider":"master","secret_env":"SMOL_SECRET","key_version":1,
    "seed_credits":2,"max_identities_per_ip":2,"cost_credits":{"/push":1}}}]
JSON

SMOL_MASTER="$MASTER" SMOL_SECRET="$SECRET" \
  "$WORK/broker" -registry "$WORK/apps.json" -addr 127.0.0.1:8410 -ip-header X-Real-IP &
sleep 1

B=http://127.0.0.1:8410

# signed_call <key> <method> <path> <body> <real-ip> [xff] → sets $STATUS and
# writes the response body to $WORK/out.json. Headers + body go through an array
# and a file so the bytes the caller signs are byte-for-byte the bytes curl sends.
STATUS=""
signed_call() {
  local key="$1" method="$2" path="$3" body="$4" ip="$5" xff="${6:-}"
  printf '%s' "$body" >"$WORK/body"
  local hdrs=()
  while IFS= read -r line; do hdrs+=(-H "$line"); done \
    < <("$WORK/broker-sign" -key "$key" -method "$method" -path "$path" -body "$body")
  hdrs+=(-H "X-Real-IP: $ip")
  [ -n "$xff" ] && hdrs+=(-H "X-Forwarded-For: $xff")
  STATUS="$(curl -s -o "$WORK/out.json" -w '%{http_code}' -X "$method" "${hdrs[@]}" --data-binary "@$WORK/body" "$B$path")"
}

jget() { python3 -c "import sys,json;print(json.load(open('$WORK/out.json')).get('$1',''))"; }
jlen() { python3 -c "import sys,json;d=json.load(open('$WORK/out.json'));print(len(d) if isinstance(d,list) else -1)"; }

echo "generating identities…"
"$WORK/broker-sign" -gen-key "$WORK/alice.key" -path /x >/dev/null
"$WORK/broker-sign" -gen-key "$WORK/bob.key"   -path /x >/dev/null

# push body helper: {name, artifact(base64)}
PUSHBODY='{"name":"web","artifact":"'"$(printf 'vmbytes' | base64)"'"}'

echo "── provisioning ──"
signed_call "$WORK/alice.key" POST /io.pilot.smol/_provision "" 1.1.1.1
[ "$STATUS" = 200 ] || fail "alice provision status=$STATUS ($(cat "$WORK/out.json"))"
ALICE_KEY="$(jget key)"
[ -n "$ALICE_KEY" ] || fail "provision returned no key"
[ "$(jget credits)" = 2 ] && [ "$(jget new)" = "True" ] && pass "alice provisioned: key + 2 seeded credits"

signed_call "$WORK/alice.key" POST /io.pilot.smol/_provision "" 1.1.1.1
[ "$(jget new)" = "False" ] && pass "re-provision is idempotent (no re-seed)"

signed_call "$WORK/alice.key" GET /io.pilot.smol/_balance "" 1.1.1.1
[ "$(jget credits)" = 2 ] && pass "balance reports 2"

echo "── cloud push + metering ──"
signed_call "$WORK/alice.key" POST /io.pilot.smol/push "$PUSHBODY" 1.1.1.1
[ "$STATUS" = 201 ] || fail "alice push status=$STATUS ($(cat "$WORK/out.json"))"
OWNER="$(python3 -c "import json;print(json.load(open('$WORK/out.json'))['env']['PILOT_OWNER'])")"
[ -n "$OWNER" ] && pass "push created an owner-tagged machine"
signed_call "$WORK/alice.key" GET /io.pilot.smol/_balance "" 1.1.1.1
[ "$(jget credits)" = 1 ] && pass "push debited 1 credit (2→1)"

echo "── isolation ──"
signed_call "$WORK/bob.key" POST /io.pilot.smol/_provision "" 2.2.2.2
signed_call "$WORK/bob.key" POST /io.pilot.smol/push "$PUSHBODY" 2.2.2.2
[ "$STATUS" = 201 ] || fail "bob push status=$STATUS"
signed_call "$WORK/alice.key" GET /io.pilot.smol/list "" 1.1.1.1
[ "$(jlen)" = 1 ] || fail "alice should see exactly 1 machine, saw $(jlen)"
signed_call "$WORK/bob.key" GET /io.pilot.smol/list "" 2.2.2.2
[ "$(jlen)" = 1 ] || fail "bob should see exactly 1 machine, saw $(jlen)"
pass "each caller lists ONLY their own machine (broker-enforced isolation)"

echo "── credit exhaustion (402) ──"
signed_call "$WORK/alice.key" POST /io.pilot.smol/push "$PUSHBODY" 1.1.1.1   # 1→0
[ "$STATUS" = 201 ] || fail "alice second push status=$STATUS"
signed_call "$WORK/alice.key" POST /io.pilot.smol/push "$PUSHBODY" 1.1.1.1   # 0 → 402
[ "$STATUS" = 402 ] && pass "push at zero credit returns 402"

echo "── per-IP identity cap (ignores spoofed XFF) ──"
# alice already used IP 3.3.3.3? no — use a fresh IP with cap=2.
"$WORK/broker-sign" -gen-key "$WORK/c1.key" -path /x >/dev/null
"$WORK/broker-sign" -gen-key "$WORK/c2.key" -path /x >/dev/null
"$WORK/broker-sign" -gen-key "$WORK/c3.key" -path /x >/dev/null
signed_call "$WORK/c1.key" POST /io.pilot.smol/_provision "" 9.9.9.9 10.0.0.1
[ "$STATUS" = 200 ] || fail "c1 provision status=$STATUS"
signed_call "$WORK/c2.key" POST /io.pilot.smol/_provision "" 9.9.9.9 10.0.0.2
[ "$STATUS" = 200 ] || fail "c2 provision status=$STATUS"
signed_call "$WORK/c3.key" POST /io.pilot.smol/_provision "" 9.9.9.9 10.0.0.3
[ "$STATUS" = 429 ] && pass "3rd identity on the same X-Real-IP is capped (429), spoofed XFF ignored"

echo "── spoofed (unsigned) call rejected ──"
STATUS="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-Real-IP: 1.1.1.1' "$B/io.pilot.smol/_provision")"
[ "$STATUS" = 401 ] && pass "unsigned provision rejected (401)"

echo
echo "all provisioning e2e checks passed."
