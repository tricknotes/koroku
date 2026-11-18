package objstore

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The vectors in this file are the worked examples from the Amazon S3 API
// reference ("Authenticating Requests: Using the Authorization Header" and
// "Using Query Parameters"). The credentials are the public documentation
// placeholders, not real keys.
const (
	docAccessKeyID     = "AKIAIOSFODNN7EXAMPLE"
	docSecretAccessKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	docHost            = "examplebucket.s3.amazonaws.com"
	emptyPayloadHash   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var (
	docSigner = signer{accessKeyID: docAccessKeyID, secretAccessKey: docSecretAccessKey, region: "us-east-1"}
	docTime   = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
)

func TestSignRequestAWSDocumentationExamples(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		target        string
		header        http.Header
		body          string
		payloadHash   string
		signedHeaders string
		signature     string
	}{
		{
			name:          "GET Object",
			method:        http.MethodGet,
			target:        "/test.txt",
			header:        http.Header{"Range": {"bytes=0-9"}},
			payloadHash:   emptyPayloadHash,
			signedHeaders: "host;range;x-amz-content-sha256;x-amz-date",
			signature:     "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "PUT Object",
			method: http.MethodPut,
			target: "/test$file.text",
			header: http.Header{
				"Date":                {"Fri, 24 May 2013 00:00:00 GMT"},
				"X-Amz-Storage-Class": {"REDUCED_REDUNDANCY"},
			},
			body:          "Welcome to Amazon S3.",
			payloadHash:   "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			signedHeaders: "date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			signature:     "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name:          "GET Bucket Lifecycle",
			method:        http.MethodGet,
			target:        "/?lifecycle",
			payloadHash:   emptyPayloadHash,
			signedHeaders: "host;x-amz-content-sha256;x-amz-date",
			signature:     "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name:          "GET Bucket (List Objects)",
			method:        http.MethodGet,
			target:        "/?max-keys=2&prefix=J",
			payloadHash:   emptyPayloadHash,
			signedHeaders: "host;x-amz-content-sha256;x-amz-date",
			signature:     "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sha256Hex([]byte(tt.body)); got != tt.payloadHash {
				t.Fatalf("payload hash = %s, want %s", got, tt.payloadHash)
			}
			req, err := http.NewRequest(tt.method, "https://"+docHost+tt.target, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			for name, values := range tt.header {
				req.Header[name] = values
			}
			docSigner.signRequest(req, tt.payloadHash, docTime)

			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=" + tt.signedHeaders + ",Signature=" + tt.signature
			if got := req.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization =\n  %s\nwant\n  %s", got, want)
			}
			if got := req.Header.Get("X-Amz-Date"); got != "20130524T000000Z" {
				t.Errorf("X-Amz-Date = %q", got)
			}
			if got := req.Header.Get("X-Amz-Content-Sha256"); got != tt.payloadHash {
				t.Errorf("X-Amz-Content-Sha256 = %q", got)
			}
		})
	}
}

func TestPresignAWSDocumentationExample(t *testing.T) {
	u := &url.URL{Scheme: "https", Host: docHost, Path: "/test.txt"}
	got := docSigner.presign(http.MethodGet, u, 86400, docTime)
	want := "https://examplebucket.s3.amazonaws.com/test.txt" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z" +
		"&X-Amz-Expires=86400" +
		"&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Errorf("presign =\n  %s\nwant\n  %s", got, want)
	}
}

func TestSignRequestUsesUTC(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	req, err := http.NewRequest(http.MethodGet, "https://"+docHost+"/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	docSigner.signRequest(req, emptyPayloadHash, docTime.In(jst))
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41") {
		t.Errorf("signature changed with the time zone: %s", got)
	}
}

func TestSignRequestReplacesStaleAuthorization(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://"+docHost+"/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	req.Header.Set("Authorization", "stale")
	docSigner.signRequest(req, emptyPayloadHash, docTime)
	got := req.Header.Get("Authorization")
	if strings.Contains(got, "authorization") || !strings.HasSuffix(got, "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41") {
		t.Errorf("Authorization = %s", got)
	}
}

func TestURIEncode(t *testing.T) {
	tests := []struct {
		in        string
		path      string
		component string
	}{
		{"", "", ""},
		{"AZaz09-._~", "AZaz09-._~", "AZaz09-._~"},
		{"logs/app/web.log.gz", "logs/app/web.log.gz", "logs%2Fapp%2Fweb.log.gz"},
		{"a b", "a%20b", "a%20b"},
		{"a+b", "a%2Bb", "a%2Bb"},
		{"a=b&c", "a%3Db%26c", "a%3Db%26c"},
		{"100%", "100%25", "100%25"},
		{"test$file.text", "test%24file.text", "test%24file.text"},
		{"a*b(c)!'", "a%2Ab%28c%29%21%27", "a%2Ab%28c%29%21%27"},
		{"日本語", "%E6%97%A5%E6%9C%AC%E8%AA%9E", "%E6%97%A5%E6%9C%AC%E8%AA%9E"},
		{"é/ü", "%C3%A9/%C3%BC", "%C3%A9%2F%C3%BC"},
	}
	for _, tt := range tests {
		if got := uriEncode(tt.in, false); got != tt.path {
			t.Errorf("uriEncode(%q, false) = %q, want %q", tt.in, got, tt.path)
		}
		if got := uriEncode(tt.in, true); got != tt.component {
			t.Errorf("uriEncode(%q, true) = %q, want %q", tt.in, got, tt.component)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	tests := []struct {
		name  string
		query url.Values
		want  string
	}{
		{"empty", url.Values{}, ""},
		{"value-less parameter", url.Values{"lifecycle": {""}}, "lifecycle="},
		{"sorted by name", url.Values{"prefix": {"J"}, "max-keys": {"2"}}, "max-keys=2&prefix=J"},
		{
			"uppercase sorts before lowercase",
			url.Values{"list-type": {"2"}, "X-Amz-Date": {"20130524T000000Z"}},
			"X-Amz-Date=20130524T000000Z&list-type=2",
		},
		{
			"values are encoded",
			url.Values{"prefix": {"logs/my app/"}, "continuation-token": {"a+b/c=="}},
			"continuation-token=a%2Bb%2Fc%3D%3D&prefix=logs%2Fmy%20app%2F",
		},
		{"repeated name sorts by value", url.Values{"k": {"b", "a"}}, "k=a&k=b"},
	}
	for _, tt := range tests {
		if got := canonicalQuery(tt.query); got != tt.want {
			t.Errorf("%s: canonicalQuery = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCanonicalizeHeaders(t *testing.T) {
	canonical, signed := canonicalizeHeaders(map[string]string{
		"x-amz-date":   "20130524T000000Z",
		"host":         "example.test",
		"content-type": "  text/plain;   charset=utf-8 ",
	})
	wantCanonical := "content-type:text/plain; charset=utf-8\nhost:example.test\nx-amz-date:20130524T000000Z\n"
	if canonical != wantCanonical {
		t.Errorf("canonical headers = %q, want %q", canonical, wantCanonical)
	}
	if signed != "content-type;host;x-amz-date" {
		t.Errorf("signed headers = %q", signed)
	}
}
