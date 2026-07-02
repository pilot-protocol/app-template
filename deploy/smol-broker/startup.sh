#!/usr/bin/env bash
# GCE startup script for the DEDICATED smol provisioning broker (io.pilot.smol).
# Separate from the shared managed-key broker on pilot-publish: this VM runs ONLY
# the broker, fronting the smol cloud with a per-user provisioning data plane.
#
# It builds the broker from app-template, runs it under systemd bound to
# localhost:8099, and puts nginx in front for TLS at smol-broker.pilotprotocol.network
# with the real client IP passed as X-Real-IP (the broker's anti-spoof source).
#
# Secrets come from instance metadata attribute `broker-env` (newline KEY=VALUE):
#   SMOL_MASTER_KEY     the smk_… cloud master key (broker-only; never shipped)
#   SMOL_DERIVE_SECRET  the HMAC secret used to mint per-user keys
set -euo pipefail

GO_VERSION=1.25.0
BRANCH="${BROKER_BRANCH:-feat/smol-provisioning-broker}"
HOSTNAME_FQDN="smol-broker.pilotprotocol.network"

apt-get update -y
apt-get install -y git curl ca-certificates nginx

if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
fi
export PATH=$PATH:/usr/local/go/bin

meta() { curl -sf -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" || true; }

id -u pilot >/dev/null 2>&1 || useradd -r -m -d /opt/pilot pilot
install -d -o pilot -g pilot /opt/pilot /opt/pilot/registry

# Build the broker from the branch (unmerged until the app-template PR lands).
sudo -u pilot HOME=/opt/pilot bash -c "
  set -e
  export PATH=\$PATH:/usr/local/go/bin
  cd /opt/pilot
  if [ -d app-template/.git ]; then (cd app-template && git fetch origin '${BRANCH}' && git checkout '${BRANCH}' && git pull --ff-only origin '${BRANCH}'); else git clone --branch '${BRANCH}' --depth 1 https://github.com/pilot-protocol/app-template; fi
  cd app-template
  go build -o /opt/pilot/broker ./cmd/broker
  cp deploy/smol-broker/apps.json /opt/pilot/registry/apps.json
"

# Secrets → EnvironmentFile (0600, pilot-only).
printf '%s\n' "$(meta broker-env)" >/opt/pilot/broker.env
chown pilot:pilot /opt/pilot/broker.env && chmod 600 /opt/pilot/broker.env

cat >/etc/systemd/system/pilot-smol-broker.service <<UNIT
[Unit]
Description=Pilot smol provisioning broker (io.pilot.smol)
After=network-online.target
Wants=network-online.target

[Service]
User=pilot
EnvironmentFile=/opt/pilot/broker.env
Environment=BROKER_DB=/opt/pilot/registry/usage.db
ExecStart=/opt/pilot/broker -registry /opt/pilot/registry/apps.json -addr 127.0.0.1:8099 -ip-header X-Real-IP
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now pilot-smol-broker

# nginx: terminate TLS and pass the REAL client IP as X-Real-IP (the broker's
# only trusted source; a client-supplied value is overwritten here).
cat >/etc/nginx/sites-available/smol-broker <<NGINX
server {
    listen 80;
    server_name ${HOSTNAME_FQDN};
    location / {
        proxy_pass http://127.0.0.1:8099;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        client_max_body_size 300m;
    }
}
NGINX
ln -sf /etc/nginx/sites-available/smol-broker /etc/nginx/sites-enabled/smol-broker
rm -f /etc/nginx/sites-enabled/default
nginx -t && systemctl restart nginx

echo "smol-broker: broker + nginx up on :80. Run certbot after DNS resolves:"
echo "  certbot --nginx -d ${HOSTNAME_FQDN} --non-interactive --agree-tos -m apps@pilotprotocol.network --redirect"
