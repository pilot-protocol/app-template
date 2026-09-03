# Deploy the app-store metadata API (GCE VM)

`cmd/appstore-meta-api` serves the presentation metadata that the public
website and the Alpha management console both render. Before it existed each
surface carried its own hand-maintained copy, and they drifted.

The service is read-only, stateless, and holds no secret — everything it
serves is already public. The data is embedded in the binary, so a deploy is a
rebuild.

## One-time provision (Pilot Staging)

```bash
PROJECT=pilot-protocol-stg-vl
ZONE=europe-west3-c
HOST=appstore-meta.pilotprotocol.network

gcloud compute instances create appstore-meta \
  --project="$PROJECT" --zone="$ZONE" \
  --machine-type=e2-micro \
  --image-family=debian-12 --image-project=debian-cloud \
  --boot-disk-size=20GB \
  --tags=appstore-meta-web \
  --metadata=appstore-meta-host="$HOST",appstore-meta-email=apps@pilotprotocol.network,appstore-meta-ref=main \
  --metadata-from-file=startup-script=deploy/appstore-meta/startup.sh

gcloud compute firewall-rules create allow-appstore-meta-web \
  --project="$PROJECT" --allow=tcp:80,tcp:443 --target-tags=appstore-meta-web \
  --description="Pilot app-store presentation metadata API"

gcloud compute instances describe appstore-meta --zone="$ZONE" --project="$PROJECT" \
  --format='get(networkInterfaces[0].accessConfigs[0].natIP)'
```

Point `$HOST` at that IP with an A record **before** the certificate can
issue. The startup script provisions the certificate on every boot and treats
a failure as non-fatal, so if DNS is not ready yet the box still serves the API
over HTTP; `gcloud compute instances reset` once the record resolves, or SSH in
and run `certbot --nginx -d $HOST`.

## Verify

```bash
curl -s https://$HOST/healthz
curl -s https://$HOST/v1/appstore/apps?featured=true | jq '.apps[].id'
curl -sI https://$HOST/v1/appstore/metadata | grep -i etag
```

## Update

Repository deployments use a dedicated `APPSTORE_META_GCP_SA_KEY` secret.
The service account must be able to read and SSH to the `appstore-meta`
instance in this project, including IAP tunnel access. The general publishing
VM credential is intentionally not reused across this project boundary.

`startup.sh` re-clones and rebuilds on every boot:

```bash
gcloud compute instances reset appstore-meta --zone="$ZONE" --project="$PROJECT"
```

or, on the box, `cd /opt/pilot/app-template && git pull && go build -o
/opt/pilot/appstore-meta-api ./cmd/appstore-meta-api && sudo systemctl restart
appstore-meta-api`.

`appstore-meta-api -check` runs at build time in the startup script, so a data
file that fails validation aborts the deploy instead of replacing a good
document with a broken one.

## Sizing

The whole document is ~330 KB of JSON, ~95 KB gzipped, rendered once at start
and served from memory. `e2-micro` is the right size; the systemd unit caps the
process at 256 MB.
