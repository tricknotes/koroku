package objstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sigAlgorithm    = "AWS4-HMAC-SHA256"
	sigService      = "s3"
	sigTerminator   = "aws4_request"
	amzDateFormat   = "20060102T150405Z"
	amzShortDate    = "20060102"
	unsignedPayload = "UNSIGNED-PAYLOAD"
)

// signer implements AWS Signature Version 4 for S3.
type signer struct {
	accessKeyID     string
	secretAccessKey string
	region          string
}

// signRequest authenticates req with an Authorization header. It sets
// x-amz-date and x-amz-content-sha256, then signs the host header together
// with every header already present on the request. payloadHash is the
// lowercase hex SHA-256 of the request body.
func (s signer) signRequest(req *http.Request, payloadHash string, now time.Time) {
	now = now.UTC()
	req.Header.Set("X-Amz-Date", now.Format(amzDateFormat))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Del("Authorization")

	headers := map[string]string{"host": requestHost(req)}
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ",")
	}
	canonicalHeaders, signedHeaders := canonicalizeHeaders(headers)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	req.Header.Set("Authorization", sigAlgorithm+
		" Credential="+s.accessKeyID+"/"+s.scope(now)+
		",SignedHeaders="+signedHeaders+
		",Signature="+s.signature(canonicalRequest, now))
}

// presign returns u with query-string authentication added, valid for
// expirySeconds from now. Only the host header is signed and the payload is
// left unsigned, so the URL works from any HTTP client.
func (s signer) presign(method string, u *url.URL, expirySeconds int64, now time.Time) string {
	now = now.UTC()
	query := u.Query()
	query.Set("X-Amz-Algorithm", sigAlgorithm)
	query.Set("X-Amz-Credential", s.accessKeyID+"/"+s.scope(now))
	query.Set("X-Amz-Date", now.Format(amzDateFormat))
	query.Set("X-Amz-Expires", strconv.FormatInt(expirySeconds, 10))
	query.Set("X-Amz-SignedHeaders", "host")
	query.Del("X-Amz-Signature")

	canonicalHeaders, signedHeaders := canonicalizeHeaders(map[string]string{"host": u.Host})
	path := canonicalPath(u)
	rawQuery := canonicalQuery(query)
	canonicalRequest := strings.Join([]string{
		method,
		path,
		rawQuery,
		canonicalHeaders,
		signedHeaders,
		unsignedPayload,
	}, "\n")

	return u.Scheme + "://" + u.Host + path + "?" + rawQuery +
		"&X-Amz-Signature=" + s.signature(canonicalRequest, now)
}

// scope is the credential scope: date/region/service/aws4_request.
func (s signer) scope(now time.Time) string {
	return now.Format(amzShortDate) + "/" + s.region + "/" + sigService + "/" + sigTerminator
}

// signature derives the signing key for the day and signs the canonical request.
func (s signer) signature(canonicalRequest string, now time.Time) string {
	stringToSign := strings.Join([]string{
		sigAlgorithm,
		now.Format(amzDateFormat),
		s.scope(now),
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.secretAccessKey), now.Format(amzShortDate))
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, sigService)
	key = hmacSHA256(key, sigTerminator)
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

// requestHost is the value net/http sends in the Host header.
func requestHost(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}

// canonicalizeHeaders renders the canonical headers block (one "name:value\n"
// per header, sorted by name) and the matching signed headers list. Names must
// already be lowercase.
func canonicalizeHeaders(headers map[string]string) (canonical, signed string) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		// Trim and collapse runs of whitespace into a single space.
		b.WriteString(strings.Join(strings.Fields(headers[name]), " "))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalPath is the URI-encoded absolute path: every segment is
// percent-encoded and the slashes between them are kept. S3 does not normalize
// the path and does not double-encode it.
func canonicalPath(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return uriEncode(u.Path, false)
}

// canonicalQuery renders the query string with every name and value
// URI-encoded and the pairs sorted.
func canonicalQuery(query url.Values) string {
	pairs := make([]string, 0, len(query))
	for name, values := range query {
		for _, value := range values {
			pairs = append(pairs, uriEncode(name, true)+"="+uriEncode(value, true))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes s as Signature Version 4 requires: every byte
// except the unreserved characters A-Z a-z 0-9 - . _ ~ becomes %XX with
// uppercase hex digits. A slash is kept when encodeSlash is false (object
// keys in the path) and encoded when it is true (query strings).
func uriEncode(s string, encodeSlash bool) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
