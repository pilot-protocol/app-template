<!-- Submission PR to the Pilot app store. See submissions/README.md and
     docs/APP-PUBLISHING-SPEC.md. CI (submission-validate) runs the objective
     gate; a maintainer runs the human checklist below before merge. -->

## App submission

**App id:** `io.pilot.____`
**Version:** `____`
**Publisher:** `____`  (will be added to `pilot-protocol/catalog` `publishers/registry.json`)

### Automated gate
- [ ] `submission-validate` CI is green (bundle sha, manifest valid + signed, `<ns>.help`, id/version).

### Maintainer checklist (SPEC §7.2 — do not merge until all checked)
- [ ] Publisher identity is known / added to the registry.
- [ ] **Grants are proportional** to the described function. Scrutinize any
      `proc.exec`, `fs.write`, `key.sign`, or broad `net.dial` target.
- [ ] `submission.json` description accurately reflects the app.
- [ ] Backend host (for `net.dial`) is one the publisher controls.

> On merge, automation releases the bundle on `pilot-protocol/catalog` and opens
> the catalogue index PR on `TeoSlayer/pilotprotocol`.
