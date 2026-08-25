#!/usr/bin/env bash
# GCE startup script for the app-store presentation metadata API.
#
# Builds cmd/appstore-meta-api from pilot-protocol/app-template and runs it
# under systemd behind nginx, with a Let's Encrypt certificate when a hostname
# is set. The API is read-only and holds no secret: everything it serves is
# already public on the website, so the only thing worth protecting here is
# availability.
#
# The script is idempotent and re-runs on every boot, so `gcloud compute
# instances reset` is a full redeploy.
#
# Instance metadata:
#   appstore-meta-host   DNS name to obtain a certificate for (empty -> HTTP only)
#   appstore-meta-email  contact address for the ACME account (expiry notices)
#   appstore-meta-ref    git ref to build (empty -> main)
set -euo pipefail

GO_VERSION=1.25.0
ARCH=linux-amd64
REPO=https://github.com/pilot-protocol/app-template

meta() { curl -sf -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" || true; }
HOST="$(meta appstore-meta-host)"
ACME_EMAIL="$(meta appstore-meta-email)"
REF="$(meta appstore-meta-ref)"; REF="${REF:-main}"

apt-get update -y
apt-get install -y git curl ca-certificates nginx

if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${ARCH}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
fi
export PATH=$PATH:/usr/local/go/bin

id -u pilot >/dev/null 2>&1 || useradd -r -m -d /opt/pilot pilot
install -d -o pilot -g pilot /opt/pilot

sudo -u pilot HOME=/opt/pilot REF="$REF" bash -c '
  set -e
  export PATH=$PATH:/usr/local/go/bin
  cd /opt/pilot
  if [ -d app-template/.git ]; then
    cd app-template && git fetch --depth 1 origin "$REF" && git checkout -f FETCH_HEAD
  else
    git clone --depth 1 --branch "$REF" '"$REPO"' app-template && cd app-template
  fi
  # The data is embedded, so this binary is the whole deployment.
  go build -o /opt/pilot/appstore-meta-api ./cmd/appstore-meta-api
  # Refuse to install a build whose data does not validate.
  /opt/pilot/appstore-meta-api -check
'

cat >/etc/systemd/system/appstore-meta-api.service <<UNIT
[Unit]
Description=Pilot app-store presentation metadata API
After=network-online.target
Wants=network-online.target

[Service]
User=pilot
ExecStart=/opt/pilot/appstore-meta-api -addr 127.0.0.1:8080
Restart=always
RestartSec=2
# Read-only service with no state: nothing here needs to write anywhere.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
RestrictAddressFamilies=AF_INET AF_INET6
MemoryMax=256M

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now appstore-meta-api
systemctl restart appstore-meta-api

SERVER_NAME="${HOST:-_}"
cat >/etc/nginx/sites-available/appstore-meta <<NGINX
server {
    listen 80;
    listen [::]:80;
    server_name ${SERVER_NAME};

    # The payload is JSON and compresses about 3.5x; the site's build and the
    # console both ask for gzip.
    gzip on;
    gzip_types application/json;
    gzip_proxied any;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        # Let the origin's ETag through untouched so consumers can revalidate.
        proxy_set_header If-None-Match \$http_if_none_match;
    }
}
NGINX
ln -sf /etc/nginx/sites-available/appstore-meta /etc/nginx/sites-enabled/appstore-meta
rm -f /etc/nginx/sites-enabled/default
nginx -t && systemctl reload nginx

# TLS last: certbot needs the HTTP vhost above to answer the ACME challenge.
if [ -n "$HOST" ]; then
  apt-get install -y certbot python3-certbot-nginx
  EMAIL_ARG="--register-unsafely-without-email"
  [ -n "$ACME_EMAIL" ] && EMAIL_ARG="--email $ACME_EMAIL"
  # Non-fatal: a DNS record that has not propagated yet must not leave the box
  # without a running API. Re-run `certbot --nginx -d $HOST` once it resolves.
  certbot --nginx -d "$HOST" --agree-tos --non-interactive --redirect $EMAIL_ARG \
    || echo "certbot failed for $HOST; serving HTTP only until DNS resolves"
fi

echo "appstore-meta-api ready: $(curl -sf http://127.0.0.1:8080/healthz || echo 'health check failed')"
