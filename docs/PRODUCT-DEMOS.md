# Product demos — the usage guide that ships with every app

A **product demo** is a compact, example-driven, skill-file-shaped usage guide
authored **once per app** in `submission.json` under the `product_demo` key. It is
the single source that becomes three surfaces:

1. the block `pilotctl` prints at the **last step of install**
   (`pilotctl appstore install io.pilot.<app>`),
2. a **`SKILL.md`** the harness injects into an agent's skill directory, and
3. the **"Full usage demo"** section on the website store page.

One source, three renders — authored where descriptions and metadata already live,
validated at submit time, and copied verbatim into the catalogue `metadata.json`.

The schema and every rule below come from
[`internal/demo/demo.go`](../internal/demo/demo.go) (types + `Validate`) and
[`internal/demo/render.go`](../internal/demo/render.go) (the three renderers). If a
doc and the code disagree, the code wins — file a bug.

---

## Why a product demo exists

The network has ~250,000 autonomous AI agents (openclaw / hermes and friends) that
**install apps and then never use them**. The failure is not the app — it is that
the app was never *surfaced* to a small-context agent in a shape it could act on. An
agent that just installed `io.pilot.duckdb` has no idea, at the moment it needs to
crunch a CSV, that the tool it already has is the right one, or what the first call
looks like.

`<ns>.help` does not solve this. `help` enumerates **every** capability with full
params — it is the reference, and it is large and complex, exactly the wrong thing
to page into a tight context window. A product demo is the opposite: **short,
copy-pasteable, example-first.** One first call, a handful of worked flows, and (for
metered apps) an explicit cost table. Its job is to drive **right-away, correct
FIRST usage.**

### The skill-file rationale

The demo renders as a `SKILL.md` with YAML frontmatter — the same shape the harness
already uses to decide which skill to load:

```yaml
---
name: io.pilot.duckdb
description: Full usage demo — When you need an in-process SQL/OLAP engine …
when_to_use: When you need an in-process SQL/OLAP engine to query CSV/Parquet/JSON …
---
```

The `name` (= the app id) and `when_to_use` are the **disambiguator**: they are what
stop an agent from reaching for the wrong tool at the wrong time. `when_to_use` is a
single sentence answering *"WHEN should an agent reach for this app?"* — so a
small-context agent, seeing only the frontmatter, can decide whether this is the
tool for the task before it ever reads the body. The body is then *WHAT to run*: the
first call, the worked examples, the cost, the gotchas. Frontmatter routes; body
executes.

---

## The `product_demo` schema, field by field

The object authors write in `submission.json` maps 1:1 to the `Demo` struct in
`demo.go`. Field names below are the JSON tags — write them exactly.

| Field | JSON key | Req? | Meaning |
|---|---|:---:|---|
| Skill | `skill` | **required** | Stable skill identifier. **MUST equal the app id** (`io.pilot.<name>`) so the injected `SKILL.md` and the app line up. |
| Title | `title` | optional | Heading shown on the website and atop the install banner. Defaults to `"Full usage demo"`. |
| WhenToUse | `when_to_use` | **required** | One sentence: *when* an agent should reach for this app. The frontmatter disambiguator. **≤ 240 chars.** |
| Metered | `metered` | **required** | `true` for apps behind a paying broker. When `true`, `cost` is required and the worked example costs are budget-checked. |
| Quickstart | `quickstart` | **required** | The ONE call to run right now — the fastest path to a first successful result. A `Step` (below). |
| Examples | `examples[]` | **required** | **2–6** worked, copy-pasteable flows covering the app's core value. Each a `Step`. |
| Cost | `cost` | **required iff metered** | The budget-checked cost breakdown. A `Cost` (below). Must be **absent** when `metered:false`. |
| Gotchas | `gotchas[]` | optional | **≤ 6** one-liners an agent must know before spending or before a call fails surprisingly. Each **≤ 240 chars.** |
| Next | `next[]` | optional | **≤ 4** pointers to deeper docs (typically `"<ns>.help"`). A pointer, never a dump. |

### `Step` (`quickstart` and each `examples[]` entry)

| Field | JSON key | Req? | Meaning |
|---|---|:---:|---|
| Title | `title` | optional | Short label for the example (shown as the heading). |
| Goal | `goal` | optional | What this call achieves (used as the heading if `title` is empty). |
| Command | `command` | **required** | The exact command. **Must start with `pilotctl appstore call `** and **must call a real `<ns>.*` method** of this app. |
| Expect | `expect` | optional | The output shape to expect (e.g. `{"rows":[[42]],"columns":["answer"]}`). |
| Cost | `cost` | **required on metered spending steps** | Price of running THIS step: `"$0.10"`, `"$0.00 (read)"`, or `"dynamic — see cost.operations"`. |
| Note | `note` | optional | One extra line (e.g. "Poll `agentphone.get_call` for the transcript"). **≤ 400 chars.** |

### `Cost` (metered apps only)

| Field | JSON key | Req? | Meaning |
|---|---|:---:|---|
| Unit | `unit` | **required** | e.g. `"micro-USD (1000000 = $1.00)"` or `"requests (50 free per user)"`. |
| FreeBudget | `free_budget` | **required** | e.g. `"$5.00 per Pilot user"` or `"50 requests per user"`. |
| HardCapUSD | `hard_cap_usd` | **required** | The machine-checkable per-user-per-app ceiling in dollars (e.g. `5.00`). `0` marks a **non-dollar** meter (a request quota) — see below. |
| Operations | `operations[]` | **required** | The price table: one `CostOp` per spending operation. |
| WorkedTotal | `worked_total` | optional | Human summary of what the demo spends (e.g. *"This demo spends $0.12 of your $5.00 budget"*). |
| CheckBalance | `check_balance` | optional | The command to read remaining balance. |

Each `CostOp`: `op` (method name, e.g. `"agentphone.place_call"`), `price`
(`"$0.10"`, `"$0.0432/cpu-hour"`, or `"dynamic"`), and optional `note`
(`"reads are free"`).

---

## Golden example — non-metered (local) app

From [`product-demo/example.local.json`](product-demo/example.local.json) — the
`io.pilot.duckdb` demo. Local app, so `metered:false` and no `cost`:

```json
{
  "skill": "io.pilot.duckdb",
  "title": "Full usage demo",
  "when_to_use": "When you need an in-process SQL/OLAP engine to query CSV/Parquet/JSON or crunch data locally without standing up a database server.",
  "metered": false,
  "quickstart": {
    "goal": "Run your first query",
    "command": "pilotctl appstore call io.pilot.duckdb duckdb.query '{\"sql\":\"SELECT 42 AS answer\"}'",
    "expect": "{\"rows\":[[42]],\"columns\":[\"answer\"]}"
  },
  "examples": [
    {
      "title": "Aggregate a CSV in place",
      "command": "pilotctl appstore call io.pilot.duckdb duckdb.query '{\"sql\":\"SELECT country, count(*) FROM read_csv_auto(\\\"/data/users.csv\\\") GROUP BY 1\"}'",
      "expect": "{\"rows\":[[\"US\",120],[\"DE\",44]]}"
    },
    {
      "title": "Read a Parquet file",
      "command": "pilotctl appstore call io.pilot.duckdb duckdb.query '{\"sql\":\"SELECT * FROM read_parquet(\\\"/data/events.parquet\\\") LIMIT 5\"}'",
      "expect": "{\"rows\":[...]}"
    },
    {
      "title": "Engine version",
      "command": "pilotctl appstore call io.pilot.duckdb duckdb.version '{}'",
      "expect": "{\"version\":\"1.5.4\"}"
    }
  ],
  "gotchas": [
    "File paths resolve inside the app sandbox, not your shell CWD.",
    "duckdb.query returns rows as arrays, not objects — index by column order."
  ],
  "next": [
    "io.pilot.duckdb duckdb.help '{}' — every method with params + latency"
  ]
}
```

## Golden example — metered ($) app

From [`product-demo/example.metered.json`](product-demo/example.metered.json) — the
`io.pilot.agentphone` demo. `metered:true`, so every spending `Step` declares its
`cost`, and the `cost` block prices every operation. The worked flow spends **$0.12**
— comfortably under the **$5.00** `hard_cap_usd`:

```json
{
  "skill": "io.pilot.agentphone",
  "title": "Full usage demo",
  "when_to_use": "When your agent needs to place a real phone call or send an SMS/iMessage to a person (bookings, follow-ups, reminders).",
  "metered": true,
  "quickstart": {
    "goal": "Orient — check your account (free read)",
    "command": "pilotctl appstore call io.pilot.agentphone agentphone.usage '{}'",
    "expect": "{\"plan\":\"managed\",\"credits_remaining\":5000000}",
    "cost": "$0.00 (read)"
  },
  "examples": [
    {
      "title": "Place an autonomous call",
      "command": "pilotctl appstore call io.pilot.agentphone agentphone.place_call '{\"to\":\"+14155551234\",\"systemPrompt\":\"Confirm the 7pm reservation for two.\"}'",
      "expect": "{\"id\":\"call_...\",\"status\":\"queued\"}",
      "cost": "$0.10",
      "note": "Poll agentphone.get_call for the transcript."
    },
    {
      "title": "Send a text",
      "command": "pilotctl appstore call io.pilot.agentphone agentphone.send_message '{\"to\":\"+14155551234\",\"text\":\"On my way\"}'",
      "expect": "{\"id\":\"msg_...\",\"channel\":\"imessage\"}",
      "cost": "$0.02"
    },
    {
      "title": "Read the call transcript (free)",
      "command": "pilotctl appstore call io.pilot.agentphone agentphone.get_call '{\"call_id\":\"call_...\"}'",
      "expect": "{\"status\":\"completed\",\"transcripts\":[...]}",
      "cost": "$0.00 (read)"
    }
  ],
  "cost": {
    "unit": "micro-USD (1000000 = $1.00)",
    "free_budget": "$5.00 per Pilot user",
    "hard_cap_usd": 5.0,
    "operations": [
      { "op": "agentphone.place_call",   "price": "$0.10", "note": "per call" },
      { "op": "agentphone.send_message", "price": "$0.02", "note": "per SMS/iMessage" },
      { "op": "agentphone.buy_number",   "price": "$3.00", "note": "per month; released numbers are gone forever" },
      { "op": "agentphone.usage / get_call / list_*", "price": "$0.00", "note": "all reads are free" }
    ],
    "worked_total": "This demo spends $0.12 of your $5.00 budget (1 call + 1 text; reads free).",
    "check_balance": "pilotctl appstore call io.pilot.agentphone agentphone.usage '{}'"
  },
  "gotchas": [
    "Always use E.164 numbers (+14155551234) — never (415) 555-1234.",
    "You cannot dial 911 or crisis lines; they are blocked.",
    "402 Payment Required means the $5.00 budget is spent — reads still work."
  ],
  "next": [
    "io.pilot.agentphone agentphone.help '{}' — full method list"
  ]
}
```

---

## The rules (enforced by `Demo.Validate`)

These are **hard errors** — a demo that breaks one is not publishable. They live in
`demo.go`; the numbers are the length budgets that keep a demo small enough to
survive a small context window.

1. **`skill` == the app id.** `product_demo.skill` must equal `io.pilot.<name>`
   exactly, or the injected `SKILL.md` and the app do not line up.
2. **`when_to_use` is required and ≤ 240 chars.** One sentence. It is the
   frontmatter disambiguator; keep it a sentence, not a paragraph.
3. **2–6 examples.** Fewer than 2 is not a demo; more than 6 is documentation and
   belongs in `<ns>.help`. (`minExamples = 2`, `maxExamples = 6`.)
4. **Every command is a real `<ns>.*` call.** Each `command` (quickstart and every
   example) **must start with `pilotctl appstore call `** and **must contain
   ` <ns>.`** — i.e. it invokes a method in *this* app's namespace. This catches
   examples pasted from another app and commands an agent can't copy-paste.
5. **`gotchas` ≤ 6 entries, each ≤ 240 chars; `next` ≤ 4 pointers; `note` ≤ 400
   chars.**
6. **Metered apps MUST show a cost breakdown.** If `metered:true`:
   - `cost` is required, with a non-empty `operations` table, and non-empty `unit`
     and `free_budget`.
   - When `hard_cap_usd > 0` (a **dollar-metered** broker): **every spending step
     must declare its `cost`** (e.g. `"$0.00 (read)"` for free reads), and the
     **worked flow — quickstart + examples — must sum to ≤ `hard_cap_usd`.** The
     validator adds up the `$n` amounts and rejects a flow that overspends the
     per-user budget. Annotate each `$`-op.
   - Conversely, if `metered:false`, `cost` must be **absent** (setting it is an
     error).
7. **Respect the real per-user budget.** The ceilings the worked flow must fit are
   the deployed broker budgets in
   [`BROKER_COSTS.md`](../../BROKER_COSTS.md): **$5.00 per Pilot user** for credit
   apps (agentphone, orthogonal, smol), and a **50-request quota** for sixtyfour.
   Keep the demo's worked flow well under the ceiling so an agent that runs it
   verbatim can't exhaust the budget.

### Request-quota apps (`hard_cap_usd: 0`)

Some managed apps meter a **request count**, not dollars — e.g. `io.pilot.sixtyfour`
(50 requests per user). These set `metered:true` and `hard_cap_usd: 0`. The
`operations`, `unit`, and `free_budget` still describe the meter, but there is **no
dollar sum to check**, so per-step `$` costs are not required. Describe the quota in
`unit`/`free_budget` (e.g. `unit: "requests (50 free per user)"`) and keep the worked
flow to a couple of calls.

### Dynamic-priced apps

Apps whose price is set by the response (e.g. `io.pilot.orthogonal`, whose cost is
`priceCents × 10000`) can't state a flat per-call `$`. Mark those steps with a
**`"dynamic"`** cost (e.g. `"dynamic — see cost.operations"`) and price the op as
`"dynamic"` in the `operations` table. A `dynamic` marker excludes that step from the
budget sum and turns off the per-step `$`-required check — so a dynamic-priced flow
is not forced to invent a number it can't know. Still set `unit`, `free_budget`, and
`hard_cap_usd` (the $5.00 ceiling) so an agent knows the envelope.

---

## How it renders (one source, three surfaces)

All three renderers are deterministic (`render.go`), so output is diffable and
testable.

- **At install — `RenderInstall`.** `pilotctl` prints a boxed banner at the last
  step of `pilotctl appstore install io.pilot.<app>`: a headline
  (`<id> installed — <title>`), the **When to use** line, **▶ Run this first** (the
  quickstart command + expected output), **▶ More examples** (each example, with its
  cost suffixed when metered), and for metered apps a **▶ Cost** line (free budget +
  `worked_total` + balance command). This is the moment that turns an install into a
  first call.
- **As a skill — `RenderSkill`.** The harness writes a `SKILL.md` into the agent's
  skill dir with the `name` / `description` / `when_to_use` frontmatter, then the
  body: **Run this first**, **Worked examples**, **Cost** (metered), **Gotchas**,
  **Go deeper** (`next`). The frontmatter is what lets a small-context agent decide
  *when* to reach for the app.
- **On the website — `RenderMarkdown`.** The store page embeds the
  **"Full usage demo"** section: *When to use*, **Run this first**, **Examples**, a
  **Cost** table (Operation | Price | Notes) for metered apps, and **Gotchas**.

---

## How it's validated and scored

**Validation (blocking, at submit + in CI).** A `product_demo` is validated by
`Demo.Validate(appID, ns)`, called from `Submission.Validate` at submit time (its
errors are prefixed `Product demo:`). The CI gate is:

```bash
go test ./internal/publish/ -run TestAllSubmissionDemosValid
```

Every `submission.json` in `submissions/` that carries a `product_demo` must produce
a valid, publishable demo; the test also flags a metered app that ships **no** demo
(the exact app that gets installed and never used). Once valid, the demo flows
verbatim into the catalogue `metadata.json` (`BuildMetadata` copies
`cfg.ProductDemo`).

**Scoring (quality signal, non-blocking).** Beyond the pass/fail gate, a demo is
graded for *quality* by `pilot-app demo-score` (added by a separate workstream; it
reads the non-blocking `Lint` signals in `demo.go`). Use it to sharpen a demo that
already validates — clear `when_to_use`, tight examples, real expected output,
budget-honest costs. A submission should validate **and** clear the `demo-score`
threshold before it goes live.

---

## Authoring checklist

- [ ] `product_demo` is present in `submission.json` (required-by-policy for every
      new submission).
- [ ] `skill` **equals the app id** exactly (`io.pilot.<name>`).
- [ ] `when_to_use` is **one sentence, ≤ 240 chars**, answering *when* to reach for
      this app.
- [ ] `metered` is set correctly (`true` iff the app is behind a paying broker).
- [ ] `quickstart` is the single fastest path to a first success, with `expect`.
- [ ] **2–6 `examples`**, each copy-pasteable, with the output shape in `expect`.
- [ ] **Every `command`** starts with `pilotctl appstore call ` and calls a real
      `<ns>.*` method of this app.
- [ ] **Metered apps:** `cost` present; `operations` prices every spending op;
      `unit` + `free_budget` set; **every spending step declares its `cost`**; the
      worked flow **sums to ≤ `hard_cap_usd`** and ≤ the real per-user budget
      ([`BROKER_COSTS.md`](../../BROKER_COSTS.md)).
- [ ] **Request-quota apps:** `hard_cap_usd: 0`, quota described in
      `unit`/`free_budget`.
- [ ] **Dynamic-priced apps:** dynamic steps/ops marked `"dynamic"`; envelope still
      stated via `unit`/`free_budget`/`hard_cap_usd`.
- [ ] Non-metered apps have **no** `cost` block.
- [ ] `gotchas` ≤ 6 (each ≤ 240 chars); `next` ≤ 4 pointers (typically
      `<ns>.help`).
- [ ] `go test ./internal/publish/ -run TestAllSubmissionDemosValid` is green.
- [ ] `pilot-app demo-score` clears the threshold.
</content>
</invoke>
