package multipartkit

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"sort"
	"strings"
)

// BuildForm assembles one multipart/form-data body: the staged file under
// fileField, plus the scalar fields.
//
// The body is materialised into memory rather than streamed, and that is forced
// by the security model, not laziness. A managed adapter signs
// base64(sha256(BODY)) so the broker can prove who is calling; you cannot hash
// a body you have not finished producing. DefaultMaxBlobBytes is what keeps
// that bounded.
//
// Returns the body and the Content-Type header value — which carries the
// generated boundary and is therefore not optional to propagate. Every hop
// between here and the partner has to preserve it verbatim or the body is
// undecodable on arrival.
func BuildForm(fileField, fileName, fileContentType string, content io.Reader, fields map[string]string) ([]byte, string, error) {
	if strings.TrimSpace(fileField) == "" {
		return nil, "", fmt.Errorf("multipartkit: file field name is required")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// Deterministic field order. Not cosmetic: it makes a signed body
	// reproducible, so a failure can be replayed byte-for-byte when diagnosing
	// a signature or a partner-side parse.
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := w.WriteField(k, fields[k]); err != nil {
			return nil, "", fmt.Errorf("multipartkit: write field %q: %w", k, err)
		}
	}

	// The file part is written with an explicit Content-Type. multipart's own
	// CreateFormFile hardcodes application/octet-stream, which loses the real
	// type the partner uses to decide whether it will accept the document at
	// all.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`,
		escapeQuotes(fileField), escapeQuotes(fileName)))
	ct := strings.TrimSpace(fileContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)

	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", fmt.Errorf("multipartkit: create file part: %w", err)
	}
	if _, err := io.Copy(part, content); err != nil {
		return nil, "", fmt.Errorf("multipartkit: copy file content: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("multipartkit: close writer: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// escapeQuotes matches mime/multipart's own escaper so a file name containing a
// quote or a backslash cannot break out of the Content-Disposition header and
// forge part boundaries.
func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(s)
}
