# Docker: prod-like broker + publish-server

Run the two services that make up app publishing locally, wired the same way
they are in production. Use this to test the full managed-key flow (submit →
approve → broker routes + meters) before touching a VM.

## What's here

| File | Purpose |
|---|---|
| `broker.Dockerfile` | The managed-key gateway — static binary on distroless. |
| `publish-server.Dockerfile` | The submission API + admin — keeps Go + git (it compiles adapters at runtime). |
| `docker-compose.yml` | Both services + shared volumes, prod-like. |
| `apps.example.json` | A sample broker registry (the publish-server writes the real one on approval). |

## Quick start

```bash
cd deploy/docker
SIXTYFOUR_MASTER_KEY=sk-real-key ADMIN_TOKEN=dev-admin docker compose up --build
```

- Broker:          http://localhost:8099/gw/health
- Publish admin:   http://localhost:8080/admin?token=dev-admin

The broker boots with an **empty** registry and serves 404s until a managed app
is approved. On approval the publish-server writes `/registry/apps.json` (the
shared `registry` volume). Load it into the running broker with no downtime:

```bash
docker compose kill -s HUP broker
```

Or seed the registry directly for a manual test:

```bash
docker compose cp apps.example.json broker:/registry/apps.json
docker compose kill -s HUP broker
```

## Master keys

Each managed app reads its master key from `<NAMESPACE>_MASTER_KEY` (the
publish-server prints the exact name on approval). Set it in the broker's
environment — never in the registry file or the image.

## Usage / metering

```bash
curl localhost:8099/gw/usage    # per-(app, caller) calls + cents
```

The broker's usage store is durable (`BROKER_DB=/data/usage.db` on the
`brokerdata` volume), so metering survives restarts.
