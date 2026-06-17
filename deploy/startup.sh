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

meta() { curl -s -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" || true; }
PUBLISH_TOKEN="$(meta pilot-publish-token)"
ADMIN_TOKEN="$(meta admin-token)"

id -u pilot >/dev/null 2>&1 || useradd -r -m -d /opt/pilot pilot
install -d -o pilot -g pilot /opt/pilot

sudo -u pilot HOME=/opt/pilot bash -c '
  set -e
  export PATH=$PATH:/usr/local/go/bin
  cd /opt/pilot
  if [ -d app-template/.git ]; then (cd app-template && git pull --ff-only); else git clone --depth 1 https://github.com/pilot-protocol/app-template; fi
  cd app-template && go build -o /opt/pilot/publish-server ./cmd/publish-server
'

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
WorkingDirectory=/opt/pilot
ExecStart=/opt/pilot/publish-server -addr :80 -store /opt/pilot/store -key /opt/pilot/platform.key
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable pilot-publish
# RESTART (not just enable --now): on a reboot/reset systemd auto-starts the
# previously-enabled service with the OLD on-disk binary BEFORE this script
# rebuilds it. enable --now is then a no-op and the stale binary keeps serving.
# An explicit restart loads the freshly-built binary every deploy.
systemctl restart pilot-publish
echo "pilot-publish (re)started on the freshly built binary"
