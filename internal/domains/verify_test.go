package domains

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

var _ Resolver = (*net.Resolver)(nil)

var publicIP = net.ParseIP("203.0.113.10")

// fakeResolver answers from a fixed result and records what it was asked.
type fakeResolver struct {
	addrs []string
	err   error
	// block makes the lookup wait for the context to end, like a resolver
	// that never answers.
	block bool

	asked       []string
	hadDeadline bool
}

func (f *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	f.asked = append(f.asked, host)
	_, f.hadDeadline = ctx.Deadline()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([]net.IPAddr, len(f.addrs))
	for i, a := range f.addrs {
		out[i] = net.IPAddr{IP: net.ParseIP(a)}
	}
	return out, nil
}

func TestVerify(t *testing.T) {
	const resolverAddr = "192.0.2.53:53"
	cases := map[string]struct {
		resolver fakeResolver
		// wantReason is nil when verification must succeed.
		wantReason error
		wantMsg    string
	}{
		"matches": {
			resolver: fakeResolver{addrs: []string{"203.0.113.10"}},
		},
		"matches, duplicate answers": {
			resolver: fakeResolver{addrs: []string{"203.0.113.10", "203.0.113.10"}},
		},
		"matches, IPv4-mapped IPv6 form": {
			resolver: fakeResolver{addrs: []string{"::ffff:203.0.113.10"}},
		},
		"no such host": {
			resolver:   fakeResolver{err: &net.DNSError{Err: "no such host", Name: "www.example.org.", Server: resolverAddr, IsNotFound: true}},
			wantReason: ErrNotFound,
			wantMsg:    "no DNS record found for www.example.org",
		},
		"empty answer": {
			resolver:   fakeResolver{},
			wantReason: ErrNotFound,
			wantMsg:    "no DNS record found for www.example.org",
		},
		"resolver reports a timeout": {
			resolver:   fakeResolver{err: &net.DNSError{Err: "i/o timeout", Name: "www.example.org.", Server: resolverAddr, IsTimeout: true}},
			wantReason: ErrTimeout,
			wantMsg:    "DNS lookup for www.example.org timed out",
		},
		"resolver never answers": {
			resolver:   fakeResolver{block: true},
			wantReason: ErrTimeout,
			wantMsg:    "DNS lookup for www.example.org timed out",
		},
		"server failure": {
			resolver:   fakeResolver{err: &net.DNSError{Err: "server misbehaving", Name: "www.example.org.", Server: resolverAddr, IsTemporary: true}},
			wantReason: ErrLookupFailed,
			wantMsg:    "DNS lookup for www.example.org failed",
		},
		"unexpected resolver error": {
			resolver:   fakeResolver{err: errors.New("dial udp " + resolverAddr + ": connect: network is unreachable")},
			wantReason: ErrLookupFailed,
			wantMsg:    "DNS lookup for www.example.org failed",
		},
		"another address": {
			resolver:   fakeResolver{addrs: []string{"198.51.100.7"}},
			wantReason: ErrMismatch,
			wantMsg:    "www.example.org resolves to 198.51.100.7; it must resolve only to 203.0.113.10",
		},
		"mixed addresses": {
			resolver:   fakeResolver{addrs: []string{"203.0.113.10", "198.51.100.7"}},
			wantReason: ErrMismatch,
			wantMsg:    "www.example.org also resolves to 198.51.100.7; it must resolve only to 203.0.113.10",
		},
		"mixed addresses, other order": {
			resolver:   fakeResolver{addrs: []string{"198.51.100.7", "203.0.113.10"}},
			wantReason: ErrMismatch,
			wantMsg:    "www.example.org also resolves to 198.51.100.7; it must resolve only to 203.0.113.10",
		},
		"only an IPv6 answer": {
			resolver:   fakeResolver{addrs: []string{"2001:db8::1"}},
			wantReason: ErrMismatch,
			wantMsg:    "www.example.org resolves to 2001:db8::1; it must resolve only to 203.0.113.10",
		},
		"correct A record plus an AAAA record": {
			resolver:   fakeResolver{addrs: []string{"203.0.113.10", "2001:db8::1"}},
			wantReason: ErrMismatch,
			wantMsg:    "www.example.org also resolves to 2001:db8::1; it must resolve only to 203.0.113.10",
		},
		"many other addresses": {
			resolver: fakeResolver{addrs: []string{
				"198.51.100.5", "198.51.100.4", "198.51.100.3", "198.51.100.2", "198.51.100.1", "198.51.100.1",
			}},
			wantReason: ErrMismatch,
			wantMsg:    "www.example.org resolves to 198.51.100.1, 198.51.100.2, 198.51.100.3 and 2 more; it must resolve only to 203.0.113.10",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resolver := tc.resolver
			v := Verifier{Resolver: &resolver, PublicIP: publicIP, Timeout: 50 * time.Millisecond}
			err := v.Verify(context.Background(), "www.example.org")

			// The name is looked up as an absolute name, exactly once, with a deadline.
			if len(resolver.asked) != 1 || resolver.asked[0] != "www.example.org." {
				t.Errorf("resolver was asked %q, want one lookup of %q", resolver.asked, "www.example.org.")
			}
			if !resolver.hadDeadline {
				t.Error("the lookup context has no deadline")
			}

			if tc.wantReason == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.wantMsg {
				t.Errorf("message = %q, want %q", err, tc.wantMsg)
			}
			for _, reason := range []error{ErrNotFound, ErrMismatch, ErrTimeout, ErrLookupFailed, ErrNoPublicIP} {
				if got, want := errors.Is(err, reason), reason == tc.wantReason; got != want {
					t.Errorf("errors.Is(err, %q) = %v, want %v", reason, got, want)
				}
			}
			// The message is shown in the UI: no resolver internals.
			if strings.Contains(err.Error(), resolverAddr) || strings.Contains(err.Error(), "192.0.2.53") {
				t.Errorf("message %q leaks the resolver address", err)
			}
		})
	}
}

func TestVerifyKeepsTheCauseForLogs(t *testing.T) {
	cause := &net.DNSError{Err: "server misbehaving", Name: "www.example.org.", Server: "192.0.2.53:53"}
	v := Verifier{Resolver: &fakeResolver{err: cause}, PublicIP: publicIP}
	err := v.Verify(context.Background(), "www.example.org")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || dnsErr != cause {
		t.Errorf("the resolver error is not reachable from %v", err)
	}
}

func TestVerifyIPv6PublicIP(t *testing.T) {
	v6 := net.ParseIP("2001:db8::10")
	ok := Verifier{Resolver: &fakeResolver{addrs: []string{"2001:db8:0:0::10"}}, PublicIP: v6}
	if err := ok.Verify(context.Background(), "www.example.org"); err != nil {
		t.Errorf("Verify: %v", err)
	}
	mixed := Verifier{Resolver: &fakeResolver{addrs: []string{"2001:db8::10", "203.0.113.10"}}, PublicIP: v6}
	err := mixed.Verify(context.Background(), "www.example.org")
	if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), "also resolves to 203.0.113.10") {
		t.Errorf("error = %v", err)
	}
}

func TestVerifyAcceptsTrailingDot(t *testing.T) {
	resolver := &fakeResolver{addrs: []string{"203.0.113.10"}}
	v := Verifier{Resolver: resolver, PublicIP: publicIP}
	if err := v.Verify(context.Background(), "www.example.org."); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(resolver.asked) != 1 || resolver.asked[0] != "www.example.org." {
		t.Errorf("resolver was asked %q", resolver.asked)
	}
}

func TestVerifyWithoutPublicIP(t *testing.T) {
	for name, ip := range map[string]net.IP{"nil": nil, "unspecified": net.IPv4zero, "unparsed": net.ParseIP("not an address")} {
		resolver := &fakeResolver{addrs: []string{"203.0.113.10"}}
		err := Verifier{Resolver: resolver, PublicIP: ip}.Verify(context.Background(), "www.example.org")
		if !errors.Is(err, ErrNoPublicIP) {
			t.Errorf("%s: error = %v, want ErrNoPublicIP", name, err)
		}
		if len(resolver.asked) != 0 {
			t.Errorf("%s: the resolver was asked without a public IP", name)
		}
	}
}

func TestVerifyEmptyHostname(t *testing.T) {
	resolver := &fakeResolver{}
	err := Verifier{Resolver: resolver, PublicIP: publicIP}.Verify(context.Background(), "")
	if err == nil || len(resolver.asked) != 0 {
		t.Errorf("error = %v, lookups = %q", err, resolver.asked)
	}
}

func TestVerifyTimeoutBoundsTheLookup(t *testing.T) {
	v := Verifier{Resolver: &fakeResolver{block: true}, PublicIP: publicIP, Timeout: 30 * time.Millisecond}
	start := time.Now()
	err := v.Verify(context.Background(), "www.example.org")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Verify returned after %s", elapsed)
	}
}

func TestVerifyCallerDeadlineIsATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	v := Verifier{Resolver: &fakeResolver{block: true}, PublicIP: publicIP, Timeout: time.Minute}
	if err := v.Verify(ctx, "www.example.org"); !errors.Is(err, ErrTimeout) {
		t.Errorf("error = %v, want ErrTimeout", err)
	}
}

// A canceled caller (shutdown) is not a verification result and must not be
// mistaken for one.
func TestVerifyCanceledByCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	v := Verifier{Resolver: &fakeResolver{block: true}, PublicIP: publicIP, Timeout: time.Minute}
	err := v.Verify(ctx, "www.example.org")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	for _, reason := range []error{ErrNotFound, ErrMismatch, ErrTimeout, ErrLookupFailed} {
		if errors.Is(err, reason) {
			t.Errorf("a canceled verification matches %q", reason)
		}
	}
}
