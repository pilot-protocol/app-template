# Guidance for AI coding agents

This repo (`pilot-app`) turns a declarative `pilot.app.yaml` into a signed, publishable
**Pilot app-store** app.

## Publishing or deploying an app? Follow the playbook.

If you are an AI agent tasked with **publishing, deploying, or updating an app** on the
Pilot app-store, read and follow **[`docs/PUBLISHING-PLAYBOOK.md`](docs/PUBLISHING-PLAYBOOK.md)**
end to end. It is the complete runbook across every backend (`http` / `cli`) and auth mode
(`byo` / `managed` broker), native-binary delivery from the R2 registry, testing, and the
submission → catalogue → website-card steps, with edge cases and a pre-flight checklist.
Do not skip its runtime tests. Start there and follow the focused docs it links when a step
needs detail.

## Non-negotiables (the playbook expands each)

- **Never commit or bake a secret.** Publisher keys, API keys, catalogue-signing keys, and
  registry credentials are never committed and never baked into a bundle (bundles are public
  and sha-pinned). An app needing an API key uses `byo` or `managed` auth — never a hardcoded key.
- **Ship the full platform set.** `darwin × linux × arm64 × amd64` (or a true universal
  binary). A single-platform bundle is a build-host accident and is rejected.
- **`app_version` == the upstream tool/release version** for a wrapped tool.
- **Test both ways before you PR:** run every method in socket mode *and* via a real
  `pilotctl appstore install`, on the host OS *and* the other OS. Verify vanilla and
  through-pilot produce the same result.
- **The catalogue is signed and fail-closed.** Keep the catalogue at `version: 2`; the
  publisher pin in the entry must match the bundle manifest's `store.publisher`.
- **One stable publisher key per app id, forever** — back it up; the update gate requires it.
- **Every submission carries a product demo.** Author a `product_demo` in
  `submission.json` — the example-first, skill-file-shaped usage guide shown at install,
  injected as a `SKILL.md`, and rendered on the website. Required by policy for new
  submissions; metered apps MUST show costs ≤ the per-user budget. Guide:
  [`docs/PRODUCT-DEMOS.md`](docs/PRODUCT-DEMOS.md).
- **Every submission carries a next-steps graph.** Author a `next_steps` in
  `submission.json` — the recommended next commands pilotctl prints after *every*
  `appstore call`, on success and failure. The demo drives install→first-call; the graph
  drives first-call→usage. Recommended, not exhaustive: name the flow, not the method
  list. If your app has a mandatory gateway (`signup`, `start`), a `from:"*"` edge must
  route a cold agent to it. Guide:
  [`docs/NEXT-STEPS-GRAPHS.md`](docs/NEXT-STEPS-GRAPHS.md).

## Tool entry points

- `go build -o pilot-app ./cmd/pilot-app` — then `pilot-app example|validate|init|verify|verify-submission|submit`.
- Root [`README.md`](README.md) has the quickstart; `docs/` has the field-level reference,
  the CLI-adapter and native-app archetypes, the managed-key (broker) design, and updating.
