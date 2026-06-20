# Caveats & TODO

Honest record of what is intentionally simple, what is deferred, and what needs
the live environment to finish. The managed-key broker core (caller-identity
verification, allow-list, quota, metering, auth injection, breaker) is
implemented and tested; the items below are the edges.

## Security

- **Daemon identity-file contract.** The managed adapter signs with the ed25519
  key the daemon passes via `--identity`. The loader accepts a base64/hex seed
  (32 bytes) or full key (64 bytes) — `internal/scaffold/templates/signer.go.tmpl`.
  Confirm this matches what the daemon actually writes; if the daemon uses a
  different on-disk format, adjust `loadIdentity` (one function) to match. This
  is the single integration point between the broker and the live daemon.
- **`key.sign` grant.** Managed manifests request `{"cap":"key.sign","target":"self"}`.
  Verify the daemon grants an app the ability to read/sign with its own identity
  under this capability (or whichever capability gates it).
- **Query/body integrity for GET.** The signature covers method, path,
  timestamp, and the body hash — **not** the query string. GET adapters put
  params in the query, so a man-in-the-middle could alter query params without
  breaking the signature. Mitigated by TLS to the broker; if stronger integrity
  is needed, fold the canonicalized query into the signed bytes on both ends.
- **Clock skew.** Replay protection uses a ±5-minute timestamp window. Hosts with
  badly wrong clocks will be rejected (`ErrStale`). Tune `-window` if needed.

## Metering & limits

- **Quota, not rate-limit.** The broker enforces a per-caller **total** call cap
  (`quota`), which bounds spend. It does **not** yet enforce a per-second rate
  limit; a single caller can burst up to their quota quickly. Add a token-bucket
  behind the existing `Store` seam if burst control becomes necessary.
- **Durable store is single-writer.** `SQLiteStore` caps the pool at one
  connection so `Admit` is atomic without explicit locking — correct and simple,
  but it serializes metering writes. Fine for the expected volume; for multiple
  broker replicas sharing state, implement the `Store` interface against Postgres
  or Redis (the broker code does not change — only the constructor).
- **Cost extraction is one field.** `cost_field` reads a single numeric dot-path
  from the partner response (default `cost_cents`). Partners that report cost
  differently (headers, per-line-item) need a small extractor extension.
- **Timeouts are per-app, not per-method.** `timeout_ms` applies to the whole
  app. The audit asked for per-method; per-app covers the need today. If one
  method is much slower, either set the app timeout to its worst case or extend
  the registry with a per-method override map.

## Operations

- **Master keys live in the broker environment.** One env var per app
  (`<NAMESPACE>_MASTER_KEY`). Use a real secret manager in production; never put
  keys in `apps.json` (the registry only names the env var).
- **Registry reload is SIGHUP.** The publish-server writes `apps.json` on
  approval; the broker reloads on `kill -HUP`. If the broker and publish-server
  run on different hosts, the registry file must be on shared storage (or shipped
  to the broker host) — the `docker-compose.yml` shares a volume for the local
  case.
- **Docker image builds need a running daemon.** The Dockerfiles and
  `docker-compose.yml` are validated (`docker compose config`) and their exact
  build commands run clean locally, but a full `docker build` / `compose up`
  was not run here because no Docker daemon was available. Run
  `make docker-broker` (or the `deploy-broker` workflow) once on a host with
  Docker to confirm the image.
- **gcloud VM e2e is pending auth.** The real-process, multi-user broker e2e
  passes on this machine (`scripts/e2e-broker.sh`). Running the same against a
  gcloud VM needs `gcloud auth login` (the session's credentials had expired).

## Publishing flow

- **Managed auth header.** For a managed app, the broker injects the master key
  as the first header named in the submission (default `Authorization`). Make
  sure the submission/website form lets a managed publisher name the partner's
  auth header (e.g. `x-api-key`) — it is metadata, not a secret.
- **No per-app default quota.** Approved managed apps register with `quota: 0`
  (unlimited). Set a sane per-caller cap per app before going wide.

## Tests

- `publish` package coverage is ~51%: the uncovered paths shell out to `go build`
  (adapter compilation), `git` (publish trigger), and SendGrid (email send), and
  aren't worth mocking. The broker (security-critical) is ~80% and gated in CI.
