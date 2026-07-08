# Broker-side signup — a per-user key with no email, no code

Some providers gate their API key behind an **email one-time code**: you register
with an email, they send a 6-character code, you confirm it, and only then do you
get a key. That is fine for a human, but it breaks an autonomous agent — and
providers routinely **suppress disposable-mail domains**, so a throwaway inbox
never receives the code.

The **broker-signup** pattern makes this fully autonomous: `app.signup {}` — no
arguments — returns a working, per-user key. It complements the no-broker
`register` / `verify` flow (where the user brings their own email and pastes the
code); pick whichever fits your provider and your users.

## Architecture

Two small services, plus a one-line adapter route:

```
 user (adapter, keyless)              signup broker                 mail server            provider
 ───────────────────────             ─────────────                 ───────────            ────────
 app.signup {}  ──signed──▶  1. provision pilot_<rand>@<domain>  ──▶  (start accepting)
                             2. POST provider register {email,pw} ─────────────────────▶  201, emails code
                             3.  provider MTA ── SMTP :25 ─────────▶  receive + store
                             4. read the code (control API)  ◀─────────
                             5. POST provider verify {email,code} ───────────────────────▶ 200 {api_key}
                             6. tear the mailbox down
   {email, api_key} ◀──────  7. return; adapter caches to secrets.json; ops stay byo
```

- **Mail server** (`internal/otpmail`) — a **receive-only** SMTP catch-all for one
  domain plus a token-authed control API (`provision` / `otp` / `teardown`). We
  only ever *receive*, so there is **no SPF/DKIM/PTR/reputation** to manage — just
  an `MX` for the domain and inbound `:25`. It keeps mail only for currently
  provisioned addresses, stores it on tmpfs, returns the parsed code once, and
  never logs the code or the message body.
- **Signup broker** (`internal/otpsignup`) — a signed HTTP service that runs the
  handshake above. It reuses the shared broker's ed25519 caller-identity
  verification, is **idempotent per caller** (at most one provider account per
  Pilot identity — a repeat call or a fresh install returns the same account),
  seals the account password + key **encrypted at rest**, and applies a per-IP
  cap so a caller can't farm accounts. It returns the key and does **not** stay in
  the data path — operational calls go direct to the provider with the cached key.
- **Adapter** — a `signup: { step: broker }` route signs one keyless call to the
  broker and caches `{email, api_key}` to `$APP/secrets.json`; a
  `signup: { step: account }` route reads it back. All other methods stay byo
  (`x-api-key: ${...}` resolved per request from `secrets.json`).

## Authoring an app that uses it

In the submission (or `pilot.app.yaml`):

```yaml
backend: { type: http, base_url: https://api.provider.example/v1, auth: byo,
           headers: { x-api-key: "${PROVIDER_API_KEY}" } }
methods:
  - name: myapp.signup            # one call, no arguments
    latency: slow
    signup: { step: broker, broker_url: https://broker.pilotprotocol.network/<app>/signup,
              secret_key: PROVIDER_API_KEY, email_key: PROVIDER_ACCOUNT_EMAIL }
  - name: myapp.account           # retrieve the cached account
    latency: fast
    signup: { step: account, secret_key: PROVIDER_API_KEY, email_key: PROVIDER_ACCOUNT_EMAIL }
  # ... your byo operational methods, using ${PROVIDER_API_KEY} in the header ...
```

The generator emits the signer + the broker-call runtime and grants `key.sign`
and `net.dial` to the broker host; `fs.read`/`fs.write` on `secrets.json` come
with any signup route. The key is minted at runtime, so the adapter re-resolves
its `${...}` auth headers per request (no restart needed).

## Deploying the two services

Both are plain, config-driven binaries (`cmd/otpmail`, `cmd/otpsignup-broker`) —
everything provider- and host-specific is environment configuration, nothing is
compiled in. In broad strokes:

1. Run `otpmail` on a host with a public IP, and point a **dedicated subdomain's
   `MX`** (DNS-only, not proxied — a CDN proxy will not pass `:25`) at it. Do not
   touch a domain's apex mail. Its control API listens on a **private** interface,
   reachable only by the broker, behind a bearer token.
2. Run `otpsignup-broker` next to your existing broker; point it at the mail
   server's control API and the provider's register/verify endpoints, give it a
   stable at-rest encryption key, and expose a signed `/<app>/signup` route
   through your reverse proxy (preserve the request URI so the signed path
   matches; forward the real client IP for the per-IP cap).

See each package's `Config` for the full environment contract.

## Security notes

- The mailbox holds a **live code for seconds** — treat it as a secret: tmpfs,
  `0600`, delete on read/teardown, never log the code or the body.
- The broker seals the provider password + key **encrypted at rest** and hands
  the key to the adapter, which owns it in `secrets.json`.
- Receive-only, relay-denied SMTP — no open relay, no outbound mail.
- Mind the provider's own register rate limit (often per source IP): the broker's
  egress IP is the bucket, so throttle mints if volume is high.
