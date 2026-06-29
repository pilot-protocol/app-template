package publish

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// R2Config holds the Cloudflare R2 (S3-compatible) credentials + location the
// presign endpoint uploads artifacts into. All from the environment so secrets
// never live in code. Zero value (no creds) ⇒ the endpoint reports "not
// configured" rather than erroring obscurely.
type R2Config struct {
	Endpoint   string // https://<account>.r2.cloudflarestorage.com
	Bucket     string // pilot-artifacts-prod
	AccessKey  string
	SecretKey  string
	Region     string // "auto" for R2
	PublicBase string // public read base for derived URLs (artifacts.pilotprotocol.network)
}

// R2FromEnv reads the R2 configuration from the environment.
//
//	R2_ENDPOINT (or R2_ACCOUNT_ID), R2_BUCKET, R2_ACCESS_KEY_ID,
//	R2_SECRET_ACCESS_KEY, R2_REGION (default "auto"), R2_PUBLIC_BASE.
func R2FromEnv() R2Config {
	endpoint := strings.TrimRight(os.Getenv("R2_ENDPOINT"), "/")
	if endpoint == "" {
		if acct := os.Getenv("R2_ACCOUNT_ID"); acct != "" {
			endpoint = "https://" + acct + ".r2.cloudflarestorage.com"
		}
	}
	region := os.Getenv("R2_REGION")
	if region == "" {
		region = "auto"
	}
	bucket := os.Getenv("R2_BUCKET")
	if bucket == "" {
		bucket = "pilot-artifacts-prod"
	}
	pub := strings.TrimRight(os.Getenv("R2_PUBLIC_BASE"), "/")
	if pub == "" {
		pub = "https://artifacts.pilotprotocol.network"
	}
	return R2Config{
		Endpoint:   endpoint,
		Bucket:     bucket,
		AccessKey:  os.Getenv("R2_ACCESS_KEY_ID"),
		SecretKey:  os.Getenv("R2_SECRET_ACCESS_KEY"),
		Region:     region,
		PublicBase: pub,
	}
}

// Configured reports whether enough is set to presign uploads.
func (c R2Config) Configured() bool {
	return c.Endpoint != "" && c.AccessKey != "" && c.SecretKey != ""
}

// ArtifactKey is the write-once R2 object key for one platform artifact. The
// version is in the path, so a new app version is a new prefix — an artifact can
// never be overwritten under a live version (immutability is what prevents
// artifact↔version drift). Mirrors scaffold.DerivedAssetURL.
func ArtifactKey(id, version, os, arch, file string) string {
	return fmt.Sprintf("%s/%s/%s-%s/%s", id, version, os, arch, file)
}

// PublicURL is the public-read URL the derived asset URL resolves to.
func (c R2Config) PublicURL(key string) string {
	return strings.TrimRight(c.PublicBase, "/") + "/" + key
}

// PresignPut returns a presigned S3 (SigV4, query-auth) PUT URL for key, valid
// for expires. Path-style addressing (/<bucket>/<key>) keeps the host stable for
// R2. The payload is UNSIGNED-PAYLOAD so the uploader streams arbitrary bytes.
func (c R2Config) PresignPut(key string, expires time.Duration) (string, error) {
	return presignS3("PUT", c.Endpoint, c.Bucket, key, c.Region, "s3",
		c.AccessKey, c.SecretKey, expires, time.Now().UTC())
}

// presignS3 builds a SigV4 query-authenticated URL for an S3-style request. It is
// deliberately dependency-free and exercised against AWS's published reference
// vector in the test, so it can be trusted without a live bucket.
//
// pathStyle: the canonical URI is "/<bucket>/<key>" against the account endpoint
// host (R2 supports both, path-style avoids per-bucket DNS). For the AWS test
// vector (virtual-hosted) the bucket is empty and the host carries it.
func presignS3(method, endpoint, bucket, key, region, service, access, secret string, expires time.Duration, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("bad endpoint %q: %w", endpoint, err)
	}
	host := u.Host
	canonicalURI := "/" + uriEncodePath(key)
	if bucket != "" {
		canonicalURI = "/" + bucket + "/" + uriEncodePath(key)
	}

	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", access+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	canonicalQuery := encodeQuerySorted(q)

	canonicalHeaders := "host:" + host + "\n"
	signedHeaders := "host"
	payloadHash := "UNSIGNED-PAYLOAD"

	canonicalRequest := strings.Join([]string{
		method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := sigV4Key(secret, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	q.Set("X-Amz-Signature", signature)
	return u.Scheme + "://" + host + canonicalURI + "?" + encodeQuerySorted(q), nil
}

// ObjectExists does a presigned HEAD and reports whether the object is already
// present — the write-once guard: refuse to re-issue an upload URL for a key that
// exists (a re-upload requires a new version). A network error is returned so the
// caller can decide; a 404 means "free to upload".
func (c R2Config) ObjectExists(key string) (bool, error) {
	signed, err := presignS3("HEAD", c.Endpoint, c.Bucket, key, c.Region, "s3",
		c.AccessKey, c.SecretKey, 2*time.Minute, time.Now().UTC())
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest("HEAD", signed, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound, http.StatusForbidden:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s: unexpected status %d", key, resp.StatusCode)
	}
}

// ── SigV4 primitives ──────────────────────────────────────────────────────────

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sigV4Key(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// encodeQuerySorted renders query params in sorted key order with RFC3986
// encoding (AWS requires the spaces-as-%20, tilde-unescaped form, which
// url.Values.Encode does not produce verbatim — so we build it ourselves).
func encodeQuerySorted(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncodePath encodes an object key for the canonical URI: each path segment is
// RFC3986-encoded but the "/" separators are preserved.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// uriEncode implements AWS's RFC3986 encoding. Unreserved chars (A-Z a-z 0-9 -._~)
// pass through; everything else is %XX. When encodeSlash is false, "/" is left as
// is (for path segments joined by the caller).
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
