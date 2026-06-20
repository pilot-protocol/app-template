# TODO

Tracked follow-ups for the managed-key broker and publishing flow. The core
(caller-identity verification, allow-list, per-caller quota, metering, auth
injection, breaker, publish→register) is implemented and tested; these are the
edges, written as work items rather than warnings.

## Security

- [ ] **Re-confirm the daemon identity contract on each daemon release.** The
      adapter signs with the key the daemon writes to `--identity` (`identity.json`:
      `{"private_key":"<base64>","public_key":"<base64>"}`, std-base64 of the
      64-byte ed25519 key). `signer.go.tmpl` → `loadIdentity` reads exactly this
      (verified against `common/crypto.SaveIdentity`). Keep them aligned if the
      daemon format ever changes.
- [ ] **Confirm the `key.sign` capability.** Managed manifests request
      `{"cap":"key.sign","target":"self"}`. Verify the daemon grants an app the
      ability to read/sign with its own identity under this capability.
- [ ] **Sign the query string for GET methods.** The signature covers method,
      path, timestamp, and body hash — not the query. GET params ride in the
      query, so a MITM could alter them without breaking the signature (TLS to the
      broker mitigates). Fold a canonicalized query into the signed bytes on both
      ends if stronger integrity is needed.
- [ ] **Make the replay window configurable per deployment.** Currently ±5 min
      (`-window`). Hosts with bad clocks get `ErrStale`.

## Metering & limits

- [ ] **Add a per-second rate limit** (token bucket behind the `Store` seam) if
      burst control is needed. Today the broker enforces a per-caller *total*
      quota (bounds spend), not a rate.
- [ ] **Multi-replica metering.** `SQLiteStore` is single-writer (atomic `Admit`
      without external locks). For multiple broker replicas sharing state,
      implement `Store` against Postgres/Redis — the broker code doesn't change,
      only the constructor.
- [ ] **Richer cost extraction.** `cost_field` reads one numeric dot-path
      (default `cost_cents`). Extend for partners that report cost in headers or
      per-line-item.
- [ ] **Per-method timeouts.** `timeout_ms` is per-app today. Add a per-method
      override map if one method is much slower than the rest.

## Operations

- [ ] **Move master keys into a secret manager.** One env var per app
      (`<NAMESPACE>_MASTER_KEY`); never in `apps.json` (it only names the env var).
- [ ] **Shared registry storage for multi-host.** The publish-server writes
      `apps.json` on approval; the broker reloads on `SIGHUP`. If they run on
      different hosts, put the registry on shared storage or ship it to the broker
      (the compose stack shares a volume locally).

## Publishing flow

- [ ] **Expose `auth: managed` + auth header + quota in the website form.** The
      API and broker already accept them; the form is the remaining surface so a
      publisher can pick "managed", name the partner auth header, and set the
      per-caller rate limit.
- [ ] **Per-app default quota policy.** Apps register with the submitted `quota`
      (0 = unlimited). Decide a sane default cap before going wide.

## Tests / CI

- [ ] **Raise `publish` package coverage** (~51%). The gaps shell out to
      `go build`, `git`, and SendGrid; mock or wrap them if coverage matters more.
- [ ] **Confirm the Docker image build on a host with a daemon** (`make
      docker-broker`). Compose is validated and the build commands run clean
      locally, but a full `docker build` needs a running daemon.
