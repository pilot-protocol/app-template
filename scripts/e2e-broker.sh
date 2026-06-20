#!/usr/bin/env bash
# Real-process end-to-end test for the managed-key broker. Starts a mock partner
# API + the actual broker binary, then drives it as two distinct signed callers
# and asserts: signed call succeeds + meters, per-caller quota isolation, the
# method allow-list, and rejection of an unsigned (spoofed) call.
#
# No external services, no secrets — runs entirely on this machine.
#   ./scripts/e2e-broker.sh
set -euo pipefail
cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$WORK"' EXIT

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

echo "building broker + broker-sign + mock partner…"
go build -o "$WORK/broker" ./cmd/broker
go build -o "$WORK/broker-sign" ./cmd/broker-sign

# Mock partner API: requires the master key, echoes a fixed cost so we can assert
# metering end to end.
cat >"$WORK/partner.go" <<'GO'
package main

import ("net/http";"os")
func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "MASTER-123" { w.WriteHeader(401); return }
		w.Header().Set("Content-Type","application/json")
		w.Write([]byte(`{"ok":true,"cost_cents":3}`))
	})
	http.ListenAndServe(os.Getenv("ADDR"), nil)
}
GO
ADDR=127.0.0.1:8211 go run "$WORK/partner.go" &
sleep 1

cat >"$WORK/apps.json" <<JSON
[{"id":"io.pilot.demo","upstream":"http://127.0.0.1:8211","key_env":"DEMO_MASTER_KEY",
  "auth_header":"x-api-key","allow":["/enrich"],"quota":2}]
JSON

DEMO_MASTER_KEY=MASTER-123 "$WORK/broker" -registry "$WORK/apps.json" -addr 127.0.0.1:8210 &
sleep 1

B=http://127.0.0.1:8210
"$WORK/broker-sign" -gen-key "$WORK/alice.key" -path /x >/dev/null
"$WORK/broker-sign" -gen-key "$WORK/bob.key"   -path /x >/dev/null

# signed_call <key> <method> <path> <body> → echoes the HTTP status. Headers go
# through a bash array and the body through a file, so the bytes the caller signs
# are byte-for-byte the bytes curl sends (no shell-quoting drift).
signed_call() {
	local key=$1 method=$2 path=$3 body=$4
	printf '%s' "$body" >"$WORK/body"
	local hdrs=()
	while IFS= read -r line; do hdrs+=(-H "$line"); done \
		< <("$WORK/broker-sign" -key "$key" -method "$method" -path "$path" -body "$body")
	curl -s -o /dev/null -w '%{http_code}' -X "$method" "${hdrs[@]}" --data-binary "@$WORK/body" "$B$path"
}

P=/io.pilot.demo/enrich

# 1. signed call from alice → 200
[ "$(signed_call "$WORK/alice.key" POST "$P" '{"q":"a1"}')" = "200" ] \
	&& pass "signed call accepted (200)" || fail "signed call rejected"

# 2. allow-list: undeclared method → 403
[ "$(signed_call "$WORK/alice.key" POST /io.pilot.demo/admin '{}')" = "403" ] \
	&& pass "allow-list blocks undeclared method (403)" || fail "allow-list not enforced"

# 3. unsigned (spoofed) call → 401
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B$P" --data '{}')
[ "$code" = "401" ] && pass "unsigned call rejected (401)" || fail "unsigned got $code"

# 4. quota: alice's 2nd call ok, 3rd exceeds quota 2 → 429
signed_call "$WORK/alice.key" POST "$P" '{"q":"a2"}' >/dev/null
[ "$(signed_call "$WORK/alice.key" POST "$P" '{"q":"a3"}')" = "429" ] \
	&& pass "per-caller quota enforced (429 on 3rd)" || fail "quota not enforced"

# 5. isolation: bob still has his own quota → 200
[ "$(signed_call "$WORK/bob.key" POST "$P" '{"q":"b1"}')" = "200" ] \
	&& pass "per-caller isolation (bob unaffected by alice)" || fail "isolation broken"

# 6. usage endpoint reflects metered cents (alice: 2 successful calls x 3c = 6)
usage=$(curl -s "$B/gw/usage")
echo "$usage" | grep -q '"cents":6' \
	&& pass "metering recorded in /gw/usage" || fail "usage missing cents:6 — $usage"

echo
echo "e2e: all checks passed ✓"
