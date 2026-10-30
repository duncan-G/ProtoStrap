// Package aws is the small part of AWS the agent calls: instance metadata, Auto Scaling
// lifecycle hooks, STS, EC2 and S3. It signs requests with Signature Version 4 and needs
// nothing outside the standard library.
package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// EmptyPayloadHash is the SHA-256 of an empty body.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials are temporary AWS credentials.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time // The zero time means they never expire.
}

// ExpiresWithin reports whether the credentials are missing or expire within d of now.
func (c Credentials) ExpiresWithin(now time.Time, d time.Duration) bool {
	if c.AccessKeyID == "" {
		return true
	}
	if c.Expires.IsZero() {
		return false
	}
	return !now.Add(d).Before(c.Expires)
}

// HashHex is the hex SHA-256 of b, the form SigV4 wants for a payload hash.
func HashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Sign adds Signature Version 4 headers to req. It signs the host and every header already
// set on req, so set Content-Type and any x-amz-* headers first.
func Sign(req *http.Request, payloadHash string, creds Credentials, region, service string, now time.Time) {
	t := now.UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "user-agent" {
			continue
		}
		trimmed := make([]string, len(values))
		for i, v := range values {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		headers[lower] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, n := range names {
		canonicalHeaders.WriteString(n + ":" + headers[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.Path),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + HashHex([]byte(canonicalRequest))
	signature := hexString(hmacSHA256(signingKey(creds.SecretAccessKey, date, region, service), stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID, scope, signedHeaders, signature))
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexString(b []byte) string { return hex.EncodeToString(b) }

// canonicalPath encodes each segment of a decoded path once, as S3 expects. The other
// services the agent calls only use "/".
func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = uriEncode(s)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery sorts the query by name, then value, with both URI-encoded.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		pairs = append(pairs, pair{uriEncode(k), uriEncode(v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

// uriEncode percent-encodes everything but RFC 3986's unreserved characters.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
