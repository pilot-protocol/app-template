# CLI adapter archetype — design & the `proc.exec` capability

`pilot-app` already **generates** a working CLI-backed adapter (`backend.type:
cli`): `internal/backend/exec.go` runs a subprocess per method, substitutes
`${field}` placeholders from the IPC payload, and returns stdout as the JSON
reply. The generated Go compiles and serves IPC exactly like the HTTP archetype.

What it can't do **yet** is install through the catalogue with permission to
exec, because the platform has no capability for "run a local process." This
doc specifies the one platform change that unblocks it.

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
