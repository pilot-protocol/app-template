package publish

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// R2 is the Cloudflare R2 artifact registry the publish-server uploads to. It
// holds S3 credentials and presigns PUT/GET URLs with SigV4 (path-style,
// UNSIGNED-PAYLOAD) using only the standard library — no AWS SDK dependency.
//
// Endpoint is the account S3 API endpoint, e.g.
// https://<account>.r2.cloudflarestorage.com. PublicBase is the public read base
// (r2.dev managed URL or a custom domain) used to build install URLs; empty
// means reads go through the signing proxy instead.
type R2 struct {
	Endpoint   string // https://<account>.r2.cloudflarestorage.com
	Bucket     string // pilot-artifacts-dev | pilot-artifacts-prod
	Region     string // "auto" for R2
	AccessKey  string
	SecretKey  string
	PublicBase string // https://pub-….r2.dev  (optional)
}

// R2FromEnv builds an R2 from the standard env vars, or returns (nil) when no
// credentials are configured (the artifact endpoints then report 503).
//
//	R2_ENDPOINT, R2_BUCKET, R2_PUBLIC_BASE,
//	AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_DEFAULT_REGION (default "auto")
func R2FromEnv() *R2 {
	ak := os.Getenv("AWS_ACCESS_KEY_ID")
	sk := os.Getenv("AWS_SECRET_ACCESS_KEY")
	ep := os.Getenv("R2_ENDPOINT")
	bucket := os.Getenv("R2_BUCKET")
	if ak == "" || sk == "" || ep == "" || bucket == "" {
		return nil
	}
	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = "auto"
	}
	return &R2{
		Endpoint: strings.TrimRight(ep, "/"), Bucket: bucket, Region: region,
		AccessKey: ak, SecretKey: sk, PublicBase: strings.TrimRight(os.Getenv("R2_PUBLIC_BASE"), "/"),
	}
}

// ArtifactKey is the canonical object key for one platform binary.
func ArtifactKey(id, version, os, arch, filename string) string {
	return fmt.Sprintf("%s/%s/%s-%s/%s", id, version, os, arch, filename)
}

// PublicURL is the install-time download URL for a key: the public base when set,
// else the signing proxy path served by GET /artifact/<key>.
func (r *R2) PublicURL(key, proxyBase string) string {
	if r.PublicBase != "" {
		return r.PublicBase + "/" + pathEscapeKeepSlash(key)
	}
	return strings.TrimRight(proxyBase, "/") + "/artifact/" + pathEscapeKeepSlash(key)
}

// PresignPut returns a presigned URL the browser can PUT the object to directly.
func (r *R2) PresignPut(key string, expires time.Duration, now time.Time) (string, error) {
	return r.presign("PUT", key, expires, now)
}

// PresignGet returns a presigned URL for reading the object (signing proxy).
func (r *R2) PresignGet(key string, expires time.Duration, now time.Time) (string, error) {
	return r.presign("GET", key, expires, now)
}

func (r *R2) host() string {
	h := r.Endpoint
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	return strings.TrimRight(h, "/")
}

// presign builds a SigV4 query-signed URL (path-style, UNSIGNED-PAYLOAD).
func (r *R2) presign(method, key string, expires time.Duration, now time.Time) (string, error) {
	if r == nil {
		return "", fmt.Errorf("r2: not configured")
	}
	host := r.host()
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + r.Region + "/s3/aws4_request"

	q := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    r.AccessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(expires.Seconds())),
		"X-Amz-SignedHeaders": "host",
	}
	canonicalURI := "/" + r.Bucket + "/" + pathEscapeKeepSlash(key)
	canonicalQuery := canonicalQueryString(q)
	canonicalHeaders := "host:" + host + "\n"
	canonicalRequest := strings.Join([]string{
		method, canonicalURI, canonicalQuery, canonicalHeaders, "host", "UNSIGNED-PAYLOAD",
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := hmacSHA256(
		hmacSHA256(
			hmacSHA256(
				hmacSHA256([]byte("AWS4"+r.SecretKey), []byte(dateStamp)),
				[]byte(r.Region)),
			[]byte("s3")),
		[]byte("aws4_request"))
	sig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	return "https://" + host + canonicalURI + "?" + canonicalQuery + "&X-Amz-Signature=" + sig, nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// canonicalQueryString sorts params by key and RFC3986-encodes both sides
// (every reserved char escaped), as SigV4 requires.
func canonicalQueryString(q map[string]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(q))
	for _, k := range keys {
		parts = append(parts, rfc3986Escape(k, true)+"="+rfc3986Escape(q[k], true))
	}
	return strings.Join(parts, "&")
}

// pathEscapeKeepSlash escapes a key path RFC3986-style but keeps "/" literal
// (S3 canonical URI encodes each segment, not the separators).
func pathEscapeKeepSlash(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = rfc3986Escape(s, true)
	}
	return strings.Join(segs, "/")
}

// rfc3986Escape encodes per AWS rules: unreserved (A-Za-z0-9-_.~) pass through,
// everything else becomes %XX (uppercase). encodeSlash controls "/".
func rfc3986Escape(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// parsePublicKey extracts the object key from a public/proxy URL, for validating
// that a submitted artifact url points into our registry.
func (r *R2) keyFromURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if r.PublicBase != "" {
		if pb, err := url.Parse(r.PublicBase); err == nil && u.Host == pb.Host {
			return strings.TrimPrefix(u.Path, "/"), true
		}
	}
	return "", false
}
