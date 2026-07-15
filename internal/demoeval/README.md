# demoeval — does a product demo actually drive usage?

The Pilot network has ~250k autonomous agents (openclaw / hermes harnesses) that
**install apps but never call them** — the app was never surfaced well to a
small-context agent. [Product demos](../demo) are the fix: a compact, example-
driven SKILL.md injected at install time so an agent knows *when* to reach for the
app and *what* to run. This package quantifies whether a given demo will pay off.

It measures two things.

## 1. Potential uplift (deterministic, no LLM) — what this package computes

**Rubric score (0–100)** — `Score(d, appID, ns) Report`. A structural quality
grade with a per-criterion breakdown and human-readable reasons. It never needs
the app's method set; it judges the demo's *shape*. Criteria and weights:

| Criterion | Weight | Why it maps to fleet usage |
|---|--:|---|
| `quickstart` | 18 | One runnable first call with an expected output is the single thing that converts an install into a first success. |
| `examples` | 16 | 2–6 worked, real `<ns>.*` calls cover the app's core value without becoming documentation. |
| `copy_pasteable` | 16 | Every command must start with the real call prefix and show its output, or an agent can't paste-and-verify. |
| `brevity` | 14 | The rendered SKILL must fit a small context window (target 3000 B, credit falls off to 2×). A demo the agent can't afford to read drives nothing. |
| `skill_discipline` | 14 | `skill == appID` + a `next` pointer + ≤6 gotchas are what make the injected SKILL.md wire up and stay scannable. |
| `cost_discipline` | 12 | Metered apps must show a price table, a balance check, an in-budget worked flow, and a cost on every spending step — or an agent silently burns the user's credits. Non-metered apps get full credit. |
| `when_to_use` | 10 | A single-sentence disambiguator stops an agent reaching for the wrong tool at all. |

Higher weight = closer to the moment of conversion. The three "will an agent even
try, and succeed on the first go" criteria (quickstart, examples, copy-paste)
carry the most; the framing/hygiene criteria carry less.

**First-call proxy (0–1)** — `SimulateFirstCall(d, methodSet) FirstCallResult`.
The reproducible stand-in for "usage uplift". It parses the demo's own commands,
extracts each method name + JSON arg keys, and checks them against the app's
**real declared methods and params** (`MethodSetFromSubmission` /
`LoadMethodSet`). The insight: *a demo whose commands reference real methods with
plausible args means an agent copying it makes a valid first call; one that
doesn't, fails.* It reports `Reachable` (parses to a runnable call), `MethodValid`
(the method exists), `ArgsPlausible` (no invented arg keys), and a folded `Score`.
A missing *required* param is a note + score dock (an agent can add it); an
*invented* key or *unknown method* is a hard miss (the demo teaches a wrong call).

`ScoreSubmissionsDir(dir)` runs both over `submissions/*/submission.json` and
`Summarize` rolls them into mean score, with/without-demo counts, and the list of
apps below the gate.

## 2. Actual uplift — [`scripts/demo-telemetry.sh`](../../scripts/demo-telemetry.sh)

The potential score predicts; telemetry confirms. The actual metric is
**install→first-call conversion**, per app, sliced before/after the demo rollout:

```
conversion(app) = distinct callers with >=1 non-read call within 7d of install
                  --------------------------------------------------------------
                                    distinct callers who installed
```

Reads (`help`/`usage`/`get_*`/`list_*`/`version`) are excluded — an install that
only ever calls `<ns>.help` is a bounce, not usage. A demo "worked" if conversion
rose after it shipped. The script pulls this from the broker journal on the
`pilot-publish` / `smol-broker` VMs (and an optional aggregator); it degrades
gracefully and never fails when telemetry is out of reach. See its header for the
exact query and how to run it on an authorized host.

## Run it

```bash
# Per-app table (quality + first-call proxy + metered $ vs budget) and summary.
# Exits nonzero if any demo scores below -min (default 60) — a CI gate.
pilot-app demo-score submissions
pilot-app demo-score submissions -min 70
pilot-app demo-score submissions -json      # machine-readable

# Actual-uplift query (safe to run anywhere; documents itself if telemetry is out of reach).
./scripts/demo-telemetry.sh
```

Together: **demo-score gates the demos we ship (potential); demo-telemetry proves
they moved the number (actual).**
