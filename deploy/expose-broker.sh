#!/usr/bin/env bash
# Expose the managed-key broker at broker.pilotprotocol.network — ONCE, via the
# Cloudflare API (no dashboard clicking). Adds an ingress rule to the existing
# cloudflared tunnel (the same one already serving publish-api) and the DNS
# record. Idempotent: safe to re-run; a no-op if already configured.
#
# This is platform-level, not per-app: the broker routes every managed app by
# id, so you run this once (and again only for a new broker host / DR).
#
# Requires a Cloudflare API token with: Zone:DNS:Edit + Account:Cloudflare
# Tunnel:Edit on the pilotprotocol.network zone. Run it from anywhere:
#   CLOUDFLARE_API_TOKEN=... ./deploy/expose-broker.sh
#
# Overridable env: CF_ZONE (default pilotprotocol.network), CF_HOSTNAME
# (default broker.pilotprotocol.network), BROKER_ORIGIN (default
# http://localhost:8099), CF_TUNNEL_ID (default: auto-detect the active tunnel).
set -euo pipefail

: "${CLOUDFLARE_API_TOKEN:?set CLOUDFLARE_API_TOKEN (Zone:DNS:Edit + Account:Cloudflare Tunnel:Edit)}"
ZONE="${CF_ZONE:-pilotprotocol.network}"
HOST="${CF_HOSTNAME:-broker.pilotprotocol.network}"
ORIGIN="${BROKER_ORIGIN:-http://localhost:8099}"
API=https://api.cloudflare.com/client/v4
AUTH=(-H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "Content-Type: application/json")

cf() { curl -fsS "${AUTH[@]}" "$@"; }
ok() { jq -e '.success' >/dev/null; }

echo "→ resolving zone $ZONE"
zresp=$(cf "$API/zones?name=$ZONE")
echo "$zresp" | ok || { echo "zone lookup failed: $zresp"; exit 1; }
ZONE_ID=$(echo "$zresp" | jq -r '.result[0].id')
ACCOUNT_ID=$(echo "$zresp" | jq -r '.result[0].account.id')
[ -n "$ZONE_ID" ] && [ "$ZONE_ID" != null ] || { echo "zone $ZONE not found on this token"; exit 1; }
echo "  zone=$ZONE_ID account=$ACCOUNT_ID"

# Tunnel: explicit, or auto-detect the single active (non-deleted) tunnel.
if [ -n "${CF_TUNNEL_ID:-}" ]; then
  TUNNEL_ID="$CF_TUNNEL_ID"
else
  tresp=$(cf "$API/accounts/$ACCOUNT_ID/cfd_tunnel?is_deleted=false")
  TUNNEL_ID=$(echo "$tresp" | jq -r '[.result[] | select(.status=="healthy" or .status=="degraded")][0].id // .result[0].id')
fi
[ -n "$TUNNEL_ID" ] && [ "$TUNNEL_ID" != null ] || { echo "no tunnel found; set CF_TUNNEL_ID"; exit 1; }
echo "→ tunnel=$TUNNEL_ID"

echo "→ ensuring ingress: $HOST → $ORIGIN"
cfg=$(cf "$API/accounts/$ACCOUNT_ID/cfd_tunnel/$TUNNEL_ID/configurations")
ingress=$(echo "$cfg" | jq '.result.config.ingress // []')
if echo "$ingress" | jq -e --arg h "$HOST" 'any(.[]; .hostname==$h)' >/dev/null; then
  echo "  ingress for $HOST already present — skipping"
else
  # Insert our rule before the catch-all (the trailing rule with no hostname).
  new=$(echo "$cfg" | jq --arg h "$HOST" --arg s "$ORIGIN" '
    .result.config as $c
    | ($c.ingress // []) as $ing
    | ($ing | map(select(.hostname != null))) as $named
    | ($ing | map(select(.hostname == null))) as $catch
    | {config: ($c + {ingress: ($named + [{hostname:$h, service:$s}] + (if ($catch|length)>0 then $catch else [{service:"http_status:404"}] end))})}')
  echo "$new" | cf -X PUT "$API/accounts/$ACCOUNT_ID/cfd_tunnel/$TUNNEL_ID/configurations" --data @- | ok \
    && echo "  ingress updated" || { echo "ingress update failed"; exit 1; }
fi

echo "→ ensuring DNS: $HOST CNAME ${TUNNEL_ID}.cfargotunnel.com (proxied)"
rec=$(cf "$API/zones/$ZONE_ID/dns_records?name=$HOST&type=CNAME")
REC_ID=$(echo "$rec" | jq -r '.result[0].id // empty')
body=$(jq -n --arg n "$HOST" --arg c "${TUNNEL_ID}.cfargotunnel.com" \
  '{type:"CNAME", name:$n, content:$c, proxied:true, ttl:1}')
if [ -n "$REC_ID" ]; then
  echo "$body" | cf -X PUT "$API/zones/$ZONE_ID/dns_records/$REC_ID" --data @- | ok && echo "  DNS updated"
else
  echo "$body" | cf -X POST "$API/zones/$ZONE_ID/dns_records" --data @- | ok && echo "  DNS created"
fi

echo
echo "✓ broker exposed at https://$HOST  (origin $ORIGIN on the tunnel host)"
echo "  verify:  curl -fsS https://$HOST/gw/health"
