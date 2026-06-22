# Using Smol Machines via Pilot — utility guide

How an agent drives `io.pilot.smolmachines` (the smolvm microVM engine) through the
Pilot app store. The app is a **passthrough**: one method, `smolmachines.exec`,
forwards a verbatim `smolvm` argv into a hardware-isolated microVM and returns the
result. No methods are enumerated per-subcommand — this guide + `smolmachines.help`
are how you discover the surface.

## Install + discover

```bash
pilotctl appstore install io.pilot.smolmachines        # fetch + verify + stage the binary from the registry
pilotctl appstore view io.pilot.smolmachines           # the long store page (what it does, how to use)
pilotctl appstore call io.pilot.smolmachines smolmachines.help '{}'   # live method surface + params + latency
```

## The one convention: `smolmachines.exec` takes a verbatim argv

```bash
pilotctl appstore call io.pilot.smolmachines smolmachines.exec '{"args":[ ...smolvm argv... ]}'
```
The `args` array is exactly what you would type after `smolvm`. The reply is JSON
`{"stdout","stderr","exit"}` (a non-zero exit is a normal result, not an error).
There is no allowlist — **every** smolvm subcommand and flag is reachable.

## Command surface (what to put in `args`)

| Goal | `args` |
|---|---|
| Run a one-off command in a throwaway VM | `["machine","run","--net","--image","alpine","--","sh","-c","echo hi"]` |
| Run a script in a language image | `["machine","run","--net","--image","python:3.12-alpine","--","python3","-c","print(2**100)"]` |
| List machines | `["machine","ls","--json"]` |
| Create a persistent VM | `["machine","create","--net","--name","dev","--image","ubuntu"]` |
| Start / stop / delete it | `["machine","start","--name","dev"]` · `["machine","stop","--name","dev"]` · `["machine","delete","--name","dev","-f"]` |
| Run in a persistent VM (changes persist) | `["machine","exec","--name","dev","--","apt-get","install","-y","python3"]` |
| Copy a file in, then out | `["machine","cp","./x.py","dev:/workspace/x.py"]` then `["machine","cp","dev:/workspace/out.json","./out.json"]` |
| Pack a portable artifact | `["pack","create","--image","python:3.12-alpine","-o","./py"]` |
| Status of a VM | `["machine","status","--name","dev"]` |

Full command tree: `machine run|exec|create|start|stop|delete|shell|status|ls|cp|update|monitor|prune`,
`pack create|run`, `serve`, `config`. To read smolvm's own reference at runtime:
`{"args":["--help"]}` or `{"args":["machine","run","--help"]}`.

## Conventions & suggestions

- **Networking is OFF by default.** Add `--net` when the workload needs the network. Scope it with `--allow-host H` / `--allow-cidr C`.
- **`run` is ephemeral, `exec` is persistent.** `machine run` discards everything on exit (best for untrusted/one-off). `machine create` + `machine exec` keep filesystem changes across calls — use a stable `--name` and the VM survives between Pilot calls.
- **Use `/workspace`** for data you want to keep or copy out; it persists across `exec` and `stop`/`start`.
- **Pass inputs/outputs with `machine cp`** (host↔VM), or mount a host dir with `-v HOST:GUEST`.
- **Secrets:** `--secret-env GUEST=HOSTVAR` references a *host* env var. The Pilot adapter scrubs the child env to a minimal baseline, so a host var is only visible if the publisher listed it in `env_passthrough`. Prefer `machine cp` of a file, or `--ssh-agent` for git/ssh, when a secret must reach the VM.
- **Pin images by digest** (`name@sha256:…`) for reproducibility.
- **Latency:** VM-booting calls are the `slow` class (seconds: image pull + boot). `machine ls`/`status`/`--help` are sub-second.

## Not supported over Pilot IPC

The adapter is one-shot request/response (no PTY, bounded output). These don't work and should be avoided:
- **Interactive sessions** — `-it`, `machine shell`, interactive `/bin/sh` (no live TTY/stdin stream).
- **Long-running servers** — `serve start` (blocks until the call timeout).
- **Huge stdin/stdout** — `--image -` (multi-GB docker-save over a JSON field); output is capped at 4 MiB (truncation flagged). Use `--image <oci-ref>` or `machine cp` instead.

## End-to-end example

```bash
pilotctl appstore install io.pilot.smolmachines
pilotctl appstore call io.pilot.smolmachines smolmachines.exec \
  '{"args":["machine","run","--net","--image","python:3.12-alpine","--","python3","-c","import platform;print(platform.platform())"]}'
# → {"stdout":"Linux-6.12...-aarch64-with","stderr":"...pull progress...","exit":0}
```
