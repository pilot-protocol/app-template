#!/usr/bin/env bash
#
# demo-telemetry.sh — the ACTUAL-uplift query for product demos.
#
# ─── What we are measuring ────────────────────────────────────────────────────
# Product demos exist to fix one number: the network's ~250k autonomous agents
# INSTALL apps but rarely USE them, because the app was never surfaced well to a
# small-context agent. `pilot-app demo-score` measures the POTENTIAL uplift (is a
# demo good enough to drive a correct first call?). THIS script measures the
# ACTUAL uplift — did behaviour change once demos shipped?
#
# The metric is INSTALL→FIRST-CALL CONVERSION, per app, sliced by the demo
# rollout date:
#
#     conversion(app) = users_who_made_>=1_non-read_call_within_7d(app)
#                       ----------------------------------------------
#                                  users_who_installed(app)
#
#   • numerator   — distinct caller identities that issued at least ONE
#                   non-read (state-changing / $-spending) method call to the
#                   app within 7 days of installing it. Reads (usage/help/get_*/
#                   list_*) are EXCLUDED: an install that only ever calls
#                   <ns>.help is not "usage", it's a bounce.
#   • denominator — distinct caller identities that installed the app.
#   • sliced BEFORE vs AFTER the demo rollout date (ROLLOUT_DATE below): the
#     uplift is  conversion_after − conversion_before.  A demo "worked" if
#     conversion rose after it shipped.
#
# Read it as: "of everyone who installed <app>, what fraction actually used it —
# and did that fraction go up after we shipped the demo?"
#
# ─── Where the signal lives (you may NOT have access — degrades gracefully) ───
#   • pilot-publish VM      — shared broker on :8099, registry at
#                             /opt/pilot/registry/apps.json; per-call meter events
#                             in the broker journal (journalctl -u pilot-broker).
#   • smol-broker VM        — the smol provisioning broker (compute-metered).
#   • pilot-log-aggregator  — the aggregated telemetry sink, if reachable.
#   • telemetry repo        — schema + canonical queries for the above.
#
# This host is NOT guaranteed to have gcloud creds or network reach to those
# VMs. Every step below is attempted, and on failure prints exactly what to run
# where, then continues — the script always exits 0 so it is safe to invoke in
# CI or a dry environment. NOTHING here is required by the Go tests.
#
# ─── Usage ────────────────────────────────────────────────────────────────────
#   ./scripts/demo-telemetry.sh                # all apps, default rollout date
#   ./scripts/demo-telemetry.sh io.pilot.smol  # one app
#   ROLLOUT_DATE=2026-07-13 ./scripts/demo-telemetry.sh
#   BROKER_VM=pilot-publish GCLOUD_ZONE=us-central1-a ./scripts/demo-telemetry.sh
#
set -uo pipefail   # NOT -e: a missing telemetry backend must not abort the run.

APP="${1:-all}"
ROLLOUT_DATE="${ROLLOUT_DATE:-2026-07-13}"     # date product demos went live
WINDOW_DAYS="${WINDOW_DAYS:-7}"                # install→first-call window
BROKER_VM="${BROKER_VM:-pilot-publish}"
SMOL_VM="${SMOL_VM:-smol-broker}"
GCLOUD_ZONE="${GCLOUD_ZONE:-us-central1-a}"
REGISTRY_PATH="${REGISTRY_PATH:-/opt/pilot/registry/apps.json}"
BROKER_UNIT="${BROKER_UNIT:-pilot-broker}"
AGG_URL="${AGG_URL:-}"                         # optional pilot-log-aggregator base URL

bold() { printf '\033[1m%s\033[0m\n' "$1"; }
info() { printf '  %s\n' "$1"; }
warn() { printf '  \033[33m! %s\033[0m\n' "$1"; }
ok()   { printf '  \033[32m✓ %s\033[0m\n' "$1"; }

bold "product-demo ACTUAL-uplift — install→first-call conversion"
info "app=${APP}  rollout=${ROLLOUT_DATE}  window=${WINDOW_DAYS}d  broker=${BROKER_VM}"
echo

# The journald query we WANT to run on the broker. It reduces the per-call meter
# log to, per app, the set of distinct callers that installed and the subset that
# then made a non-read call — split before/after the rollout date. The broker log
# line shape is assumed to be JSON with {ts, app, caller, method, kind} where
# kind ∈ {install, call} and read methods match /(help|usage|get_|list_|version)/.
read -r -d '' REMOTE_QUERY <<'AWK'
journalctl -u __UNIT__ --output=cat --since "__SINCE__" 2>/dev/null \
  | awk -v rollout="__ROLLOUT__" '
      # Expects one JSON meter event per line. Falls back silently on non-JSON.
      function field(k,   s,v){ s=$0; if (match(s, "\""k"\":\"[^\"]*\"")) {
          v=substr(s,RSTART,RLENGTH); sub("\""k"\":\"","",v); sub("\"$","",v); return v } return "" }
      {
        app=field("app"); caller=field("caller"); method=field("method");
        kind=field("kind"); ts=field("ts");
        if (app=="" || caller=="") next;
        era=(ts < rollout ? "before" : "after");
        key=app SUBSEP era;
        isread = (method ~ /(help|usage|\.get_|\.list_|version|balance)/);
        if (kind=="install") { inst[key SUBSEP caller]=1; apps[app]=1 }
        else if (kind=="call" && !isread) { used[key SUBSEP caller]=1 }
      }
      END{
        for (a in apps) for (e in eras=split("before after",E," ")?E:E) {}
        split("before after", ERA, " ");
        printf("%-28s %-7s %8s %8s %10s\n","APP","ERA","INSTALL","USED","CONV");
        for (a in apps) for (i=1;i<=2;i++){ e=ERA[i]; ni=0; nu=0;
          for (k in inst) if (index(k,a SUBSEP e SUBSEP)==1) ni++;
          for (k in used) if (index(k,a SUBSEP e SUBSEP)==1) nu++;
          conv=(ni>0? sprintf("%.1f%%",100*nu/ni):"n/a");
          printf("%-28s %-7s %8d %8d %10s\n",a,e,ni,nu,conv);
        }
      }'
AWK
SINCE="$(date -u -v-90d +%Y-%m-%d 2>/dev/null || date -u -d '90 days ago' +%Y-%m-%d 2>/dev/null || echo "$ROLLOUT_DATE")"
REMOTE_QUERY="${REMOTE_QUERY//__UNIT__/$BROKER_UNIT}"
REMOTE_QUERY="${REMOTE_QUERY//__SINCE__/$SINCE}"
REMOTE_QUERY="${REMOTE_QUERY//__ROLLOUT__/$ROLLOUT_DATE}"

tried_any=0

# ── Path 1: aggregated telemetry endpoint (cheapest, if configured) ───────────
if [ -n "$AGG_URL" ]; then
  tried_any=1
  bold "→ pilot-log-aggregator ($AGG_URL)"
  if command -v curl >/dev/null 2>&1; then
    q="${AGG_URL%/}/v1/conversion?app=${APP}&rollout=${ROLLOUT_DATE}&window=${WINDOW_DAYS}"
    if body="$(curl -fsS --max-time 15 "$q" 2>/dev/null)"; then
      ok "aggregator responded:"; echo "$body"; echo
    else
      warn "aggregator unreachable — falling through to the broker journal"
    fi
  else
    warn "curl not installed; skipping aggregator path"
  fi
fi

# ── Path 2: broker journald on the VM (authoritative meter events) ────────────
bold "→ broker journal on ${BROKER_VM} (unit ${BROKER_UNIT})"
if command -v gcloud >/dev/null 2>&1; then
  tried_any=1
  if out="$(gcloud compute ssh "$BROKER_VM" --zone "$GCLOUD_ZONE" --tunnel-through-iap \
              --command "$REMOTE_QUERY" 2>/dev/null)" && [ -n "$out" ]; then
    ok "broker conversion table:"; echo "$out"; echo
  else
    warn "could not reach ${BROKER_VM} (no creds / IAP / unit not present)."
    info "Run it yourself on the box:"
    info "  gcloud compute ssh ${BROKER_VM} --zone ${GCLOUD_ZONE} --tunnel-through-iap --command '<the awk query above>'"
    info "  # or on the VM directly: journalctl -u ${BROKER_UNIT} --since ${SINCE} | <awk query>"
  fi
  # The install denominator can also be recovered from the registry install log.
  info "install counts also live in the registry: ${REGISTRY_PATH} (per-app installs[])"
else
  warn "gcloud not installed — cannot reach the broker VM from here."
  info "On a host WITH gcloud + access, run:"
  info "  gcloud compute ssh ${BROKER_VM} --zone ${GCLOUD_ZONE} --tunnel-through-iap \\"
  info "    --command 'journalctl -u ${BROKER_UNIT} --since ${SINCE}'  | <the awk reducer in this script>"
fi
echo

# ── Path 3: smol broker (compute-metered app) ────────────────────────────────
bold "→ smol broker on ${SMOL_VM}"
if command -v gcloud >/dev/null 2>&1; then
  info "smol meters by compute, not per-call; its 'use' signal is any /push after install."
  info "  gcloud compute ssh ${SMOL_VM} --zone ${GCLOUD_ZONE} --tunnel-through-iap \\"
  info "    --command 'journalctl -u smol-broker --since ${SINCE}'"
else
  warn "gcloud not installed — see the command above to run on an authorized host."
fi
echo

if [ "$tried_any" -eq 0 ]; then
  warn "No telemetry backend was reachable from this host."
fi
bold "How to read the result"
info "conversion = distinct callers with >=1 non-read call within ${WINDOW_DAYS}d of install,"
info "over distinct installers. Compare the 'after' row to 'before': a demo worked if"
info "conversion rose after ${ROLLOUT_DATE}. Cross-check the POTENTIAL score with:"
info "  pilot-app demo-score submissions        # per-app quality + first-call proxy"
echo
info "(this script never fails the build; it documents + attempts the real query.)"
exit 0
