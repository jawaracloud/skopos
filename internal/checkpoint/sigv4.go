package checkpoint

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	amzTimeFormat   = "20060102T150405Z"
	amzDateFormat   = "20060102"
	emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type credentials struct {
	accessKey string
	secretKey string
	token     string
}

// signer implements AWS Signature Version 4 for an S3-compatible endpoint.
// It is intentionally small: one HMAC chain, one canonical request, no SDK.
type signer struct {
	creds   credentials
	region  string
	service string
	now     func() time.Time
}

func newSigner(creds credentials, region, service string, now func() time.Time) *signer {
	if now == nil {
		now = time.Now
	}
	return &signer{creds: creds, region: region, service: service, now: now}
}

// sign adds x-amz-date, x-amz-content-sha256, an optional session token, and
// the Authorization header. payloadSHA is the SHA-256 of the exact request
// body, or emptyPayloadSHA for a body-less request.
func (s *signer) sign(req *http.Request, payloadSHA string) {
	now := s.now().UTC()
	amzDate := now.Format(amzTimeFormat)
	date := now.Format(amzDateFormat)

	// Signing is idempotent even if a caller reuses a request: the previous
	// signature must not become part of the next canonical header block.
	req.Header.Del("Authorization")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadSHA)
	if s.creds.token != "" {
		req.Header.Set("x-amz-security-token", s.creds.token)
	}

	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadSHA,
	}, "\n")

	scope := date + "/" + s.region + "/" + s.service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(s.creds.secretKey, date, s.region, s.service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.creds.accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

// canonicalHeaders returns the canonical header block and the signed-header
// list. Host is always signed; all other request headers participate so the
// signature covers what is actually sent.
func canonicalHeaders(req *http.Request) (string, string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for key, values := range req.Header {
		headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteByte(':')
		b.WriteString(strings.Join(strings.Fields(headers[key]), " "))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(keys, ";")
}

func canonicalURI(u *url.URL) string {
	path := u.Path
	if path == "" {
		path = "/"
	}
	return uriEncode(path, false)
}

func canonicalQuery(u *url.URL) string {
	return encodeQueryValues(u.Query())
}

// encodeQueryValues renders a query with the SigV4 encoding rules. The result
// is used both as the canonical query string and as the literal RawQuery, so
// the signed bytes and the sent bytes can never disagree.
func encodeQueryValues(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var parts []string
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		for _, value := range items {
			parts = append(parts, uriEncode(key, true)+"="+uriEncode(value, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode implements RFC 3986 encoding with the SigV4 rule that every byte
// except unreserved characters is percent-encoded. Slashes stay literal in
// paths and are encoded in query values.
func uriEncode(value string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if isUnreserved(c) || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
