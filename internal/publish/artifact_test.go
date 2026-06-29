package publish

import (
	"net/url"
	"testing"
	"time"
)

// Proves the SigV4 presigner against AWS's own published reference vector
// (Signature Version 4 — "Create a presigned URL" GET example). If this matches,
// the signer is correct and can be trusted without a live bucket.
//
//	GET examplebucket.s3.amazonaws.com/test.txt, us-east-1/s3, expires 86400,
//	AKIAIOSFODNN7EXAMPLE / wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY, 20130524T000000Z
//	→ X-Amz-Signature aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404
func TestPresignAWSReferenceVector(t *testing.T) {
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got, err := presignS3(
		"GET",
		"https://examplebucket.s3.amazonaws.com",
		"", // virtual-hosted: bucket is in the host, canonical URI is /test.txt
		"test.txt",
		"us-east-1", "s3",
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		86400*time.Second, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if sig := u.Query().Get("X-Amz-Signature"); sig != want {
		t.Fatalf("X-Amz-Signature = %q, want %q\nfull: %s", sig, want, got)
	}
	if cred := u.Query().Get("X-Amz-Credential"); cred != "AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request" {
		t.Fatalf("unexpected credential %q", cred)
	}
}

func TestArtifactKeyAndPublicURL(t *testing.T) {
	if k := ArtifactKey("io.pilot.toolx", "1.2.3", "darwin", "arm64", "toolx.tar.gz"); k != "io.pilot.toolx/1.2.3/darwin-arm64/toolx.tar.gz" {
		t.Fatalf("key = %q", k)
	}
	c := R2Config{PublicBase: "https://artifacts.pilotprotocol.network"}
	want := "https://artifacts.pilotprotocol.network/io.pilot.toolx/1.2.3/darwin-arm64/toolx.tar.gz"
	if got := c.PublicURL(ArtifactKey("io.pilot.toolx", "1.2.3", "darwin", "arm64", "toolx.tar.gz")); got != want {
		t.Fatalf("public URL = %q, want %q", got, want)
	}
}

// The derived public URL must equal what the scaffold derives from app_version,
// so an upload and the adapter's expectation never disagree.
func TestPublicURLMatchesScaffoldDerivation(t *testing.T) {
	c := R2Config{PublicBase: "https://artifacts.pilotprotocol.network"}
	key := ArtifactKey("io.pilot.toolx", "1.2.3", "linux", "amd64", "toolx")
	if got, want := c.PublicURL(key), "https://artifacts.pilotprotocol.network/io.pilot.toolx/1.2.3/linux-amd64/toolx"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
