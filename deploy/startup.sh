#!/usr/bin/env bash
# GCE startup script for the Pilot app-store submission server.
# Installs Go + git (needed at RUNTIME — the server shells out to `go build` to
# compile each adapter and to `git` to trigger the publish workflow), builds
# publish-server from pilot-protocol/app-template, and runs it under systemd.
#
# Secrets come from instance metadata:
#   pilot-publish-token  GitHub token with push to pilot-protocol/app-template
#   admin-token          gates /admin + approve/reject
set -euo pipefail

GO_VERSION=1.25.0
ARCH=linux-amd64

apt-get update -y
apt-get install -y git curl ca-certificates

# Install Go only if the right version isn't already on disk (fast reboots).
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${ARCH}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
fi
export PATH=$PATH:/usr/local/go/bin

# -f so a MISSING attribute returns empty (not a 404 HTML body that would
# otherwise pollute env files like broker.env).
meta() { curl -sf -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" || true; }
PUBLISH_TOKEN="$(meta pilot-publish-token)"
ADMIN_TOKEN="$(meta admin-token)"
ALLOWED_ORIGINS="$(meta allowed-origins)"       # empty -> server defaults to the prod website origins
SENDGRID_API_KEY="$(meta sendgrid-api-key)"     # empty -> emails fall back to dev log (no real send)
MAIL_FROM="$(meta mail-from)"                    # empty -> apps@pilotprotocol.network
MAIL_REGION="$(meta mail-region)"                # "eu" for EU data residency
# Key rotation (POST /admin/rotate-key): a token with write to the platform repo
# + the hex catalogue signing key. Empty -> the admin rotation endpoint returns 503.
CATALOG_PUBLISH_TOKEN="$(meta catalog-publish-token)"
CATALOG_SIGN_KEY="$(meta catalog-sign-key)"
# Artifact registry (POST /api/artifact/presign): R2 S3 creds + public base.
# Empty -> the presign endpoint returns 503 (uploads disabled).
R2_ACCOUNT_ID="$(meta r2-account-id)"
R2_BUCKET="$(meta r2-bucket)"
R2_ACCESS_KEY_ID="$(meta r2-access-key-id)"
R2_SECRET_ACCESS_KEY="$(meta r2-secret-access-key)"
R2_PUBLIC_BASE="$(meta r2-public-base)"

id -u pilot >/dev/null 2>&1 || useradd -r -m -d /opt/pilot pilot
install -d -o pilot -g pilot /opt/pilot

sudo -u pilot HOME=/opt/pilot bash -c '
  set -e
  export PATH=$PATH:/usr/local/go/bin
  cd /opt/pilot
  if [ -d app-template/.git ]; then (cd app-template && git pull --ff-only); else git clone --depth 1 https://github.com/pilot-protocol/app-template; fi
  cd app-template
  go build -o /opt/pilot/publish-server ./cmd/publish-server
  go build -o /opt/pilot/broker         ./cmd/broker
'
install -d -o pilot -g pilot /opt/pilot/registry   # shared: publish-server writes apps.json, broker reads it

# Broker master keys (one per managed app, e.g. PARTNER_MASTER_KEY=sk-...),
# newline-separated KEY=VALUE, from instance metadata -> systemd EnvironmentFile.
BROKER_ENV="$(meta broker-env)"
printf '%s\n' "$BROKER_ENV" >/opt/pilot/broker.env
chown pilot:pilot /opt/pilot/broker.env && chmod 600 /opt/pilot/broker.env

cat >/etc/systemd/system/pilot-publish.service <<UNIT
[Unit]
Description=Pilot app-store submission server
After=network-online.target
Wants=network-online.target

[Service]
User=pilot
# Bind :80 as a non-root user (the privileged-port capability, nothing more).
AmbientCapabilities=CAP_NET_BIND_SERVICE
Environment=PATH=/usr/local/go/bin:/usr/bin:/bin
Environment=HOME=/opt/pilot
Environment=PILOT_PUBLISH_TOKEN=${PUBLISH_TOKEN}
Environment=ADMIN_TOKEN=${ADMIN_TOKEN}
Environment=ALLOWED_ORIGINS=${ALLOWED_ORIGINS}
Environment=SENDGRID_API_KEY=${SENDGRID_API_KEY}
Environment=MAIL_FROM=${MAIL_FROM}
Environment=MAIL_REGION=${MAIL_REGION}
# Managed-app approval writes this registry; the broker (below) reads it.
Environment=BROKER_REGISTRY=/opt/pilot/registry/apps.json
# Key rotation (admin) — write to the platform repo + re-sign the catalogue.
Environment=CATALOG_PUBLISH_TOKEN=${CATALOG_PUBLISH_TOKEN}
Environment=CATALOG_SIGN_KEY=${CATALOG_SIGN_KEY}
# Artifact registry presign uploads (R2 S3-compatible).
Environment=R2_ACCOUNT_ID=${R2_ACCOUNT_ID}
Environment=R2_BUCKET=${R2_BUCKET}
Environment=R2_ACCESS_KEY_ID=${R2_ACCESS_KEY_ID}
Environment=R2_SECRET_ACCESS_KEY=${R2_SECRET_ACCESS_KEY}
Environment=R2_PUBLIC_BASE=${R2_PUBLIC_BASE}
WorkingDirectory=/opt/pilot
ExecStart=/opt/pilot/publish-server -addr :80 -store /opt/pilot/store -key /opt/pilot/platform.key
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

# The managed-key broker: holds the partner master keys (from broker.env), reads
# the registry the publish-server writes, meters per caller to a durable store.
cat >/etc/systemd/system/pilot-broker.service <<UNIT
[Unit]
Description=Pilot managed-key broker
After=network-online.target
Wants=network-online.target

[Service]
User=pilot
EnvironmentFile=/opt/pilot/broker.env
# NOTE: the broker binary is /opt/pilot/broker (a file), so the durable store
# lives in a SEPARATE dir to avoid a path collision.
Environment=BROKER_DB=/opt/pilot/broker-data/usage.db
WorkingDirectory=/opt/pilot
ExecStartPre=/usr/bin/install -d -o pilot -g pilot /opt/pilot/broker-data
ExecStart=/opt/pilot/broker -registry /opt/pilot/registry/apps.json -addr :8099
# Reload the registry on approval without dropping traffic.
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable pilot-publish pilot-broker
# RESTART (not just enable --now): on a reboot/reset systemd auto-starts the
# previously-enabled service with the OLD on-disk binary BEFORE this script
# rebuilds it. enable --now is then a no-op and the stale binary keeps serving.
# An explicit restart loads the freshly-built binary every deploy.
systemctl restart pilot-publish pilot-broker
echo "pilot-publish + pilot-broker (re)started on freshly built binaries"

# Expose the broker over HTTPS via nginx + a Let's Encrypt cert (idempotent;
# no-op once the cert exists). Best-effort: if the broker hostname doesn't
# resolve to this VM yet, it logs and leaves publish/broker untouched.
BROKER_HOST="$(meta broker-host)"; BROKER_HOST="${BROKER_HOST:-broker.pilotprotocol.network}"
CERT_EMAIL="$(meta mail-from)"; CERT_EMAIL="${CERT_EMAIL:-apps@pilotprotocol.network}"
BROKER_HOST="$BROKER_HOST" CERT_EMAIL="$CERT_EMAIL" \
  bash /opt/pilot/app-template/deploy/setup-broker-tls.sh || true
