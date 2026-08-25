# App-store presentation metadata API

One source of truth for how apps are described, read by both surfaces that
show an app store:

- the public site, `pilotprotocol.network/app-store` and `/apps/<id>`
- the Alpha management console, `/v1/manage/apps`

## Why it exists

Both surfaces render the same app pages — the same marks, taglines, methods,
compatibility and version history — and each used to carry its own copy of the
data. The site generated `src/data/apps.ts` from local JSON; the console
embedded `authorityhttp/assets/appstore/app-metadata.json`, copied across by
hand. They drifted: at the point this API was written, 25 of 27 apps had
different summary copy on the two surfaces, and one app's console blurb had
been taken from a different paragraph than the site's.

This API removes the second copy. Editing an app means editing exactly one
file, and both surfaces read the result.

## What it is not

It is **not** the catalogue. The publisher-signed catalogue decides what
exists and what may be installed, and it is fail-closed. This document only
decides how an app is *described*. A record here can never make something
installable, and neither consumer may treat it as authority:

- The console iterates the signed catalogue and decorates entries it already
  contains. An app described here but absent from the catalogue does not
  appear.
- `grants` is descriptive. The node reads the signed bundle manifest and
  remains the only source for what an install actually asks for.

Both consumers must degrade rather than fail when the API is unreachable — the
console falls back to its embedded snapshot, the site's build to its committed
one.

## Endpoints

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/v1/appstore/metadata` | The whole document: categories, featured order, every app |
| GET | `/v1/appstore/apps` | `{count, apps}`; filter with `?category=`, `?q=`, `?featured=true` |
| GET | `/v1/appstore/apps/{id}` | One app, or 404 `app_not_found` |
| GET | `/v1/appstore/categories` | Shelf headings and the carousel order |
| GET | `/healthz` | `{status, apps, categories, schema_version}` |

Every response is JSON, carries a strong `ETag` and `Cache-Control:
public, max-age=300`, honours `If-None-Match` with a 304, supports `HEAD`, and
gzips when asked. `Access-Control-Allow-Origin: *` — the data is public and no
request carries a credential. Anything other than `GET`/`HEAD`/`OPTIONS` is a
405: the document changes by deploy, never by request.

## The data

Lives in [`appstore-meta/data/`](../appstore-meta/data):

```
appstore-meta/data/
  index.json          categories + featured_order
  apps/<app-id>.json  one file per app, named for its id
```

One file per app is what makes an edit reviewable — a single large document
produced a diff nobody could read, which is how the two copies drifted in the
first place.

The contract is published as JSON Schema in
[`appstore-meta/schema/`](../appstore-meta/schema) and is checked against the
Go types by `TestPublishedSchemaMatchesTheServedType`, so a field cannot be
served without being documented.

### `summary` is derived, never authored

`description` is authored Markdown and is the only long-copy field edited by
hand. `summary` — the flattened one-paragraph copy a store card shows — is
computed from it at load time (Markdown stripped, capped at 420 runes on a word
boundary). A source file containing `summary` is **rejected**: a hand-written
summary is exactly the drift this API removes, because it silently outlives the
description it condenses.

### Adding or changing an app

1. Edit or add `appstore-meta/data/apps/<id>.json`.
2. `go test ./appstore-meta/ ./internal/appstoremeta/` — the shipped data is
   validated by the same rules the server applies at start.
3. Redeploy (see [`deploy/appstore-meta/README.md`](../deploy/appstore-meta/README.md)).

Validation is strict on purpose. This document is the only description of an
app either surface has, so a typo that silently drops a section is worse than a
server that refuses to start: the operator sees it at deploy time instead of
finding a blank app page in production.

## Running it

```bash
go run ./cmd/appstore-meta-api                        # embedded data, :8080
go run ./cmd/appstore-meta-api -data appstore-meta/data  # a working copy
go run ./cmd/appstore-meta-api -check                 # validate and exit
```

`APPSTORE_META_ADDR` and `APPSTORE_META_DATA` are the environment equivalents.
