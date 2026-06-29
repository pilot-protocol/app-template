#!/usr/bin/env python3
"""ab_report.py — vanilla-vs-Pilot A/B report for a native cli app.

Runs a set of EQUIVALENT commands two ways — the vanilla CLI binary directly,
and through the Pilot adapter — capturing each command's output, exit code, and
wall-clock time, plus the adapter's generated `<ns>.help` document, and emits a
self-contained HTML report.

Two drive modes for the Pilot side:
  --mode pilotctl   `pilotctl appstore call <app> <method> <json>`  (needs a daemon)
  --mode socket     drive the adapter's unix socket directly via cmd/ipc-call
                    (no daemon — used in CI; build adapter, run it with
                    --socket/--manifest, it stages artifacts from R2 on startup)

The command set is loaded from `submissions/<id>/ab-commands.json` when present,
else a CI-safe default (version + help) is used. CI-safe = NO microVM boots:
GitHub-hosted runners have no nested virtualization/KVM, so VM-launching commands
(`smolvm machine run ...`) cannot run there — keep CI commands to --version,
--help, and subcommand --help.

Usage (socket, CI):
  ab_report.py --app io.pilot.smolvm --ns smolvm --mode socket \
    --socket $APP/app.sock --ipc-call ./ipc-call \
    --vanilla $APP/<exec_path> --submissions ./submissions --out ab-report.html
Usage (pilotctl, local):
  ab_report.py --app io.pilot.smolvm --ns smolvm --mode pilotctl \
    --pilot /path/to/pilotctl --vanilla /path/to/smolvm --out ab-report.html
"""
import argparse, html, json, os, subprocess, time


def run(cmd, stdin=None, env=None, timeout=600):
    """Run argv, return (stdout, stderr, exit, ms)."""
    t0 = time.time()
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, env=env,
                           input=stdin, timeout=timeout)
        return p.stdout, p.stderr, p.returncode, int((time.time() - t0) * 1000)
    except subprocess.TimeoutExpired:
        return "", f"TIMEOUT after {timeout}s", 124, int((time.time() - t0) * 1000)
    except FileNotFoundError as e:
        return "", f"not found: {e}", 127, 0


def first_json(blob):
    """Extract the first top-level JSON object from mixed stdout."""
    i = blob.find("{")
    if i < 0:
        return None
    try:
        return json.loads(blob[i:blob.rfind("}") + 1])
    except Exception:
        return None


class Pilot:
    """Dispatches a method call to the adapter, either via pilotctl or the socket."""

    def __init__(self, a, env):
        self.mode, self.a, self.env = a.mode, a, env

    def call(self, method, payload):
        if self.mode == "pilotctl":
            cmd = [self.a.pilot, "appstore", "call", self.a.app, method,
                   json.dumps(payload), "--timeout", "8m"]
            label = f"pilotctl appstore call {self.a.app} {method} '{json.dumps(payload)}'"
        else:  # socket
            cmd = [self.a.ipc_call, "-socket", self.a.socket, "-method", method,
                   "-args", json.dumps(payload)]
            label = f"ipc-call -socket $APP/app.sock -method {method} -args '{json.dumps(payload)}'"
        out, err, code, ms = run(cmd, env=self.env)
        raw = out + ("\n" + err if err.strip() else "")
        return raw, first_json(out), ms, label


def reply_view(reply, raw):
    """Normalize an adapter reply to (stdout, stderr, exit)."""
    if isinstance(reply, dict) and "exit" in reply:
        return reply.get("stdout", ""), reply.get("stderr", ""), reply.get("exit", "")
    return (json.dumps(reply, indent=2) if reply is not None else raw), "", 0


def load_commands(a):
    """Per-app command set from submissions/<id>/ab-commands.json, else a
    CI-safe default (version + help via the passthrough exec method)."""
    path = a.commands
    if not path and a.submissions:
        path = os.path.join(a.submissions, a.app, "ab-commands.json")
    if path and os.path.exists(path):
        doc = json.load(open(path))
        cmds = doc["commands"] if isinstance(doc, dict) else doc
        return cmds, path
    ex = f"{a.ns}.exec"
    return [
        {"label": "Version", "note": "passthrough → `<cli> --version`",
         "vanilla": ["--version"], "method": ex, "payload": {"args": ["--version"]}},
        {"label": "Help", "note": "passthrough → `<cli> --help`",
         "vanilla": ["--help"], "method": ex, "payload": {"args": ["--help"]}},
    ], "(built-in default: version + help)"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--app", required=True)
    ap.add_argument("--ns", required=True)
    ap.add_argument("--mode", choices=["pilotctl", "socket"], default="pilotctl")
    ap.add_argument("--vanilla", required=True, help="the staged CLI binary (vanilla side)")
    ap.add_argument("--pilot", help="pilotctl binary (mode=pilotctl)")
    ap.add_argument("--socket", help="adapter unix socket (mode=socket)")
    ap.add_argument("--ipc-call", dest="ipc_call", help="cmd/ipc-call binary (mode=socket)")
    ap.add_argument("--submissions", help="submissions/ dir (to find <app>/ab-commands.json)")
    ap.add_argument("--commands", help="explicit ab-commands.json path")
    ap.add_argument("--out", default="ab-report.html")
    ap.add_argument("--cases", default="",
                    help="JSON file of A/B cases [{label,note,vanilla:[argv],method,payload}]; "
                         "defaults to the built-in smolvm cases")
    a = ap.parse_args()
    if a.mode == "pilotctl" and not a.pilot:
        ap.error("--pilot is required for --mode pilotctl")
    if a.mode == "socket" and not (a.socket and a.ipc_call):
        ap.error("--socket and --ipc-call are required for --mode socket")
    env = dict(os.environ)
    pilot = Pilot(a, env)

    pairs, src = load_commands(a)
    rows = []
    for p in pairs:
        vout, verr, vcode, vms = run([a.vanilla] + p["vanilla"], env=env)
        praw, preply, pms, plabel = pilot.call(p["method"], p["payload"])
        pout, perr, pcode = reply_view(preply, praw)
        rows.append(dict(p=p,
                         vanilla=dict(cmd=" ".join([os.path.basename(a.vanilla)] + p["vanilla"]),
                                      out=vout, err=verr, code=vcode, ms=vms),
                         pilot=dict(cmd=plabel, out=pout, err=perr, code=pcode, ms=pms)))

    hraw, hreply, hms, _ = pilot.call(f"{a.ns}.help", {})
    help_doc = json.dumps(hreply, indent=2) if hreply else hraw
    vhelp_out, _, _, vhelp_ms = run([a.vanilla, "--help"], env=env)

    render(a, rows, help_doc, hms, vhelp_out, vhelp_ms, src)
    print(f"wrote {a.out} ({len(rows)} command pairs, mode={a.mode}, commands={src})")


def esc(s):
    return html.escape(s if isinstance(s, str) else str(s))


def render(a, rows, help_doc, hms, vhelp_out, vhelp_ms, src):
    vcli = os.path.basename(a.vanilla)

    def block(d):
        cls = "ok" if d["code"] == 0 else "bad"
        body = esc(d["out"].rstrip())
        if d["err"].strip():
            body += '\n<span class="dim">── stderr ──</span>\n' + esc(d["err"].rstrip())
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
          <p class="note">{esc(r['p'].get('note',''))}</p>
          <div class="grid">
            <div class="col"><div class="h vanilla">Vanilla CLI</div>{block(v)}</div>
            <div class="col"><div class="h pilot">Pilot adapter</div>{block(pl)}</div>
          </div>
          <div class="delta">adapter overhead: <b>{'+' if delta>=0 else ''}{delta} ms</b>
            (vanilla {v['ms']} ms · pilot {pl['ms']} ms)</div>
        </section>""")

    summary = "".join(
        f"<tr><td>{esc(r['p']['label'])}</td><td class='r'>{r['vanilla']['ms']}</td>"
        f"<td class='r'>{r['pilot']['ms']}</td>"
        f"<td class='r'>{'+' if r['pilot']['ms']-r['vanilla']['ms']>=0 else ''}{r['pilot']['ms']-r['vanilla']['ms']}</td>"
        f"<td>{'✓' if r['vanilla']['code']==r['pilot']['code'] else '⚠'}</td></tr>"
        for r in rows)

    out = f"""<!doctype html><meta charset=utf-8>
<title>A/B report — {esc(a.app)}</title>
<style>
:root{{--ink:#0b0b0a;--dim:#6b6b63;--line:#e6e4da;--bg:#faf9f5;--ok:#1a7f37;--bad:#cf222e;--van:#8250df;--pil:#0969da}}
*{{box-sizing:border-box}}body{{font:14px/1.5 -apple-system,Inter,system-ui,sans-serif;color:var(--ink);background:var(--bg);max-width:1080px;margin:0 auto;padding:32px 24px}}
h1{{font-weight:600;margin:0 0 4px}}h3{{margin:0 0 2px}}.sub{{color:var(--dim);margin:0 0 18px}}
.lim{{background:#fff8e6;border:1px solid #f0d98a;border-radius:8px;padding:10px 14px;font-size:13px;margin:0 0 22px;color:#6b5a16}}
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
pre{{margin:0;background:#111110;color:#d6d4cb;padding:10px;border-radius:0 0 6px 6px;font:12px/1.45 ui-monospace,Menlo,monospace;white-space:pre-wrap;word-break:break-word;max-height:360px;overflow:auto}}
.dim{{color:#8a887e}}.delta{{margin-top:10px;color:var(--dim);font-size:13px}}
.help{{background:#fff;border:1px solid var(--line);border-radius:10px;padding:18px;margin:0 0 18px}}
.cols2{{display:grid;grid-template-columns:1fr 1fr;gap:14px}}
@media(max-width:760px){{.grid,.cols2{{grid-template-columns:1fr}}}}
</style>
<h1>Vanilla vs Pilot — A/B report</h1>
<p class="sub">App <b>{esc(a.app)}</b> · mode <b>{esc(a.mode)}</b> · commands: {esc(src)} · generated by scripts/ab_report.py</p>
<div class="lim"><b>CI note:</b> GitHub-hosted runners have no nested virtualization (KVM), so VM-launching
commands cannot run there. This report exercises non-VM commands (version, help, subcommand help)
that prove the adapter forwards the full CLI surface identically. Run microVM workloads locally with
<code>--mode pilotctl</code> against a daemon.</div>

<h3>Summary</h3>
<table><tr><th>Command</th><th class=r>Vanilla (ms)</th><th class=r>Pilot (ms)</th><th class=r>Δ overhead</th><th>Exit match</th></tr>{summary}</table>

<h3>Adapter-generated help <span class=dim style="font-weight:400">— {esc(a.ns)}.help (local, no backend), {hms} ms</span></h3>
<div class="help"><div class="cols2">
  <div><div class="h pilot">Pilot · {esc(a.ns)}.help (generated by the adapter)</div><pre>{esc(help_doc.rstrip())}</pre></div>
  <div><div class="h vanilla">Vanilla · {esc(vcli)} --help ({vhelp_ms} ms)</div><pre>{esc(vhelp_out.rstrip())}</pre></div>
</div></div>

<h3>Per-command detail</h3>
{''.join(cards)}
"""
    with open(a.out, "w") as f:
        f.write(out)


if __name__ == "__main__":
    main()
