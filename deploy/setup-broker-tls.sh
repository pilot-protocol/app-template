#!/usr/bin/env bash
# Expose the broker over HTTPS with nginx + a real (Let's Encrypt) cert — no
# Cloudflare. Run ON the broker VM (idempotent; safe to re-run / call from
# startup.sh). nginx terminates TLS on :443 and reverse-proxies to the broker on
# localhost:8099. The cert is obtained via TLS-ALPN-01 (port 443 only), so it
# does NOT conflict with publish-server on :80.
#
# Prerequisite: BROKER_HOST must resolve to THIS VM and :443 must be open to the
# internet (gcloud firewall). If DNS isn't pointed yet, cert issuance is skipped
# with a clear message and nginx keeps serving whatever it can.
#
#   sudo BROKER_HOST=broker.pilotprotocol.network CERT_EMAIL=apps@pilotprotocol.network ./setup-broker-tls.sh
set -uo pipefail
HOST="${BROKER_HOST:-broker.pilotprotocol.network}"
EMAIL="${CERT_EMAIL:-apps@pilotprotocol.network}"
ORIGIN="${BROKER_ORIGIN:-http://127.0.0.1:8099}"
LIVE="/etc/letsencrypt/live/$HOST"

echo "→ installing nginx + certbot"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y >/dev/null 2>&1
apt-get install -y nginx certbot >/dev/null 2>&1

# Obtain the cert if we don't have one yet, via HTTP-01 (port 80). publish-server
# holds :80, so free it just for the short issuance window, then restore it.
# (nginx on :443 is unaffected.) Needs $HOST to resolve to this VM + :80 open.
if [ ! -d "$LIVE" ]; then
  echo "→ obtaining Let's Encrypt cert for $HOST (HTTP-01)"
  systemctl stop pilot-publish 2>/dev/null || true
  systemctl stop nginx 2>/dev/null || true
  if certbot certonly --standalone --http-01-port 80 \
       --non-interactive --agree-tos -m "$EMAIL" -d "$HOST" 2>/tmp/certbot.err; then
    echo "  cert obtained ✓"
  else
    echo "  ⚠ cert issuance failed — is $HOST pointed at this VM (DNS A record) and :80 open?"
    sed 's/^/    /' /tmp/certbot.err | tail -4
  fi
  systemctl start pilot-publish 2>/dev/null || true
fi

# Write the nginx vhost only once a cert exists (else nginx would fail to start).
if [ -d "$LIVE" ]; then
  echo "→ writing nginx vhost: https://$HOST → $ORIGIN"
  cat >/etc/nginx/sites-available/broker.conf <<NGINX
server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name $HOST;

    ssl_certificate     $LIVE/fullchain.pem;
    ssl_certificate_key $LIVE/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    # Agentic partner calls can run for minutes — match the broker's ceiling.
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
    client_max_body_size 16m;

    location / {
        proxy_pass $ORIGIN;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
NGINX
  ln -sf /etc/nginx/sites-available/broker.conf /etc/nginx/sites-enabled/broker.conf
  # Remove nginx's default site — it binds :80, which publish-server owns, so
  # nginx would fail to start. The broker vhost is :443-only.
  rm -f /etc/nginx/sites-enabled/default
  # Renewal (HTTP-01) needs :80 briefly; free it from publish-server then restore.
  mkdir -p /etc/letsencrypt/renewal-hooks/pre /etc/letsencrypt/renewal-hooks/post
  echo -e '#!/bin/sh\nsystemctl stop pilot-publish' >/etc/letsencrypt/renewal-hooks/pre/stop-publish.sh
  echo -e '#!/bin/sh\nsystemctl start pilot-publish' >/etc/letsencrypt/renewal-hooks/post/start-publish.sh
  chmod +x /etc/letsencrypt/renewal-hooks/pre/stop-publish.sh /etc/letsencrypt/renewal-hooks/post/start-publish.sh
  nginx -t && systemctl enable nginx >/dev/null 2>&1 && systemctl restart nginx
  echo "✓ broker live at https://$HOST  (verify: curl -fsS https://$HOST/gw/health)"
else
  systemctl start nginx 2>/dev/null || true
  echo "nginx installed; re-run this script once $HOST resolves to this VM to finish TLS."
  exit 1
fi
