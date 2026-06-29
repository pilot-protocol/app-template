# CI: per-app vanilla-vs-Pilot A/B report

`.github/workflows/ab-report.yml` runs an A/B report when a **publish PR** touches
`submissions/<id>/`. It runs each equivalent command two ways — the vanilla CLI
binary vs the Pilot adapter — and posts an HTML report (commands, outputs, exit
codes, timings, and the adapter-generated `<ns>.help`).

## How it works

1. **Detect** the app id from the changed `submissions/<id>/` path (or the
   `workflow_dispatch` `app_id` input).
2. **Extract** the committed bundle (prefers `*linux-amd64*.tar.gz` for the
   ubuntu runner) → `manifest.json`, `bin/<adapter>`, `install.json`.
3. **Stage**: run the adapter with `--socket/--manifest`. On startup it fetches
   this host's artifacts from the R2 registry (per `install.json`), sha-verifies,
   and stages them — exactly as the daemon-spawned adapter does. The staged
   `exec_path` binary is the *vanilla* side.
4. **Run** `scripts/ab_report.py --mode socket`, driving the adapter through
   `cmd/ipc-call` (no daemon needed), with the per-app command set.
5. **Publish**: upload `ab-report.html` as a run artifact and upsert a PR comment
   with the summary table + a link to the artifact.

## ⚠️ No VM boots in CI

GitHub-hosted runners have **no nested virtualization (KVM)**, so VM-launching
commands (`smolvm machine run …`) cannot run there. Keep the CI command set to
non-VM commands that still prove the adapter forwards the full surface:
`--version`, `--help`, and subcommand `--help` (e.g. `machine --help`,
`pack --help`). Run microVM workloads locally with `--mode pilotctl` against a
daemon (see `scripts/ab_report.py`).

## Per-app command set

Add `submissions/<id>/ab-commands.json`:

```json
{
  "commands": [
    {"label": "Version", "vanilla": ["--version"], "method": "<ns>.version", "payload": {}},
    {"label": "machine --help", "vanilla": ["machine","--help"],
     "method": "<ns>.exec", "payload": {"args": ["machine","--help"]}}
  ]
}
```

- `vanilla` — argv passed to the staged binary directly.
- `method` + `payload` — the adapter method and JSON args (use the enumerated
  method for `version`; the passthrough `<ns>.exec` for everything else).

If the file is absent, a built-in default runs `--version` and `--help` via the
passthrough exec method. See `submissions/io.pilot.smolvm/ab-commands.json`.

## Requirements / limitations

- **Platform artifact**: the report runs on `ubuntu-latest`, so the submission's
  R2 artifacts must include a **linux/amd64** build for the adapter to stage.
  Apps that ship only other platforms (e.g. darwin/arm64) will skip with a clear
  error — point the workflow at a matching runner if needed (e.g. `macos-14` for
  darwin/arm64-only apps).
- **Applicability**: only cli apps that **deliver a binary** (ship `install.json`)
  get a report. HTTP apps and cli apps whose binary is assumed-present are
  skipped with a notice.
- **Network**: the runner needs egress to the R2 public URL to stage artifacts.
