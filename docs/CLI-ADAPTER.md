# CLI adapter archetype — design & the `proc.exec` capability

`pilot-app` **generates** a working CLI-backed adapter (`backend.type: cli`):
`internal/backend/exec.go` runs a subprocess per method and returns its output
as the JSON reply. The generated Go compiles and serves IPC exactly like the
HTTP archetype (a `go build ./...` regression test pins this).

What it can't do **yet** is install through the catalogue with permission to
exec, because the platform has no capability for "run a local process." This
doc specifies the one platform change that unblocks it.

## Invocation model — enumerated vs passthrough

A method maps to a subprocess one of two ways:

- **Enumerated** (`cli: {args: [...]}`): a baked argv with `${field}`
  placeholders filled from the payload, optionally `params_as_flags: true` to
  append `--key value` for each payload field (sorted, deterministic). A missing
  `${field}` is a hard error, not a silently-empty arg. Use this to expose a
  curated, named surface (`weather.current`, `weather.forecast`).

- **Passthrough** (`cli: {passthrough: true}`): the caller supplies a verbatim
  `args` array, so **every** subcommand of the fronted CLI is reachable without
  enumerating it:

  ```
  pilotctl appstore call io.pilot.toolx toolx.exec '{"args":["status","--short"]}'
  ```

  is exactly `toolx status --short`. This is the "translate all CLI commands"
  shape: one method fronts the whole tool. Both shapes accept an optional
  `stdin` string piped to the child.

Because argv is exec'd directly (no shell), payload values can never be
re-parsed as shell metacharacters. Passthrough is strictly more powerful than an
enumerated surface — the caller chooses the subcommand and flags — so reserve it
for trusted callers and keep CLI apps `guarded` (below).

## Hardening built into the runner

The generated `exec.go` is defensive by default:

- **Scrubbed environment** — the child inherits only a minimal baseline (`PATH`,
  `HOME`, locale, `TMPDIR`) plus vars the spec opts in via
  `backend.env_passthrough: [TOKEN, ...]`. The adapter's own environment (app
  identity, broker secrets) never leaks to the fronted CLI.
- **Bounded output** — stdout/stderr are capped (4 MiB) so a runaway child can't
  OOM the adapter; truncation is flagged in the reply.
- **Structured failures** — a non-zero exit is returned as
  `{"stdout","stderr","exit","truncated"}` rather than an opaque error, so the
  caller sees everything the CLI produced. Only spawn failures (binary missing)
  and timeouts surface as IPC errors; the per-method `timeout`/`duration` bounds
  the run and the child is killed on cancel.

## Why HTTP works today and CLI doesn't

An app's grants are the *only* thing the daemon authorizes (`pkg/manifest`).
The known capability vocabulary (`pkg/manifest/validate.go`, `KnownCaps`) is:

```
fs.read fs.write fs.append fs.delete net.dial net.call ipc.call key.sign audit.log
```

An HTTP adapter needs only `net.dial` — already known, already brokered. A CLI
adapter needs to `exec` a binary, which maps to **none** of these, so a
`{"cap":"proc.exec",...}` grant fails `validateGrant` ("not a known
capability") and the manifest is rejected at install and at every supervisor
scan (`supervisor.go:scanInstalled` → `m.Validate()`).

The generator emits the `proc.exec` grant anyway (so the manifest is
forward-compatible); it just won't pass validation until the platform adds it.

## The change: add `proc.exec`

Smallest viable, in `pilot-protocol/app-store`:

1. **`pkg/manifest/validate.go`** — add `"proc.exec": true` to `KnownCaps`.
   Target semantics: the executable name/path the app may spawn (e.g.
   `weathercli` or an absolute path). Validate that the target is non-empty and
   (recommended) an absolute path or a bare command name — never a shell string.

2. **Broker / supervisor** — `proc.exec` is unusual: unlike `net.dial`, the app
   execs the child *itself* (it already has the binary), so there is nothing for
   the daemon to broker per-call. The grant is therefore a **declared,
   user-consented capability** surfaced at install (`pilotctl appstore install`
   prints grants), not a brokered call. Two enforcement options:
   - **Declaration-only (RC):** the grant documents intent; the user consents at
     install. Lowest effort, matches how `audit.log` already works.
   - **Sandbox-enforced (hardening):** spawn the app under a profile that only
     permits exec of the granted target (seccomp/`execve` allowlist on Linux,
     `sandbox-exec` on macOS). Deferred — tracked with the other RC2 sandbox
     gaps in `app-store/CHANGELOG.md`.

3. **`--local` sideload** — today the sandbox sideload path strips everything
   except `fs.*`/`audit.log` (no net). A CLI app is the one class that *could*
   be useful sideloaded (no network needed), so optionally allow `proc.exec`
   through the local path once declaration-only consent is wired. Until then,
   CLI apps install via the catalogue like net apps do.

## Risk note

`proc.exec` is strictly more powerful than `net.dial`: a granted local binary
runs with the daemon's uid. The install-time consent string should make the
target explicit ("this app may run: /usr/local/bin/weathercli"), and the
publisher-review step (catalogue PR) should treat any `proc.exec` app as
higher-scrutiny than a pure HTTP adapter. Recommend keeping CLI apps
`protection: guarded` once that mode lands.

## Until then

Wrap the CLI behind a tiny HTTP shim (any framework, ~20 lines) and publish it
as an `http` adapter. That ships **today** with zero platform changes and is the
recommended path for an existing CLI whose author controls a host to run it on.
Use the native `cli` archetype when the work to add `proc.exec` is scheduled, or
for local-only/offline tools that shouldn't touch the network at all.
