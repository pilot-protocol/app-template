# Deploy the submission server (GCloud VM)

The submission server (`cmd/publish-server`) is a small web app: a developer
submits an app via a form, the **server** builds + signs + verifies the adapter
bundle (no browser-side computation), stores it pending, and on admin approval
pushes the submission to `pilot-protocol/app-template`, which fires the
`publish-on-merge` workflow → release on `pilot-protocol/catalog` + the catalogue
v2 entry on the platform repo.

## One-time provision

Requires `gcloud auth login` first (project `vulture-vision-cloud`).

```bash
PROJECT=vulture-vision-cloud
ZONE=us-central1-a

# Tokens the server needs:
#   PUBLISH_TOKEN — GitHub token with push to pilot-protocol/app-template (admin bypass for protected main)
#   ADMIN_TOKEN   — any strong secret; gates /admin + approve/reject
gcloud compute instances create pilot-publish \
  --project="$PROJECT" --zone="$ZONE" \
  --machine-type=e2-small \
  --image-family=debian-12 --image-project=debian-cloud \
  --tags=pilot-publish-http \
  --metadata=admin-token="$ADMIN_TOKEN",pilot-publish-token="$PUBLISH_TOKEN" \
  --metadata-from-file=startup-script=deploy/startup.sh

# Open HTTP (port 80) to the instance:
gcloud compute firewall-rules create allow-pilot-publish \
  --project="$PROJECT" --allow=tcp:80 --target-tags=pilot-publish-http \
  --description="Pilot app-store submission server"

# Public IP:
gcloud compute instances describe pilot-publish --zone="$ZONE" \
  --format='get(networkInterfaces[0].accessConfigs[0].natIP)'
```

Then:
- Submit form: `http://<IP>/`
- Review:      `http://<IP>/admin?token=<ADMIN_TOKEN>`

The platform signing key is generated on first boot at `/opt/pilot/platform.key`
(persisted on the instance disk). Add its public side to
`pilot-protocol/catalog` `publishers/registry.json`:
`journalctl -u pilot-publish | grep publisher` prints it.

## Updating the server

`startup.sh` pulls + rebuilds on each boot, so:
```bash
gcloud compute instances reset pilot-publish --zone="$ZONE"
```
or SSH in and `systemctl restart pilot-publish` after a manual `git pull && go build`.

## Security notes (v1, "keep it simple")

- The VM holds a GitHub push token and the platform signing key. Lock SSH down,
  and prefer a fine-grained PAT / GitHub App token scoped to just the two repos.
- HTTP only here. Front with a TLS load balancer or Caddy for production.
- `ADMIN_TOKEN` is a shared secret in a query param — fine for v1, replace with
  real auth before exposing widely.
