package domains

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const testBase = "dev.example.com"

func TestNormalizeHostnameAccepts(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"apex":                   {"example.org", "example.org"},
		"subdomain":              {"www.example.org", "www.example.org"},
		"uppercase":              {"WWW.Example.ORG", "www.example.org"},
		"surrounding space":      {"  www.example.org\n", "www.example.org"},
		"trailing dot":           {"www.example.org.", "www.example.org"},
		"hyphen and digits":      {"my-app2.example.org", "my-app2.example.org"},
		"numeric label":          {"123.example.org", "123.example.org"},
		"punycode":               {"xn--eckwd4c7c.xn--zckzah", "xn--eckwd4c7c.xn--zckzah"},
		"parent of the base":     {"example.com", "example.com"},
		"sibling of the base":    {"prod.example.com", "prod.example.com"},
		"base as a prefix only":  {"dev.example.com.example.org", "dev.example.com.example.org"},
		"base as a suffix only":  {"notdev.example.com", "notdev.example.com"},
		"63 character label":     {strings.Repeat("a", 63) + ".example.org", strings.Repeat("a", 63) + ".example.org"},
		"253 characters overall": {longHostname(253), longHostname(253)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeHostname(tc.in, testBase)
			if err != nil {
				t.Fatalf("NormalizeHostname(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeHostname(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeHostnameRejects(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"empty":                  {"", "hostname is empty"},
		"only space":             {"   ", "hostname is empty"},
		"only a dot":             {".", "hostname is empty"},
		"IPv4 address":           {"203.0.113.10", "IP address"},
		"IPv4 with trailing dot": {"203.0.113.10.", "IP address"},
		"IPv6 address":           {"2001:db8::1", "IP address"},
		"bracketed IPv6 address": {"[2001:db8::1]", "IP address"},
		"wildcard":               {"*.example.org", "wildcard"},
		"inner wildcard":         {"www.*.example.org", "wildcard"},
		"scheme":                 {"https://www.example.org", "hostname only"},
		"path":                   {"www.example.org/admin", "hostname only"},
		"port":                   {"www.example.org:8443", "hostname only"},
		"user info":              {"user@example.org", "hostname only"},
		"query":                  {"www.example.org?x=1", "hostname only"},
		"single label":           {"localhost", "fully qualified"},
		"empty label":            {"www..example.org", "empty label"},
		"leading dot":            {".example.org", "empty label"},
		"two trailing dots":      {"www.example.org..", "empty label"},
		"leading hyphen":         {"-www.example.org", "hyphen"},
		"trailing hyphen":        {"www-.example.org", "hyphen"},
		"underscore":             {"my_app.example.org", `invalid character '_'`},
		"inner space":            {"www example.org", `invalid character ' '`},
		"placeholder":            {"{http.request.host}.example.org", "invalid character"},
		"unicode":                {"日本語.example.org", "punycode"},
		"numeric last label":     {"www.example.123", "cannot be a number"},
		"short numeric form":     {"127.1", "cannot be a number"},
		"64 character label":     {strings.Repeat("a", 64) + ".example.org", "longer than 63"},
		"254 characters overall": {longHostname(254), "longer than 253"},
		"the base domain":        {"dev.example.com", "cannot be added as custom domains"},
		"under the base":         {"blog.dev.example.com", "cannot be added as custom domains"},
		"deep under the base":    {"a.b.dev.example.com", "cannot be added as custom domains"},
		"under the base, mixed":  {"Blog.DEV.example.com.", "cannot be added as custom domains"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeHostname(tc.in, testBase)
			if err == nil {
				t.Fatalf("NormalizeHostname(%q) = %q, want an error", tc.in, got)
			}
			if got != "" {
				t.Errorf("returned %q together with an error", got)
			}
			if !errors.Is(err, ErrInvalidHostname) {
				t.Errorf("error %q does not match ErrInvalidHostname", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
			wantReserved := strings.Contains(tc.want, "custom domains")
			if errors.Is(err, ErrReservedHostname) != wantReserved {
				t.Errorf("errors.Is(err, ErrReservedHostname) = %v, want %v", !wantReserved, wantReserved)
			}
		})
	}
}

func TestNormalizeHostnameBaseDomainForms(t *testing.T) {
	for _, base := range []string{"dev.example.com", "DEV.Example.com", "dev.example.com.", " dev.example.com "} {
		if _, err := NormalizeHostname("blog.dev.example.com", base); !errors.Is(err, ErrReservedHostname) {
			t.Errorf("base %q: error = %v, want ErrReservedHostname", base, err)
		}
	}
	// Without a base domain there is nothing to reserve.
	if got, err := NormalizeHostname("blog.dev.example.com", ""); err != nil || got != "blog.dev.example.com" {
		t.Errorf("empty base: got %q, %v", got, err)
	}
}

func TestInstructions(t *testing.T) {
	got := Instructions("www.example.org", "shop", "dev.example.com", "203.0.113.10")
	want := []Record{
		{Type: "CNAME", Name: "www.example.org", Value: "shop.dev.example.com"},
		{Type: "A", Name: "www.example.org", Value: "203.0.113.10"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Instructions = %+v, want %+v", got, want)
	}

	// Without a public IP there is no A record to suggest.
	got = Instructions("www.example.org", "shop", "dev.example.com", "")
	if !reflect.DeepEqual(got, want[:1]) {
		t.Errorf("Instructions without IP = %+v, want %+v", got, want[:1])
	}
}

// longHostname returns a valid hostname of exactly n characters.
func longHostname(n int) string {
	const suffix = ".example.org"
	var b strings.Builder
	for b.Len()+len(suffix) < n {
		remaining := n - len(suffix) - b.Len()
		if b.Len() > 0 {
			b.WriteByte('.')
			remaining--
		}
		size := min(remaining, 50)
		if remaining-size == 1 {
			// Never leave room for only a dot.
			size--
		}
		b.WriteString(strings.Repeat("a", size))
	}
	return b.String() + suffix
}
