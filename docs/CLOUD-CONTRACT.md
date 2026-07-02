# smol cloud contract (verified live against `api.smolmachines.com`)

This is the ground-truth the provisioning broker's `CloudProvider` is written against. It was
established by probing the live API with the smol master key (`smk_…`). The broker is the **only**
holder of that key and the **only** entrypoint to the cloud; users never receive it.

## Auth & scope of the master key

- Auth: `Authorization: Bearer smk_…`.
- `GET /v1/me` → the key's scopes are **`machine:create,read,exec,delete,files` only** — **no `admin`,
  no `usage:read`**. One tenant (`tenant-…`), one `registryNamespace: tenants/<tenant>`.
- `GET /v1/account` → plan limits (`maxMachines`, `maxConcurrentMachines`), `freeCreditMicros`, per-
  resource rate `…Micros`, and `periodUsage`/`periodCost` (tenant-level only).
- `POST /v1/tokens` → **403 `missing scope: admin`** — the master key **cannot mint per-user cloud
  tokens**. (This is why v1 uses broker-derived HMAC keys + broker-enforced isolation. The dormant
  `tokenMintProvider` targets this endpoint once an admin key exists.)
- `GET /v1/usage` → **403 `missing scope: admin or usage:read`** — no per-user cloud metering; the
  broker meters with its own credit ledger.

## Machines (`/v1/machines`)

- `POST /v1/machines` (201) body: `{"source":<Source>, "name"?:string, "env"?:{..}, ...}`.
  `Source` is type-tagged: `{"type":"image","reference":"alpine:3.20"}` (OCI image) **or**
  `{"type":"smolmachine","reference":"tenants/<tenant>/<group>:<tag>","arch":"amd64"}` (a published
  smolmachine artifact). Defaults when omitted: `cpus:1, memoryMb:256, network:blocked, state:stopped`.
- `GET /v1/machines` → **all** tenant machines. **No server-side filtering** — every tested query param
  (`name=`, `env.X=`, `owner=`, `label=`) was ignored and returned the full list.
- `GET /v1/machines/<id>` (200), `DELETE /v1/machines/<id>` (204).
- `name` and `env` round-trip verbatim on read.

### Isolation primitive (broker-enforced)

The broker stamps every machine it creates with **`env.PILOT_OWNER = <callerID>`** (the verified Pilot
pubkey) and a `name` prefix `smol-<callerShort>-…`. Because the cloud can't filter, the broker:

- **list** → `GET /v1/machines`, return only rows whose `env.PILOT_OWNER == caller`.
- **exec/delete/files/get** → `GET /v1/machines/<id>` first; proceed only if `env.PILOT_OWNER == caller`,
  else `403`. A user can never name another user's machine id and act on it.

## Registry / artifact publish (the `smol.push` target)

- Registry is OCI at `registry.smolmachines.com` (`GET /v2/` → `401 Bearer realm=…/v2/auth`). Direct
  Docker-auth token exchange with the master key yields only an **anonymous, no-access** token, so
  registry writes are **mediated by the `/v1` API**, not done directly by clients.
- `GET /v1/groups` → `[]`; `OPTIONS` → `GET,HEAD,POST`. `POST /v1/groups` requires `{"name":…,
  "image":…}` (422 reveals the fields) — a **group** is a named smolmachine artifact backed by an OCI
  `image`. This is the artifact-publish entry point.

### `smol.push` flow (v1, broker-mediated)

1. Adapter (local, hybrid): `smolvm pack`/build the local VM into an OCI image and hand it to the push
   call (bytes streamed to the broker, spooled to a temp file bounded by `ArtifactMaxBytes`; the 8 MiB
   in-memory `MaxBody` cap is bypassed for push while the full-body hash still backs the signature).
2. Broker (master key): publish the image → `POST /v1/groups {name: smol-<callerShort>-<name>, image}`
   in the tenant namespace, then `POST /v1/machines {source:{type:smolmachine,
   reference:"tenants/<tenant>/<group>:<tag>"}, env:{PILOT_OWNER:<caller>}, name}`.
3. Broker debits the caller's credit ledger (reserve-then-refund-on-failure); `402` when short.

> Exact OCI-image handoff (whether the broker pushes the blob via the mediated registry or the local
> `smolvm` push is reused) is finalized in the push implementation with the real `smolvm` CLI in hand;
> the API-level group+machine steps above are verified.

## Credit model (v1)

No `usage:read` on this key → the broker keeps its **own** ledger: seed `SeedCredits` on first
provision, debit `CostCredits[path]` (default 1) per cloud op, `402` at zero. Tenant-level cost is
observable via `/v1/account periodCost` for reconciliation. Billing/top-up is a documented follow-up.
