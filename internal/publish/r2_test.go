package publish

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPresignStructure checks the presigned URL is well-formed (host, path-style
// key, all required SigV4 query params) without needing live credentials.
func TestPresignStructure(t *testing.T) {
	r := &R2{
		Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "pilot-artifacts-dev",
		Region: "auto", AccessKey: "AKID", SecretKey: "secret",
	}
	key := ArtifactKey("io.pilot.smolvm", "1.2.0", "darwin", "arm64", "smolvm.tar.gz")
	if key != "io.pilot.smolvm/1.2.0/darwin-arm64/smolvm.tar.gz" {
		t.Fatalf("key = %q", key)
	}
	u, err := r.PresignPut(key, 15*time.Minute, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"https://acct.r2.cloudflarestorage.com/pilot-artifacts-dev/io.pilot.smolvm/1.2.0/darwin-arm64/smolvm.tar.gz?",
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=AKID%2F",
		"X-Amz-Expires=900",
		"X-Amz-SignedHeaders=host",
		"X-Amz-Signature=",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("presigned URL missing %q\n%s", want, u)
		}
	}
}

// TestPresignRoundTripLive PUTs an object via a presigned URL and reads it back
// from the public base — the real upload path the website's Artifacts step uses.
// Gated on live R2 creds so CI without secrets skips it.
//
//	R2_ENDPOINT, R2_BUCKET, R2_PUBLIC_BASE, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
func TestPresignRoundTripLive(t *testing.T) {
	r := R2FromEnv()
	if r == nil {
		t.Skip("set R2_ENDPOINT/R2_BUCKET + AWS keys to run the live presign round-trip")
	}
	key := ArtifactKey("io.pilot._presigntest", "0.0.0", "linux", "amd64", "probe.txt")
	body := []byte("pilot presign round-trip " + time.Now().UTC().Format(time.RFC3339Nano))

	putURL, err := r.PresignPut(key, 10*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("presigned PUT failed: HTTP %d: %s", resp.StatusCode, rb)
	}

	if r.PublicBase != "" {
		get, err := http.Get(r.PublicURL(key, ""))
		if err != nil {
			t.Fatalf("public GET: %v", err)
		}
		gb, _ := io.ReadAll(get.Body)
		get.Body.Close()
		if get.StatusCode != 200 || !bytes.Equal(gb, body) {
			t.Fatalf("public read mismatch: HTTP %d, body=%q", get.StatusCode, gb)
		}
	}
	t.Logf("presigned PUT + public read OK for %s", key)
	_ = os.Stdout
}
