package objstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

const (
	// defaultHTTPTimeout bounds one whole request, including reading the body
	// returned by Get.
	defaultHTTPTimeout = 60 * time.Second
	// maxErrorBodyBytes and maxListBodyBytes bound how much of a response is
	// read into memory.
	maxErrorBodyBytes = 64 << 10
	maxListBodyBytes  = 32 << 20
	// maxListPages stops List when a server keeps returning continuation tokens.
	maxListPages = 10000
	// maxErrorMessageBytes truncates the server supplied message in errors.
	maxErrorMessageBytes = 512

	defaultContentType = "application/octet-stream"
)

var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// S3Config configures an S3-compatible bucket.
type S3Config struct {
	// Endpoint is the base URL of the S3 API without a bucket or path, for
	// example https://s3.<region>.amazonaws.com.
	Endpoint string
	// Region is the signing region. Some providers accept "auto".
	Region string
	// Bucket is the name of an existing bucket.
	Bucket string
	// AccessKeyID and SecretAccessKey authenticate every request.
	AccessKeyID     string
	SecretAccessKey string
	// HTTP is the client used for requests. The default has a 60 second
	// timeout that covers the whole exchange, including the body read by Get.
	HTTP *http.Client
	// Clock supplies the signing time. The default is clock.System{}.
	Clock clock.Clock
}

// String renders the configuration without the credentials.
func (c S3Config) String() string {
	return fmt.Sprintf("objstore.S3Config{Endpoint: %q, Region: %q, Bucket: %q}", c.Endpoint, c.Region, c.Bucket)
}

// GoString keeps %#v from printing the credentials.
func (c S3Config) GoString() string { return c.String() }

// S3 is a Store backed by an S3-compatible API. It uses path-style addressing
// (<endpoint>/<bucket>/<key>) and Signature Version 4.
type S3 struct {
	scheme string
	host   string
	bucket string
	signer signer
	http   *http.Client
	clock  clock.Clock
	// listPageSize is sent as max-keys when positive. Tests lower it to
	// exercise pagination; production uses the server default of 1000.
	listPageSize int
}

var _ Store = (*S3)(nil)

// NewS3 validates cfg and returns a store for the bucket. It makes no request.
func NewS3(cfg S3Config) (*S3, error) {
	scheme, host, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, errors.New("objstore: region is required")
	}
	if !bucketNamePattern.MatchString(cfg.Bucket) {
		return nil, fmt.Errorf("objstore: invalid bucket name %q", cfg.Bucket)
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("objstore: access key id and secret access key are required")
	}
	s := &S3{
		scheme: scheme,
		host:   host,
		bucket: cfg.Bucket,
		signer: signer{
			accessKeyID:     cfg.AccessKeyID,
			secretAccessKey: cfg.SecretAccessKey,
			region:          cfg.Region,
		},
		http:  cfg.HTTP,
		clock: cfg.Clock,
	}
	if s.http == nil {
		s.http = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if s.clock == nil {
		s.clock = clock.System{}
	}
	return s, nil
}

// parseEndpoint returns the scheme and the host exactly as net/http will send
// it in the Host header, which is the value that gets signed.
func parseEndpoint(endpoint string) (scheme, host string, err error) {
	if endpoint == "" {
		return "", "", errors.New("objstore: endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", errors.New("objstore: endpoint is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", errors.New("objstore: endpoint must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return "", "", errors.New("objstore: endpoint has no host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", "", errors.New("objstore: endpoint must be scheme://host[:port] with no path, query or credentials")
	}
	host = strings.ToLower(u.Host)
	// Clients leave the default port out of the Host header.
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	return u.Scheme, host, nil
}

// String describes the store without the credentials.
func (s *S3) String() string {
	return fmt.Sprintf("objstore.S3{%s://%s/%s}", s.scheme, s.host, s.bucket)
}

// GoString keeps %#v from printing the credentials.
func (s *S3) GoString() string { return s.String() }

// Put uploads body as the object key.
func (s *S3) Put(ctx context.Context, key string, body []byte, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if contentType == "" {
		contentType = defaultContentType
	}
	header := http.Header{"Content-Type": {contentType}}
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(key), header, body)
	if err != nil {
		return fmt.Errorf("objstore: put %q: %w", key, err)
	}
	drainAndClose(resp.Body)
	return nil
}

// Get downloads the object key. The caller must close the returned reader.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	resp, err := s.do(ctx, http.MethodGet, s.objectURL(key), nil, nil)
	if err != nil {
		var respErr *ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound &&
			(respErr.Code == "" || respErr.Code == "NoSuchKey") {
			return nil, fmt.Errorf("objstore: get %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("objstore: get %q: %w", key, err)
	}
	return resp.Body, nil
}

// List returns the objects under prefix using ListObjectsV2, following
// continuation tokens until the listing is complete.
func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	var objects []Object
	token := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("objstore: list %q: more than %d pages", prefix, maxListPages)
		}
		result, err := s.listPage(ctx, prefix, token)
		if err != nil {
			return nil, fmt.Errorf("objstore: list %q: %w", prefix, err)
		}
		for _, c := range result.Contents {
			modified, err := time.Parse(time.RFC3339Nano, c.LastModified)
			if err != nil {
				return nil, fmt.Errorf("objstore: list %q: parse LastModified of %q: %w", prefix, c.Key, err)
			}
			objects = append(objects, Object{Key: c.Key, Size: c.Size, LastModified: modified.UTC()})
		}
		if !result.IsTruncated {
			break
		}
		if result.NextContinuationToken == "" || result.NextContinuationToken == token {
			return nil, fmt.Errorf("objstore: list %q: truncated response without a new continuation token", prefix)
		}
		token = result.NextContinuationToken
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

// listBucketResult is the body of a ListObjectsV2 response.
type listBucketResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *S3) listPage(ctx context.Context, prefix, token string) (*listBucketResult, error) {
	query := url.Values{"list-type": {"2"}}
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	if s.listPageSize > 0 {
		query.Set("max-keys", strconv.Itoa(s.listPageSize))
	}
	u := s.bucketURL()
	u.RawQuery = canonicalQuery(query)

	resp, err := s.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp.Body)

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxListBodyBytes {
		return nil, fmt.Errorf("response is larger than %d bytes", maxListBodyBytes)
	}
	var result listBucketResult
	if err := xml.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// Delete removes the object key. S3 reports success when the object is
// already gone.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, s.objectURL(key), nil, nil)
	if err != nil {
		return fmt.Errorf("objstore: delete %q: %w", key, err)
	}
	drainAndClose(resp.Body)
	return nil
}

// PresignGet returns a URL with query-string authentication that allows a
// plain GET of key until expires has passed.
func (s *S3) PresignGet(key string, expires time.Duration) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	seconds, err := presignSeconds(expires)
	if err != nil {
		return "", err
	}
	return s.signer.presign(http.MethodGet, s.objectURL(key), seconds, s.clock.Now()), nil
}

// bucketURL is the path-style URL of the bucket itself.
func (s *S3) bucketURL() *url.URL {
	return s.pathURL("/" + s.bucket)
}

// objectURL is the path-style URL of one object.
func (s *S3) objectURL(key string) *url.URL {
	return s.pathURL("/" + s.bucket + "/" + key)
}

// pathURL sets RawPath to the Signature Version 4 encoding so that the path
// sent on the wire is byte for byte the path that was signed.
func (s *S3) pathURL(path string) *url.URL {
	return &url.URL{Scheme: s.scheme, Host: s.host, Path: path, RawPath: uriEncode(path, false)}
}

// do signs and sends one request. On a 2xx status the caller owns the response
// body; any other status is turned into a *ResponseError.
func (s *S3) do(ctx context.Context, method string, u *url.URL, header http.Header, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	s.signer.signRequest(req, sha256Hex(body), s.clock.Now())

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer drainAndClose(resp.Body)
		return nil, parseResponseError(resp)
	}
	return resp, nil
}

// ResponseError is a non-2xx response from the S3 API.
type ResponseError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Code and Message come from the XML error document. Both are empty when
	// the response had no parsable body.
	Code    string
	Message string
}

func (e *ResponseError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("s3: %s: %s (HTTP %d)", e.Code, e.Message, e.StatusCode)
	case e.Code != "":
		return fmt.Sprintf("s3: %s (HTTP %d)", e.Code, e.StatusCode)
	default:
		return fmt.Sprintf("s3: HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	}
}

// parseResponseError reads the XML error document. Only Code and Message are
// kept: other fields can echo details of the signed request.
func parseResponseError(resp *http.Response) *ResponseError {
	respErr := &ResponseError{StatusCode: resp.StatusCode}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil || len(data) == 0 {
		return respErr
	}
	var doc struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return respErr
	}
	respErr.Code = strings.TrimSpace(doc.Code)
	respErr.Message = truncate(strings.TrimSpace(doc.Message), maxErrorMessageBytes)
	return respErr
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "..."
}

// drainAndClose reads what is left of a small response body so the connection
// can be reused.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxErrorBodyBytes))
	_ = body.Close()
}
