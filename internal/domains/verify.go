package domains

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// DefaultVerifyTimeout bounds one DNS lookup when Verifier.Timeout is zero.
const DefaultVerifyTimeout = 10 * time.Second

// maxListedAddrs is how many unexpected addresses a mismatch message spells out.
const maxListedAddrs = 3

// The reasons a verification can fail. The errors returned by Verify match
// exactly one of them with errors.Is, so callers can translate the reason or
// decide whether a retry makes sense without parsing the message.
var (
	// ErrNotFound: the name has no address record (yet).
	ErrNotFound = errors.New("hostname does not resolve")
	// ErrMismatch: the name resolves, but not only to the public IP.
	ErrMismatch = errors.New("hostname does not resolve to the public IP")
	// ErrTimeout: the lookup did not finish in time.
	ErrTimeout = errors.New("DNS lookup timed out")
	// ErrLookupFailed: the lookup failed for another reason, such as a
	// resolver that is unreachable or answers with a server failure.
	ErrLookupFailed = errors.New("DNS lookup failed")
	// ErrNoPublicIP: the Verifier has no public IP to compare with.
	ErrNoPublicIP = errors.New("the public IP of this server is not configured")
)

// Resolver looks up the addresses of a host. *net.Resolver satisfies it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Verifier checks that a custom domain points at this host.
type Verifier struct {
	// Resolver performs the lookups. Nil means net.DefaultResolver.
	Resolver Resolver
	// PublicIP is the address the hostname must resolve to.
	PublicIP net.IP
	// Timeout bounds one lookup in addition to any deadline on the context.
	// Zero means DefaultVerifyTimeout.
	Timeout time.Duration
}

// verifyError is a verification failure. Its message is short and free of
// resolver internals because it is stored and shown to the user; the
// underlying error stays reachable through errors.As / errors.Is for logs.
type verifyError struct {
	reason error
	msg    string
	cause  error
}

func (e *verifyError) Error() string        { return e.msg }
func (e *verifyError) Is(target error) bool { return target == e.reason }
func (e *verifyError) Unwrap() error        { return e.cause }

// Verify resolves hostname and returns nil only when it resolves and every
// resolved address (IPv4 and IPv6 alike) equals PublicIP. A name that also
// has other addresses would send part of the traffic, and possibly the
// certificate validation, to another host, so it is not accepted.
//
// hostname must already be normalized (see NormalizeHostname). It is looked
// up as an absolute name (with a trailing dot) so the search domains of the
// host are never appended.
//
// On failure the error matches one of ErrNotFound, ErrMismatch, ErrTimeout,
// ErrLookupFailed or ErrNoPublicIP, and its message can be shown to the user.
// When the caller's context is canceled, the context error is returned instead.
func (v Verifier) Verify(ctx context.Context, hostname string) error {
	if v.PublicIP == nil || v.PublicIP.IsUnspecified() {
		return ErrNoPublicIP
	}
	name := strings.TrimSuffix(hostname, ".")
	if name == "" {
		return &verifyError{reason: ErrLookupFailed, msg: "hostname is empty"}
	}

	resolver := v.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addrs, err := resolver.LookupIPAddr(lookupCtx, name+".")
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("verify %s: %w", name, context.Canceled)
		}
		var dnsErr *net.DNSError
		isDNSErr := errors.As(err, &dnsErr)
		switch {
		case isDNSErr && dnsErr.IsNotFound:
			return notFound(name, err)
		case lookupCtx.Err() != nil, errors.Is(err, context.DeadlineExceeded), isDNSErr && dnsErr.IsTimeout:
			return &verifyError{reason: ErrTimeout, msg: fmt.Sprintf("DNS lookup for %s timed out", name), cause: err}
		default:
			return &verifyError{reason: ErrLookupFailed, msg: fmt.Sprintf("DNS lookup for %s failed", name), cause: err}
		}
	}
	if len(addrs) == 0 {
		return notFound(name, nil)
	}

	matched := false
	var others []string
	for _, addr := range addrs {
		if v.PublicIP.Equal(addr.IP) {
			matched = true
			continue
		}
		if s := addr.IP.String(); !slices.Contains(others, s) {
			others = append(others, s)
		}
	}
	if len(others) == 0 {
		return nil
	}
	// Sorted so the stored message does not change with the answer order.
	slices.Sort(others)
	listed := others
	if len(listed) > maxListedAddrs {
		listed = listed[:maxListedAddrs]
	}
	list := strings.Join(listed, ", ")
	if rest := len(others) - len(listed); rest > 0 {
		list += fmt.Sprintf(" and %d more", rest)
	}
	verb := "resolves to"
	if matched {
		verb = "also resolves to"
	}
	return &verifyError{
		reason: ErrMismatch,
		msg:    fmt.Sprintf("%s %s %s; it must resolve only to %s", name, verb, list, v.PublicIP),
	}
}

func notFound(name string, cause error) error {
	return &verifyError{reason: ErrNotFound, msg: fmt.Sprintf("no DNS record found for %s", name), cause: cause}
}
