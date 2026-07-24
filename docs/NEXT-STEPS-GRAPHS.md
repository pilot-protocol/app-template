# Next-steps graphs — dynamic context after every call

A **next-steps graph** is the dynamic context an agent sees after every
`pilotctl appstore call` on your app, on success *and* on failure: the small set
of **recommended** commands for where the agent now stands.

It is the companion to the [product demo](PRODUCT-DEMOS.md). The demo fires once,
at install, and drives the install→first-call step. The graph fires on every call
after that, and drives first-call→actual-usage.

```
$ pilotctl appstore call io.pilot.sqlite sqlite.query '{"sql":"SELECT 42"}'
error: ipc: server error: backend: missing required param(s): database
hint:  the app "io.pilot.sqlite" rejected or could not handle "sqlite.query"
next: sqlite needs an explicit database — there is no default
  1. pilotctl appstore call io.pilot.sqlite sqlite.query '{"database":":memory:","sql":"SELECT 42 AS a"}'
     why: pass database (:memory: for a scratch db) (fixes the error above)
```

Authored in `submission.json` next to `product_demo`, validated at submit time,
carried verbatim into the catalogue's `metadata.json`, cached by pilotctl at
install, and rendered from a local file on every call — no network on the call
path.

---

## The model

A graph is a **flat list of edges**, not a node tree. Each edge answers one
question: *the agent just ran `from` and it went `on` — what now?*

```json
{
  "schema": 1,
  "app": "io.pilot.example",
  "edges": [
    {
      "from": "*",
      "on": "err",
      "code": 402,
      "why": "budget exhausted",
      "then": [
        {
          "cmd": "pilotctl appstore call io.pilot.example example.balance '{}'",
          "why": "check what is left before spending again",
          "kind": "recovery"
        }
      ]
    }
  ]
}
```

| field   | meaning |
| ------- | ------- |
| `from`  | the method just called, or `"*"` for any |
| `on`    | `"ok"` (exit 0) or `"err"` (exit 1) |
| `match` | regex over the **outcome payload** — see below |
| `code`  | exact backend HTTP status; `on:"err"` only |
| `why`   | one line of framing, printed above the steps |
| `then`  | 1–3 recommended commands, each with `cmd`, `why`, and optional `kind` |

`kind` is one of `gateway` (must run first), `flow` (the normal forward path), or
`recovery` (fixes the error just printed).

### `match` is over the payload, not the outcome

**This is the subtlety that matters most.** `match` tests:

- the **error text** when `on: "err"`, and
- the **JSON result body** when `on: "ok"`.

Matching the success body is load-bearing, because *the most important case in
this whole feature is not an error*. An app with a mandatory `signup` does not
fail when you skip it — the scaffold's `requireKey` wrapper **soft-fails with
exit 0**:

```json
{"ok": false, "needs_signup": true, "activate": "primitive.signup",
 "message": "No API key on this host yet. Call primitive.signup once ..."}
```

An outcome-only matcher sees a plain success and says nothing — to precisely the
cold agent who most needs the gateway named. So a signup app's gateway edge is an
**`on: "ok"`** edge:

```json
{"from": "*", "on": "ok", "match": "\"needs_signup\"\\s*:\\s*true",
 "why": "no account on this host yet — every other method soft-fails until you sign up",
 "then": [{"cmd": "pilotctl appstore call io.pilot.primitive primitive.signup '{}'",
           "why": "provisions a free account + inbox in one call", "kind": "gateway"}]}
```

### Precedence

The best edge wins on **specificity**, not file order:

| edge                | score |
| ------------------- | ----- |
| `from` exact + `code`  | 6 |
| `from` exact + `match` | 5 |
| `from` exact           | 4 |
| `*` + `code`           | 2 |
| `*` + `match`          | 1 |
| `*`                    | 0 |

Ties break first-in-file, so you can force a winner by ordering. An edge that
declares a `match`/`code` which does **not** match is not a fallback — it simply
does not apply, so an unrelated failure never prints a confidently wrong fix.

### There is no session state

A recommendation is a pure function of `(method, outcome, payload)`. Nothing is
remembered between calls, by design: **the app's own response is the state
signal**. An agent that skipped the gateway finds out because the call it tried
came back `needs_signup` — and that edge names the gateway. A local "have I
signed up?" flag would go stale against the real account; a response cannot.

---

## Authoring rules

**1. Recommended, not exhaustive.** A graph is not `<ns>.help`. `io.pilot.primitive`
exposes 111 methods; its graph names about four. Ask *"what does an agent do next
to get value?"* — not *"what else exists?"* Getters, listers and management
methods are reachable via help and must not crowd out the flow. The cap is three
steps per edge, and most edges want one.

**2. Every step needs a `why`.** A bare command is a guess the agent has to
re-derive. The `why` is what turns a suggestion into a decision.

**3. Cover the failures that end sessions.** Ranked by how often they make an
agent abandon an app:

| what happened | how to key it |
| ------------- | ------------- |
| needs signup | `on:"ok"`, `match:"\"needs_signup\"\\s*:\\s*true"` → the gateway |
| budget exhausted (x402) | `on:"err"`, `code:402` → balance/top-up, then retry |
| per-caller quota | `on:"err"`, `code:429` → back off; do not tell them to top up |
| missing/invalid param | `on:"err"`, `match:"missing required param"` → a **corrected, runnable** call |
| server not started (local apps) | `on:"err"`, `match:"connection refused"` → the `start` gateway |

Do **not** write an edge for `method not found` — pilotctl already prints the
app's `exposes` list for that.

**3a. A 402 edge must lead somewhere real.** "Top up" is only a recommendation if
the app has a way to. Point at the app's own balance method, or at
`io.pilot.wallet` — cross-app steps are legal and validated in CI.

**4. Name the gateway from the wildcard.** If your app has a mandatory first
method, a **`from:"*"`** edge must route a cold agent to it. A gateway advertised
only on the happy path is advertised only to agents who already found it. CI
enforces this (`TestSubmissionGatewaysAreReachable`).

**5. Commands must actually run.** Every `cmd` is copy-pasteable and complete —
real params, real values, valid JSON. A recovery step that still fails is worse
than silence. **Run every command you author.** CI checks the method exists;
only you can check it works.

**6. Placeholders only for genuinely unknowable values, and always say where to
get them.** Some params cannot be known when the graph is authored: your own
inbox address, a `jobId` from a previous call, a container id. A placeholder is
acceptable there — but it must be obviously a placeholder (`YOUR-SUBDOMAIN`, not
`example`), and the step's `why` **must** say exactly where the real value comes
from:

```jsonc
{ "cmd": "pilotctl appstore call io.pilot.primitive primitive.send_email '{\"from\":\"agent@YOUR-SUBDOMAIN.primitive.email\", ...}'",
  "why": "send your first email — use the inbox address primitive.signup returned as `from`",
  "kind": "flow" }
```

Never use a placeholder for a value you could have just written down. If a param
is knowable, write the real value and run it.

---

## Validation

`nextsteps.Graph.Validate` runs at submit time and rejects:

- a method in `from` or a `cmd` that the app does not expose — the gate that
  matters most, since a dead-end hint costs the agent a call *and* its trust in
  everything else the app says;
- a `cmd` that is not a runnable `pilotctl` command, or whose namespace does not
  match the app it calls;
- a step with no `why`; more than 3 steps; more than 40 edges;
- an uncompilable regex (silently inert at runtime — the worst failure mode:
  you think the edge shipped, the agent never sees it);
- `code` on an `on:"ok"` edge; duplicate edges that can never both fire.

CI gates in `internal/publish`:

| test | enforces |
| ---- | -------- |
| `TestAllSubmissionNextStepsValid` | every graph is publishable |
| `TestSubmissionGatewaysAreReachable` | a named gateway is reachable from `*` |
| `TestSubmissionCrossAppStepsResolve` | cross-app steps name real methods |

---

## Runtime behaviour

- Rendered to **stderr**, never stdout. stdout is the call's JSON result and
  agents pipe it into `jq`; one line of prose there breaks the workflow this
  feature exists to encourage.
- `--json` gets a structured `next_steps` object instead of prose: on the error
  envelope for a failed call, on stderr for a successful one.
- `PILOT_NEXT_STEPS=off` disables it.
- **Absent, malformed, future-schema, or unmatched → nothing is printed and the
  call is unchanged.** A hint is a nicety; a call is not. No graph bug may ever
  turn a working call into a failed one.

## Shipping a content fix

A graph lives in `metadata.json`, not in the bundle, so fixing its wording or
adding an edge is a **metadata republish** — a PR to `catalogue/apps/<id>/metadata.json`
plus a catalogue re-sign (`metadata_sha256` changes). **No app rebuild, no new
binary, no R2 upload.** `scripts/publish-rich-from-r2.sh` carries `next_steps`
from the submission into the metadata automatically, including onto an
already-published store page.

**Known limitation.** pilotctl caches the graph at install, and refreshes it on
`appstore upgrade` (which re-runs `install --force`). But `appstore outdated`
compares **app versions**, and a graph-only fix does not bump `app_version` — so
an existing install will not see the new graph until the app's next version bump.
Today the explicit refresh is:

```
pilotctl appstore install <id> --force
```

If graph iteration turns out to be frequent, the fix is to teach `outdated` to
also compare `metadata_sha256` — deliberately not done here, to keep the first
cut small.

## Checklist

- [ ] `schema: 1`, `app` equals your app id
- [ ] a `from:"*"` edge for every failure that ends a session (signup, 402, 429,
      bad params, server-not-started) — as applies to your app
- [ ] if you have a mandatory gateway, a `from:"*"` edge routes to it
- [ ] the happy path continues: `from:"<your gateway>"`, `on:"ok"` → the first
      real utility method
- [ ] ≤3 steps per edge, every step has a `why`
- [ ] **you ran every command you authored, and it worked**
- [ ] `go test ./internal/publish/ -run NextSteps` passes
