package objstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

const (
	testBucket    = "test-bucket"
	testRegion    = "auto"
	testAccessKey = "test-access-key-id"
	testSecretKey = "test-secret-access-key-value"
)

var testTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// wireRequest is one request as the fake server received it.
type wireRequest struct {
	Method string
	// RawPath and RawQuery are the bytes sent on the wire, before any decoding.
	RawPath  string
	RawQuery string
	// Path is the decoded path.
	Path   string
	Query  url.Values
	Host   string
	Header http.Header
	Body   []byte
}

// fakeS3 is an httptest server that verifies the signature of every request
// with an independent implementation before handing it to the handler.
type fakeS3 struct {
	t       *testing.T
	server  *httptest.Server
	handler func(w http.ResponseWriter, r wireRequest)

	mu       sync.Mutex
	requests []wireRequest
}

func newFakeS3(t *testing.T, handler func(w http.ResponseWriter, r wireRequest)) (*S3, *fakeS3) {
	t.Helper()
	f := &fakeS3{t: t, handler: handler}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)

	store, err := NewS3(S3Config{
		Endpoint:        f.server.URL,
		Region:          testRegion,
		Bucket:          testBucket,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
		HTTP:            f.server.Client(),
		Clock:           clock.NewFake(testTime),
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return store, f
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read request body: %v", err)
	}
	rawPath, rawQuery, _ := strings.Cut(r.RequestURI, "?")
	req := wireRequest{
		Method:   r.Method,
		RawPath:  rawPath,
		RawQuery: rawQuery,
		Path:     r.URL.Path,
		Query:    r.URL.Query(),
		Host:     r.Host,
		Header:   r.Header.Clone(),
		Body:     body,
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if req.Query.Get("X-Amz-Signature") != "" {
		verifyPresigned(f.t, req)
	} else {
		verifyHeaderAuth(f.t, req)
	}
	f.handler(w, req)
}

func (f *fakeS3) recorded() []wireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wireRequest(nil), f.requests...)
}

func (f *fakeS3) only() wireRequest {
	f.t.Helper()
	requests := f.recorded()
	if len(requests) != 1 {
		f.t.Fatalf("got %d requests, want 1", len(requests))
	}
	return requests[0]
}

// verifyHeaderAuth recomputes the Signature Version 4 signature from the raw
// request line and headers. It deliberately shares no code with the signer:
// the path and query are used exactly as received, so a mismatch between what
// was signed and what was sent fails here.
func verifyHeaderAuth(t *testing.T, r wireRequest) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !ok {
		t.Errorf("Authorization does not use AWS4-HMAC-SHA256: %q", auth)
		return
	}
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		fields[name] = value
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if len(amzDate) != len("20060102T150405Z") {
		t.Errorf("X-Amz-Date = %q", amzDate)
		return
	}
	scope := amzDate[:8] + "/" + testRegion + "/s3/aws4_request"
	if want := testAccessKey + "/" + scope; fields["Credential"] != want {
		t.Errorf("Credential = %q, want %q", fields["Credential"], want)
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if sum := sha256.Sum256(r.Body); payloadHash != hex.EncodeToString(sum[:]) {
		t.Errorf("X-Amz-Content-Sha256 = %q does not match the body", payloadHash)
	}

	signedHeaders := strings.Split(fields["SignedHeaders"], ";")
	if !sort.StringsAreSorted(signedHeaders) {
		t.Errorf("SignedHeaders are not sorted: %q", fields["SignedHeaders"])
	}
	for _, required := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
		if !contains(signedHeaders, required) {
			t.Errorf("SignedHeaders %q is missing %s", fields["SignedHeaders"], required)
		}
	}
	var canonicalHeaders strings.Builder
	for _, name := range signedHeaders {
		value := strings.Join(r.Header.Values(name), ",")
		if name == "host" {
			value = r.Host
		}
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}

	assertSortedQuery(t, r.RawQuery)
	canonical := strings.Join([]string{
		r.Method, r.RawPath, r.RawQuery, canonicalHeaders.String(), fields["SignedHeaders"], payloadHash,
	}, "\n")
	if want := referenceSignature(canonical, amzDate, scope); fields["Signature"] != want {
		t.Errorf("signature mismatch for %s %s?%s:\n got %s\nwant %s", r.Method, r.RawPath, r.RawQuery, fields["Signature"], want)
	}
}

// verifyPresigned does the same for query-string authentication.
func verifyPresigned(t *testing.T, r wireRequest) {
	t.Helper()
	if r.Header.Get("Authorization") != "" {
		t.Errorf("presigned request carries an Authorization header")
	}
	unsigned, signature, ok := strings.Cut(r.RawQuery, "&X-Amz-Signature=")
	if !ok {
		t.Errorf("X-Amz-Signature is not the last query parameter: %q", r.RawQuery)
		return
	}
	assertSortedQuery(t, unsigned)
	amzDate := r.Query.Get("X-Amz-Date")
	scope := amzDate[:8] + "/" + testRegion + "/s3/aws4_request"
	canonical := strings.Join([]string{
		r.Method, r.RawPath, unsigned, "host:" + r.Host + "\n", "host", "UNSIGNED-PAYLOAD",
	}, "\n")
	if want := referenceSignature(canonical, amzDate, scope); signature != want {
		t.Errorf("presigned signature mismatch:\n got %s\nwant %s", signature, want)
	}
}

func referenceSignature(canonicalRequest, amzDate, scope string) string {
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(hash[:])
	key := []byte("AWS4" + testSecretKey)
	for _, part := range strings.Split(scope, "/") {
		key = mac(key, part)
	}
	return hex.EncodeToString(mac(key, stringToSign))
}

func assertSortedQuery(t *testing.T, rawQuery string) {
	t.Helper()
	if rawQuery == "" {
		return
	}
	pairs := strings.Split(rawQuery, "&")
	if !sort.StringsAreSorted(pairs) {
		t.Errorf("query string is not sorted: %q", rawQuery)
	}
	if strings.Contains(rawQuery, "+") {
		t.Errorf("query string uses + instead of %%20 or %%2B: %q", rawQuery)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func xmlError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<Error><Code>%s</Code><Message>%s</Message><RequestId>REQ123</RequestId>`+
		`<StringToSign>must-not-surface</StringToSign></Error>`, code, message)
}

func TestS3Put(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		w.Header().Set("ETag", `"abc"`)
	})
	body := []byte("\x1f\x8bgzip bytes")
	key := "logs/blog/web/2026/01/02/03.log.gz"
	if err := store.Put(context.Background(), key, body, "application/gzip"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	r := fake.only()
	if r.Method != http.MethodPut {
		t.Errorf("method = %s", r.Method)
	}
	if want := "/" + testBucket + "/" + key; r.RawPath != want {
		t.Errorf("path = %q, want %q", r.RawPath, want)
	}
	if r.RawQuery != "" {
		t.Errorf("query = %q, want none", r.RawQuery)
	}
	if string(r.Body) != string(body) {
		t.Errorf("body = %q", r.Body)
	}
	if got := r.Header.Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.Header.Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length = %q", got)
	}
	if got := r.Header.Get("X-Amz-Date"); got != "20260102T030405Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != sha256Hex(body) {
		t.Errorf("X-Amz-Content-Sha256 = %q", got)
	}
	wantPrefix := "AWS4-HMAC-SHA256 Credential=" + testAccessKey + "/20260102/auto/s3/aws4_request," +
		"SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date,Signature="
	if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("Authorization = %q, want prefix %q", got, wantPrefix)
	}
	if want := strings.TrimPrefix(fake.server.URL, "http://"); r.Host != want {
		t.Errorf("Host = %q, want %q", r.Host, want)
	}
}

func TestS3PutDefaultsContentTypeAndAllowsEmptyBody(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {})
	if err := store.Put(context.Background(), "empty", nil, ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r := fake.only()
	if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != emptyPayloadHash {
		t.Errorf("X-Amz-Content-Sha256 = %q", got)
	}
	if len(r.Body) != 0 {
		t.Errorf("body = %q", r.Body)
	}
}

func TestS3Get(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		w.Header().Set("Content-Type", "application/gzip")
		io.WriteString(w, "object content")
	})
	rc, err := store.Get(context.Background(), "logs/blog/web/2026/01/02/03.log.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "object content" {
		t.Errorf("content = %q", data)
	}

	r := fake.only()
	if r.Method != http.MethodGet {
		t.Errorf("method = %s", r.Method)
	}
	if want := "/" + testBucket + "/logs/blog/web/2026/01/02/03.log.gz"; r.RawPath != want {
		t.Errorf("path = %q, want %q", r.RawPath, want)
	}
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != emptyPayloadHash {
		t.Errorf("X-Amz-Content-Sha256 = %q", got)
	}
	if got := r.Header.Get("Authorization"); !strings.Contains(got, "SignedHeaders=host;x-amz-content-sha256;x-amz-date,") {
		t.Errorf("Authorization = %q", got)
	}
}

func TestS3GetNotFound(t *testing.T) {
	tests := []struct {
		name         string
		respond      func(w http.ResponseWriter)
		wantNotFound bool
	}{
		{
			name:         "NoSuchKey",
			respond:      func(w http.ResponseWriter) { xmlError(w, 404, "NoSuchKey", "The specified key does not exist.") },
			wantNotFound: true,
		},
		{
			name:         "404 without a body",
			respond:      func(w http.ResponseWriter) { w.WriteHeader(404) },
			wantNotFound: true,
		},
		{
			name:         "404 with a non-XML body",
			respond:      func(w http.ResponseWriter) { http.Error(w, "not found", 404) },
			wantNotFound: true,
		},
		{
			name:         "NoSuchBucket is a configuration error",
			respond:      func(w http.ResponseWriter) { xmlError(w, 404, "NoSuchBucket", "The specified bucket does not exist.") },
			wantNotFound: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) { tt.respond(w) })
			rc, err := store.Get(context.Background(), "missing/key")
			if err == nil {
				rc.Close()
				t.Fatal("Get succeeded")
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v (err: %v)", got, tt.wantNotFound, err)
			}
			if !strings.Contains(err.Error(), `"missing/key"`) {
				t.Errorf("error %q does not name the key", err)
			}
		})
	}
}

func TestS3ListFollowsContinuationTokens(t *testing.T) {
	const prefix = "logs/blog/"
	pages := map[string]string{
		"": `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>test-bucket</Name><Prefix>logs/blog/</Prefix><KeyCount>2</KeyCount><MaxKeys>2</MaxKeys>
  <IsTruncated>true</IsTruncated>
  <NextContinuationToken>tok/en+1==</NextContinuationToken>
  <Contents><Key>logs/blog/web/2026/01/02/08.log.gz</Key><LastModified>2026-01-02T08:10:03.000Z</LastModified><ETag>"a"</ETag><Size>300</Size><StorageClass>STANDARD</StorageClass></Contents>
  <Contents><Key>logs/blog/web/2026/01/02/00.log.gz</Key><LastModified>2026-01-02T00:10:01Z</LastModified><Size>100</Size></Contents>
</ListBucketResult>`,
		"tok/en+1==": `<ListBucketResult>
  <IsTruncated>true</IsTruncated>
  <NextContinuationToken>token 2</NextContinuationToken>
  <Contents><Key>logs/blog/db/2026/01/02/04.log.gz</Key><LastModified>2026-01-02T13:10:02.500+09:00</LastModified><Size>0</Size></Contents>
</ListBucketResult>`,
		"token 2": `<ListBucketResult>
  <IsTruncated>false</IsTruncated>
  <Contents><Key>logs/blog/web/2026/01/02/04.log.gz</Key><LastModified>2026-01-02T04:10:02Z</LastModified><Size>200</Size></Contents>
</ListBucketResult>`,
	}
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		page, ok := pages[r.Query.Get("continuation-token")]
		if !ok {
			t.Errorf("unexpected continuation token %q", r.Query.Get("continuation-token"))
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, page)
	})

	got, err := store.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Object{
		{Key: "logs/blog/db/2026/01/02/04.log.gz", Size: 0, LastModified: time.Date(2026, 1, 2, 4, 10, 2, 500_000_000, time.UTC)},
		{Key: "logs/blog/web/2026/01/02/00.log.gz", Size: 100, LastModified: time.Date(2026, 1, 2, 0, 10, 1, 0, time.UTC)},
		{Key: "logs/blog/web/2026/01/02/04.log.gz", Size: 200, LastModified: time.Date(2026, 1, 2, 4, 10, 2, 0, time.UTC)},
		{Key: "logs/blog/web/2026/01/02/08.log.gz", Size: 300, LastModified: time.Date(2026, 1, 2, 8, 10, 3, 0, time.UTC)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d objects, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Key != want[i].Key || got[i].Size != want[i].Size || !got[i].LastModified.Equal(want[i].LastModified) {
			t.Errorf("object %d = %+v, want %+v", i, got[i], want[i])
		}
		if got[i].LastModified.Location() != time.UTC {
			t.Errorf("object %d LastModified is not UTC", i)
		}
	}

	requests := fake.recorded()
	wantQueries := []string{
		"list-type=2&prefix=logs%2Fblog%2F",
		"continuation-token=tok%2Fen%2B1%3D%3D&list-type=2&prefix=logs%2Fblog%2F",
		"continuation-token=token%202&list-type=2&prefix=logs%2Fblog%2F",
	}
	if len(requests) != len(wantQueries) {
		t.Fatalf("got %d requests, want %d", len(requests), len(wantQueries))
	}
	for i, r := range requests {
		if r.Method != http.MethodGet || r.RawPath != "/"+testBucket {
			t.Errorf("request %d = %s %s", i, r.Method, r.RawPath)
		}
		if r.RawQuery != wantQueries[i] {
			t.Errorf("request %d query = %q, want %q", i, r.RawQuery, wantQueries[i])
		}
	}
}

func TestS3ListEmptyPrefixAndEmptyResult(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><KeyCount>0</KeyCount></ListBucketResult>`)
	})
	got, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
	if r := fake.only(); r.RawQuery != "list-type=2" {
		t.Errorf("query = %q", r.RawQuery)
	}
}

func TestS3ListPageSize(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	})
	store.listPageSize = 2
	if _, err := store.List(context.Background(), "logs/"); err != nil {
		t.Fatalf("List: %v", err)
	}
	if r := fake.only(); r.RawQuery != "list-type=2&max-keys=2&prefix=logs%2F" {
		t.Errorf("query = %q", r.RawQuery)
	}
}

func TestS3ListRejectsBrokenResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"truncated without a token",
			`<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`,
			"continuation token",
		},
		{
			"bad timestamp",
			`<ListBucketResult><Contents><Key>a</Key><LastModified>yesterday</LastModified><Size>1</Size></Contents></ListBucketResult>`,
			"LastModified",
		},
		{"not XML", `{"oops": true}`, "decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) { io.WriteString(w, tt.body) })
			_, err := store.List(context.Background(), "logs/")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestS3ListStopsWhenTokenRepeats(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>same</NextContinuationToken></ListBucketResult>`)
	})
	if _, err := store.List(context.Background(), ""); err == nil {
		t.Fatal("List succeeded")
	}
	if n := len(fake.recorded()); n != 2 {
		t.Errorf("made %d requests, want 2", n)
	}
}

func TestS3Delete(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := store.Delete(context.Background(), "logs/blog/web/2026/01/02/03.log.gz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	r := fake.only()
	if r.Method != http.MethodDelete {
		t.Errorf("method = %s", r.Method)
	}
	if want := "/" + testBucket + "/logs/blog/web/2026/01/02/03.log.gz"; r.RawPath != want {
		t.Errorf("path = %q, want %q", r.RawPath, want)
	}
	if len(r.Body) != 0 {
		t.Errorf("body = %q", r.Body)
	}
}

func TestS3ErrorResponses(t *testing.T) {
	store, _ := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		switch r.Method {
		case http.MethodPut:
			xmlError(w, 403, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		case http.MethodDelete:
			xmlError(w, 403, "AccessDenied", strings.Repeat("x", 5000))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "upstream unavailable")
		}
	})
	ctx := context.Background()

	check := func(t *testing.T, err error, status int, code, message string) *ResponseError {
		t.Helper()
		var respErr *ResponseError
		if !errors.As(err, &respErr) {
			t.Fatalf("err = %v, want a *ResponseError", err)
		}
		if respErr.StatusCode != status || respErr.Code != code {
			t.Errorf("got %d %q, want %d %q", respErr.StatusCode, respErr.Code, status, code)
		}
		if message != "" && respErr.Message != message {
			t.Errorf("Message = %q, want %q", respErr.Message, message)
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("err %v must not match ErrNotFound", err)
		}
		for _, leak := range []string{testSecretKey, "Signature=", "AWS4-HMAC-SHA256", "must-not-surface"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error leaks %q: %v", leak, err)
			}
		}
		return respErr
	}

	t.Run("put", func(t *testing.T) {
		err := store.Put(ctx, "a/b", []byte("x"), "text/plain")
		check(t, err, 403, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		want := `objstore: put "a/b": s3: SignatureDoesNotMatch: The request signature we calculated does not match the signature you provided. (HTTP 403)`
		if err.Error() != want {
			t.Errorf("error = %q\n want %q", err, want)
		}
	})
	t.Run("delete with an oversized message", func(t *testing.T) {
		respErr := check(t, store.Delete(ctx, "a/b"), 403, "AccessDenied", "")
		if len(respErr.Message) > maxErrorMessageBytes+3 {
			t.Errorf("message is %d bytes long", len(respErr.Message))
		}
	})
	t.Run("get without an XML body", func(t *testing.T) {
		_, err := store.Get(ctx, "a/b")
		check(t, err, 503, "", "")
		if want := `objstore: get "a/b": s3: HTTP 503 Service Unavailable`; err.Error() != want {
			t.Errorf("error = %q, want %q", err, want)
		}
	})
	t.Run("list", func(t *testing.T) {
		_, err := store.List(ctx, "a/")
		check(t, err, 503, "", "")
		if !strings.HasPrefix(err.Error(), `objstore: list "a/": `) {
			t.Errorf("error = %q", err)
		}
	})
}

func TestS3KeyEncoding(t *testing.T) {
	tests := []struct {
		key     string
		rawPath string
	}{
		{"plain/key-1_2.3~4", "plain/key-1_2.3~4"},
		{"with space/a b.log", "with%20space/a%20b.log"},
		{"plus/a+b", "plus/a%2Bb"},
		{"unicode/日本語.log", "unicode/%E6%97%A5%E6%9C%AC%E8%AA%9E.log"},
		{"percent/100%", "percent/100%25"},
		{"reserved/a?b#c&d=e", "reserved/a%3Fb%23c%26d%3De"},
		{"sub-delims/$!'()*,;:@", "sub-delims/%24%21%27%28%29%2A%2C%3B%3A%40"},
		{"dots/a..b/.hidden", "dots/a..b/.hidden"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
				if r.Query.Get("X-Amz-Signature") != "" {
					io.WriteString(w, "presigned ok")
				}
			})
			ctx := context.Background()
			if err := store.Put(ctx, tt.key, []byte("x"), ""); err != nil {
				t.Fatalf("Put: %v", err)
			}
			rc, err := store.Get(ctx, tt.key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			rc.Close()
			if err := store.Delete(ctx, tt.key); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			presigned, err := store.PresignGet(tt.key, time.Minute)
			if err != nil {
				t.Fatalf("PresignGet: %v", err)
			}
			resp, err := fake.server.Client().Get(presigned)
			if err != nil {
				t.Fatalf("fetch presigned URL: %v", err)
			}
			resp.Body.Close()

			requests := fake.recorded()
			if len(requests) != 4 {
				t.Fatalf("got %d requests, want 4", len(requests))
			}
			for _, r := range requests {
				if want := "/" + testBucket + "/" + tt.rawPath; r.RawPath != want {
					t.Errorf("%s raw path = %q, want %q", r.Method, r.RawPath, want)
				}
				if want := "/" + testBucket + "/" + tt.key; r.Path != want {
					t.Errorf("%s decoded path = %q, want %q", r.Method, r.Path, want)
				}
			}
		})
	}
}

func TestS3PresignGet(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {
		io.WriteString(w, "downloaded")
	})
	const key = "logs/blog/web/2026/01/02/03.log.gz"
	got, err := store.PresignGet(key, 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if strings.Contains(got, testSecretKey) {
		t.Fatalf("URL contains the secret key")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if origin := u.Scheme + "://" + u.Host; origin != fake.server.URL {
		t.Errorf("origin = %q, want %q", origin, fake.server.URL)
	}
	if want := "/" + testBucket + "/" + key; u.Path != want {
		t.Errorf("path = %q, want %q", u.Path, want)
	}
	query := u.Query()
	wantParams := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    testAccessKey + "/20260102/auto/s3/aws4_request",
		"X-Amz-Date":          "20260102T030405Z",
		"X-Amz-Expires":       "900",
		"X-Amz-SignedHeaders": "host",
	}
	for name, want := range wantParams {
		if query.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, query.Get(name), want)
		}
	}
	if sig := query.Get("X-Amz-Signature"); len(sig) != 64 || strings.ToLower(sig) != sig {
		t.Errorf("X-Amz-Signature = %q, want 64 lowercase hex digits", sig)
	}
	if len(query) != len(wantParams)+1 {
		t.Errorf("unexpected query parameters: %v", query)
	}

	again, err := store.PresignGet(key, 15*time.Minute)
	if err != nil || again != got {
		t.Errorf("PresignGet is not deterministic for a fixed clock: %q, %v", again, err)
	}
	if n := len(fake.recorded()); n != 0 {
		t.Errorf("PresignGet made %d requests", n)
	}

	// A plain client with no credentials can use the URL; the fake server
	// verifies the query-string signature.
	resp, err := fake.server.Client().Get(got)
	if err != nil {
		t.Fatalf("fetch presigned URL: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(data) != "downloaded" {
		t.Errorf("presigned fetch = %d %q", resp.StatusCode, data)
	}
	if r := fake.only(); r.Method != http.MethodGet {
		t.Errorf("method = %s", r.Method)
	}
}

func TestS3PresignGetExpiryBounds(t *testing.T) {
	store, _ := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {})
	tests := []struct {
		expires time.Duration
		seconds string // empty when the duration must be rejected
	}{
		{-time.Minute, ""},
		{0, ""},
		{999 * time.Millisecond, ""},
		{time.Second, "1"},
		{1500 * time.Millisecond, "1"},
		{time.Hour, "3600"},
		{7 * 24 * time.Hour, "604800"},
		{7*24*time.Hour + time.Second, ""},
		{30 * 24 * time.Hour, ""},
	}
	for _, tt := range tests {
		got, err := store.PresignGet("a/b", tt.expires)
		if tt.seconds == "" {
			if !errors.Is(err, ErrInvalidExpiry) {
				t.Errorf("PresignGet(%s) = %q, %v; want ErrInvalidExpiry", tt.expires, got, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("PresignGet(%s): %v", tt.expires, err)
			continue
		}
		u, _ := url.Parse(got)
		if u.Query().Get("X-Amz-Expires") != tt.seconds {
			t.Errorf("PresignGet(%s): X-Amz-Expires = %q, want %q", tt.expires, u.Query().Get("X-Amz-Expires"), tt.seconds)
		}
	}
}

func TestS3RejectsInvalidKeysWithoutARequest(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {})
	ctx := context.Background()
	for _, key := range []string{"", "/leading", "a/../b", "..", "a/./b", "a//b", "trailing/", "ctl\x00", "new\nline"} {
		if err := store.Put(ctx, key, []byte("x"), ""); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := store.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Get(%q) = %v, want ErrInvalidKey", key, err)
		}
		if err := store.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := store.PresignGet(key, time.Minute); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("PresignGet(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	for _, prefix := range []string{"/leading", "a/../b", "../", "a//b"} {
		if _, err := store.List(ctx, prefix); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("List(%q) = %v, want ErrInvalidKey", prefix, err)
		}
	}
	if n := len(fake.recorded()); n != 0 {
		t.Errorf("invalid keys caused %d requests", n)
	}
}

func TestS3HonorsContext(t *testing.T) {
	store, _ := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Put(ctx, "a", nil, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v, want context.Canceled", err)
	}
	if _, err := store.Get(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
	if _, err := store.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("List = %v, want context.Canceled", err)
	}
	if err := store.Delete(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v, want context.Canceled", err)
	}
}

func TestS3TransportErrorDoesNotLeakCredentials(t *testing.T) {
	store, fake := newFakeS3(t, func(w http.ResponseWriter, r wireRequest) {})
	fake.server.Close()
	err := store.Put(context.Background(), "a/b", []byte("x"), "")
	if err == nil {
		t.Fatal("Put succeeded against a closed server")
	}
	for _, leak := range []string{testSecretKey, "Signature=", "AWS4-HMAC-SHA256"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

func TestNewS3(t *testing.T) {
	valid := S3Config{
		Endpoint:        "https://s3.example.test",
		Region:          "auto",
		Bucket:          "my-bucket",
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	}

	t.Run("defaults", func(t *testing.T) {
		store, err := NewS3(valid)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		if store.http == nil || store.http.Timeout != defaultHTTPTimeout {
			t.Errorf("default HTTP client has no timeout: %+v", store.http)
		}
		if _, ok := store.clock.(clock.System); !ok {
			t.Errorf("default clock = %T", store.clock)
		}
	})

	t.Run("endpoint forms", func(t *testing.T) {
		tests := []struct {
			endpoint string
			wantURL  string
		}{
			{"https://s3.example.test", "https://s3.example.test/my-bucket/k"},
			{"https://s3.example.test/", "https://s3.example.test/my-bucket/k"},
			{"https://S3.Example.Test:443", "https://s3.example.test/my-bucket/k"},
			{"http://s3.example.test:80", "http://s3.example.test/my-bucket/k"},
			{"https://s3.example.test:8443", "https://s3.example.test:8443/my-bucket/k"},
			{"http://127.0.0.1:9000", "http://127.0.0.1:9000/my-bucket/k"},
		}
		for _, tt := range tests {
			cfg := valid
			cfg.Endpoint = tt.endpoint
			store, err := NewS3(cfg)
			if err != nil {
				t.Errorf("NewS3(%q): %v", tt.endpoint, err)
				continue
			}
			if got := store.objectURL("k").String(); got != tt.wantURL {
				t.Errorf("endpoint %q: object URL = %q, want %q", tt.endpoint, got, tt.wantURL)
			}
		}
	})

	t.Run("rejects", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*S3Config)
		}{
			{"empty endpoint", func(c *S3Config) { c.Endpoint = "" }},
			{"endpoint without scheme", func(c *S3Config) { c.Endpoint = "s3.example.test" }},
			{"endpoint with other scheme", func(c *S3Config) { c.Endpoint = "ftp://s3.example.test" }},
			{"endpoint with path", func(c *S3Config) { c.Endpoint = "https://s3.example.test/my-bucket" }},
			{"endpoint with query", func(c *S3Config) { c.Endpoint = "https://s3.example.test?x=1" }},
			{"endpoint with credentials", func(c *S3Config) { c.Endpoint = "https://user:" + testSecretKey + "@s3.example.test" }},
			{"endpoint without host", func(c *S3Config) { c.Endpoint = "https://" }},
			{"unparsable endpoint", func(c *S3Config) { c.Endpoint = "https://s3.example.test/%zz" + testSecretKey }},
			{"empty region", func(c *S3Config) { c.Region = "" }},
			{"empty bucket", func(c *S3Config) { c.Bucket = "" }},
			{"bucket with slash", func(c *S3Config) { c.Bucket = "my/bucket" }},
			{"bucket with uppercase", func(c *S3Config) { c.Bucket = "MyBucket" }},
			{"empty access key", func(c *S3Config) { c.AccessKeyID = "" }},
			{"empty secret key", func(c *S3Config) { c.SecretAccessKey = "" }},
		}
		for _, tt := range tests {
			cfg := valid
			tt.mutate(&cfg)
			store, err := NewS3(cfg)
			if err == nil {
				t.Errorf("%s: NewS3 succeeded: %v", tt.name, store)
				continue
			}
			if strings.Contains(err.Error(), testSecretKey) {
				t.Errorf("%s: error leaks the secret key: %v", tt.name, err)
			}
		}
	})
}

func TestS3FormattingHidesCredentials(t *testing.T) {
	cfg := S3Config{
		Endpoint:        "https://s3.example.test",
		Region:          "auto",
		Bucket:          "my-bucket",
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	}
	store, err := NewS3(cfg)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{cfg, &cfg, store} {
			if out := fmt.Sprintf(format, value); strings.Contains(out, testSecretKey) {
				t.Errorf("Sprintf(%q, %T) leaks the secret key: %s", format, value, out)
			}
		}
	}
	if got, want := store.String(), "objstore.S3{https://s3.example.test/my-bucket}"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
