#!/usr/bin/env python3
"""ab_report.py — vanilla-vs-Pilot A/B report for a native cli app.

Runs a set of EQUIVALENT commands two ways — the vanilla CLI binary directly,
and through the Pilot app store (`pilotctl appstore call <id> ...`) — capturing
each command's output, exit code, and wall-clock time, plus the adapter's
generated `<ns>.help` document. Emits a self-contained HTML report.

Reused by CI (.github/workflows): on a PR tied to an app, build + install that
app, then run this to produce the report artifact.

Env / args:
  PILOT_APPSTORE_ROOT, PILOT_APPSTORE_CATALOG_URL  passed through to pilotctl
Usage:
  ab_report.py --app io.pilot.smolvm --ns smolvm \
      --pilot /path/to/pilotctl --vanilla /path/to/smolvm --out report.html
"""
import argparse, html, json, os, subprocess, sys, time


def run(cmd, stdin=None, env=None):
    """Run argv, return (stdout, stderr, exit, ms)."""
    t0 = time.time()
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, env=env,
                           input=stdin, timeout=600)
        ms = int((time.time() - t0) * 1000)
        return p.stdout, p.stderr, p.returncode, ms
    except subprocess.TimeoutExpired:
        return "", "TIMEOUT after 600s", 124, int((time.time() - t0) * 1000)


def pilot_call(pilot, app, method, payload, env):
    """pilotctl appstore call <app> <method> <json> → (raw, reply_obj, ms)."""
    out, err, code, ms = run([pilot, "appstore", "call", app, method,
                              json.dumps(payload), "--timeout", "8m"], env=env)
    reply = None
    blob = out
    i = blob.find("{")
    if i >= 0:
        j = blob.rfind("}")
        try:
            reply = json.loads(blob[i:j + 1])
        except Exception:
            reply = None
    return (out + ("\n" + err if err.strip() else ""), reply, ms)


def reply_view(reply, raw):
    """Normalize a pilot reply to (stdout, stderr, exit)."""
    if isinstance(reply, dict) and "exit" in reply:
        return reply.get("stdout", ""), reply.get("stderr", ""), reply.get("exit", "")
    # enumerated/help replies are raw JSON objects
    return (json.dumps(reply, indent=2) if reply is not None else raw), "", 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--app", required=True)
    ap.add_argument("--ns", required=True)
    ap.add_argument("--pilot", required=True)
    ap.add_argument("--vanilla", required=True)
    ap.add_argument("--out", default="ab-report.html")
    ap.add_argument("--cases", default="",
                    help="JSON file of A/B cases [{label,note,vanilla:[argv],method,payload}]; "
                         "defaults to the built-in smolvm cases")
    a = ap.parse_args()
    env = dict(os.environ)

    # Equivalent command pairs. Each: label, the smolvm argv, the pilot method +
    # payload, and whether it boots a VM (for the note).
    argv_run = ["machine", "run", "--net", "--image", "alpine", "--", "sh", "-c",
                "echo hello from microVM; uname -a; cat /etc/alpine-release"]
    argv_py = ["machine", "run", "--net", "--image", "python:3.12-alpine", "--",
               "python3", "-c", "print('2**100 =', 2**100)"]
    pairs = [
        dict(label="Version", note="enumerated method → `smolvm --version`",
             vanilla=["--version"], method=f"{a.ns}.version", payload={}),
        dict(label="List machines", note="passthrough → `smolvm machine ls`",
             vanilla=["machine", "ls"], method=f"{a.ns}.exec",
             payload={"args": ["machine", "ls"]}),
        dict(label="Run command in an ephemeral Alpine microVM",
             note="boots a real isolated VM (separate kernel)",
             vanilla=argv_run, method=f"{a.ns}.exec", payload={"args": argv_run}),
        dict(label="Compute in a Python microVM",
             note="pulls python:3.12-alpine, runs Python in the VM",
             vanilla=argv_py, method=f"{a.ns}.exec", payload={"args": argv_py}),
    ]
    if a.cases:
        with open(a.cases) as f:
            pairs = json.load(f)

    rows = []
    for p in pairs:
        vout, verr, vcode, vms = run([a.vanilla] + p["vanilla"], env=env)
        praw, preply, pms = pilot_call(a.pilot, a.app, p["method"], p["payload"], env)
        pout, perr, pcode = reply_view(preply, praw)
        rows.append(dict(p=p, vanilla=dict(cmd=" ".join([os.path.basename(a.vanilla)] + p["vanilla"]),
                                           out=vout, err=verr, code=vcode, ms=vms),
                         pilot=dict(cmd=f"pilotctl appstore call {a.app} {p['method']} '{json.dumps(p['payload'])}'",
                                    out=pout, err=perr, code=pcode, ms=pms)))

    # Adapter-generated help document.
    hraw, hreply, hms = pilot_call(a.pilot, a.app, f"{a.ns}.help", {}, env)
    help_doc = json.dumps(hreply, indent=2) if hreply else hraw
    vhelp_out, _, _, vhelp_ms = run([a.vanilla, "--help"], env=env)

    render(a, rows, help_doc, hms, vhelp_out, vhelp_ms)
    print(f"wrote {a.out}")


def esc(s):
    return html.escape(s if isinstance(s, str) else str(s))


def render(a, rows, help_doc, hms, vhelp_out, vhelp_ms):
    def block(d):
        cls = "ok" if d["code"] == 0 else "bad"
        body = esc(d["out"].rstrip())
        if d["err"].strip():
            body += f'\n<span class="dim">── stderr ──</span>\n' + esc(d["err"].rstrip())
        return (f'<div class="cmd">{esc(d["cmd"])}</div>'
                f'<div class="meta"><span class="badge {cls}">exit {d["code"]}</span>'
                f'<span class="t">{d["ms"]} ms</span></div>'
                f'<pre>{body}</pre>')

    cards = []
    for r in rows:
        v, pl = r["vanilla"], r["pilot"]
        delta = pl["ms"] - v["ms"]
        cards.append(f"""
        <section class="pair">
          <h3>{esc(r['p']['label'])}</h3>
          <p class="note">{esc(r['p']['note'])}</p>
          <div class="grid">
            <div class="col"><div class="h vanilla">Vanilla CLI</div>{block(v)}</div>
            <div class="col"><div class="h pilot">Pilot app store</div>{block(pl)}</div>
          </div>
          <div class="delta">adapter overhead: <b>{'+' if delta>=0 else ''}{delta} ms</b>
            (vanilla {v['ms']} ms · pilot {pl['ms']} ms)</div>
        </section>""")

    summary = "".join(
        f"<tr><td>{esc(r['p']['label'])}</td><td class='r'>{r['vanilla']['ms']}</td>"
        f"<td class='r'>{r['pilot']['ms']}</td>"
        f"<td class='r'>{'+' if r['pilot']['ms']-r['vanilla']['ms']>=0 else ''}{r['pilot']['ms']-r['vanilla']['ms']}</td>"
        f"<td>{'✓' if r['vanilla']['code']==r['pilot']['code']==0 else '⚠'}</td></tr>"
        for r in rows)

    out = f"""<!doctype html><meta charset=utf-8>
<title>A/B report — {esc(a.app)}</title>
<style>
:root{{--ink:#0b0b0a;--dim:#6b6b63;--line:#e6e4da;--bg:#faf9f5;--ok:#1a7f37;--bad:#cf222e;--van:#8250df;--pil:#0969da}}
*{{box-sizing:border-box}}body{{font:14px/1.5 -apple-system,Inter,system-ui,sans-serif;color:var(--ink);background:var(--bg);max-width:1080px;margin:0 auto;padding:32px 24px}}
h1{{font-weight:600;margin:0 0 4px}}h3{{margin:0 0 2px}}.sub{{color:var(--dim);margin:0 0 24px}}
table{{border-collapse:collapse;width:100%;margin:12px 0 28px;background:#fff;border:1px solid var(--line);border-radius:8px;overflow:hidden}}
th,td{{padding:8px 12px;text-align:left;border-bottom:1px solid var(--line);font-size:13px}}th{{background:#f3f1ea;font-weight:600}}td.r{{text-align:right;font-variant-numeric:tabular-nums}}
.pair{{background:#fff;border:1px solid var(--line);border-radius:10px;padding:18px;margin:0 0 18px}}
.note{{color:var(--dim);margin:0 0 12px;font-size:13px}}
.grid{{display:grid;grid-template-columns:1fr 1fr;gap:14px}}
.h{{font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.04em;margin-bottom:6px}}.h.vanilla{{color:var(--van)}}.h.pilot{{color:var(--pil)}}
.cmd{{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;background:#0b0b0a;color:#e8e6df;padding:8px 10px;border-radius:6px 6px 0 0;white-space:pre-wrap;word-break:break-all}}
.meta{{display:flex;gap:10px;align-items:center;background:#1c1c1a;padding:5px 10px}}
.badge{{font-size:11px;font-weight:600;padding:1px 7px;border-radius:99px;color:#fff}}.badge.ok{{background:var(--ok)}}.badge.bad{{background:var(--bad)}}
.t{{color:#b9b7ad;font-size:12px;font-variant-numeric:tabular-nums}}
pre{{margin:0;background:#111110;color:#d6d4cb;padding:10px;border-radius:0 0 6px 6px;font:12px/1.45 ui-monospace,Menlo,monospace;white-space:pre-wrap;word-break:break-word;max-height:340px;overflow:auto}}
.dim{{color:#8a887e}}.delta{{margin-top:10px;color:var(--dim);font-size:13px}}
.help{{background:#fff;border:1px solid var(--line);border-radius:10px;padding:18px;margin:0 0 18px}}
.cols2{{display:grid;grid-template-columns:1fr 1fr;gap:14px}}
@media(max-width:760px){{.grid,.cols2{{grid-template-columns:1fr}}}}
</style>
<h1>Vanilla vs Pilot — A/B report</h1>
<p class="sub">App <b>{esc(a.app)}</b> · delivered from the Pilot R2 artifact registry · generated by scripts/ab_report.py</p>

<h3>Summary</h3>
<table><tr><th>Command</th><th class=r>Vanilla (ms)</th><th class=r>Pilot (ms)</th><th class=r>Δ overhead</th><th>Match</th></tr>{summary}</table>

<h3>Adapter-generated help <span class=dim style="font-weight:400">— {esc(a.ns)}.help (local, no backend), {hms} ms</span></h3>
<div class="help"><div class="cols2">
  <div><div class="h pilot">Pilot · {esc(a.ns)}.help (generated by the adapter)</div><pre>{esc(help_doc.rstrip())}</pre></div>
  <div><div class="h vanilla">Vanilla · smolvm --help ({vhelp_ms} ms)</div><pre>{esc(vhelp_out.rstrip())}</pre></div>
</div></div>

<h3>Per-command detail</h3>
{''.join(cards)}
"""
    with open(a.out, "w") as f:
        f.write(out)


if __name__ == "__main__":
    main()
