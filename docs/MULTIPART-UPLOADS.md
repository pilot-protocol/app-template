# Multipart uploads — sending a file to a partner API

Some partner endpoints take a file, not JSON. General Legal's document endpoint is
the shape: `POST /api/v1/documents` is `multipart/form-data` carrying a DOCX or PDF
plus a few scalar fields, one of which names the matter the document belongs to.

Declare `multipart:` on the route and the generator handles the rest.

```yaml
- name: gl.document_upload
  summary: "Upload a document for attorney review (20 MiB max)."
  duration: slow
  timeout: 180s
  http:
    verb: POST
    path: /api/v1/documents
    multipart:
      file_field: file        # the form field the partner reads (default "file")
      max_bytes: 20971520     # match the partner's own limit
  params:
    blob_id: "string (required) — from gl.upload_begin"
    deal_id: "string (optional) — attach to an existing matter"
    context_for_legal: "string (optional)"
```

Every param except `blob_id` and the path placeholders becomes a form field, so the
shape an agent reads in `<ns>.help` is the shape the partner receives.

## Why the file is staged instead of sent inline

The obvious design — base64 the file into the method's JSON payload — does not fit,
and this is worth understanding before reaching for it anyway.

Pilot IPC is JSON in, JSON out over a framed unix socket, and `ipc.MaxFrameSize`
caps a single envelope at **1 MiB**; an oversize frame is refused and the connection
dropped. base64 inflates by 4/3, so inline encoding tops out around **740 KiB** of
real file. Partner limits are much larger (General Legal: 20 MiB). That is not a
constant to tune — a 20 MiB document needs a 27 MiB envelope, 27× the frame.

So the file travels in chunks that each fit, and the adapter reassembles it on disk
under `$APP/blobs` before building one multipart body. The IPC layer never carries
more than a chunk and no platform limit has to move.

`internal/multipartkit` asserts this premise directly
(`TestBase64InOneEnvelopeExceedsIPCFrame`) rather than leaving it as a comment, so
if the frame size ever changes the trade-off gets re-examined instead of silently
becoming wrong.

## What an agent does

Three generated methods appear automatically on any app with a multipart route —
`<ns>.upload_begin`, `<ns>.upload_chunk`, `<ns>.upload_abort`. Do not author them.

```bash
# 1. Declare the file. sha256 is the integrity contract over the whole reassembly.
pilotctl appstore call io.pilot.generallegal gl.upload_begin \
  '{"file_name":"nda.docx","content_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","total_bytes":3145728,"sha256":"<64 hex>"}'
# -> {"blob_id":"…","max_chunk_bytes":524288,"next_seq":0}

# 2. Push the bytes, in order, at most max_chunk_bytes of RAW file per call.
pilotctl appstore call io.pilot.generallegal gl.upload_chunk \
  '{"blob_id":"…","seq":0,"data_base64":"…"}'
# -> {"received":524288,"next_seq":1,"complete":false}
#    …the last chunk returns "complete":true once the sha256 verifies.

# 3. Send it.
pilotctl appstore call io.pilot.generallegal gl.document_upload \
  '{"blob_id":"…","deal_id":"…","context_for_legal":"Standard mutual NDA."}'
```

The blob is **single-use**: once the partner has the bytes the adapter drops the
local copy, so a replayed `blob_id` fails rather than uploading twice. Staged
uploads that are never sent are reclaimed on a TTL, and an unfinished staging does
not survive an adapter respawn (its rolling hash and chunk cursor die with the
process, so resuming it could splice a gap into the middle of a document).

## Rules the store enforces

Chunked reassembly is a place to get integrity wrong, so the store is strict:

| Rule | Why |
|---|---|
| `blob_id` is minted, never caller-supplied | a chosen id overwrites someone else's in-flight upload |
| ids are 32 hex chars, validated | an id becomes a filename; nothing that could hold a separator is admitted |
| chunks must be strictly sequential | tolerating a gap or a replay reassembles something the caller never sent |
| declared size is a hard cap as bytes land | not just checked at the end |
| sha256 must match at finalize | the integrity contract over the whole reassembly |
| the file name is reduced to its base name | it is metadata for the form part, never a path |

## Managed apps: two broker steps

A multipart app behind the managed-key broker needs two things in its registry
entry beyond the usual (see [`MANAGED-KEY.md`](MANAGED-KEY.md)):

```json
"forward_content_types": ["multipart/form-data"],
"max_body_bytes": 25165824,
"tenancy": {
  "body_refs": {"deal_id": "deal"},
  ...
}
```

- **`forward_content_types`** — the broker forces `application/json` by default,
  which strips the boundary and makes the body undecodable. This is an allow-list
  rather than a passthrough on purpose: the request media type selects which parser
  the partner runs, and letting a caller choose that freely is the same lever as the
  duplicate-key parser differential tenancy already refuses.
- **`max_body_bytes`** — the broker-wide default (8 MiB) is tuned for JSON calls and
  is smaller than an upload partner's limit. Without this an upload the partner
  would have accepted is refused with `413`.
- **`tenancy.body_refs`** — an upload names the resource it acts on in a **form
  field**, never in the path. An app that forwards multipart while declaring no
  `body_refs` would ownership-check nothing on exactly the route that needs it most,
  so the registry **fails the boot** rather than serve it.

The broker parses the multipart form to check those refs, with the same stance as
the JSON path: unparseable bodies, missing boundaries, and over-budget part counts
all deny; a repeated **ref** field is refused as a parser differential (repeats of
fields nobody checks are fine — they are legal multipart); and a ref arriving as a
file part is denied outright, since file parts are not inspected and that shape
would route a ref past the check.

## Testing an upload app

`docs/PUBLISHING-PLAYBOOK.md` Step 4 applies unchanged, plus:

- Upload a file **larger than 1 MiB**. Anything smaller would fit an inline
  encoding and proves nothing about the transport this design exists for.
- Verify the partner received the bytes **unchanged** — compare sha256, not size.
- If the app is managed, run it through a real broker, not just socket mode: the
  Content-Type forwarding and the form-field ownership check only exist there.

The reference tests are `internal/scaffold/zz_multipart_e2e_test.go` (generated
adapter, real socket, real chunking) and `zz_multipart_broker_e2e_test.go` (the full
adapter → broker → partner topology, including an upload into an unowned resource
being refused).

## Limits

| | |
|---|---|
| chunk | 512 KiB raw per call (`max_chunk_bytes`, reported by `upload_begin`) |
| staged upload | 24 MiB default, or `multipart.max_bytes` |
| parts the broker will parse | 64 |
| ref field value | 4 KiB |
| staging TTL | 30 minutes |

One file per request. A partner endpoint taking several files at once would need
the route to carry several blob ids; nothing we ship requires it yet.
