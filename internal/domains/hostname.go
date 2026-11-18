// Package domains holds the logic for custom domains: hostname validation,
// DNS verification against the public IP of the host and the DNS records to
// show to the user. It has no storage and no scheduling of its own.
package domains

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrInvalidHostname matches (with errors.Is) every error returned by
// NormalizeHostname.
var ErrInvalidHostname = errors.New("invalid hostname")

// ErrReservedHostname matches the error NormalizeHostname returns for names
// equal to or under the base domain. Such an error also matches
// ErrInvalidHostname.
var ErrReservedHostname = errors.New("reserved hostname")

// hostnameError carries a message that can be shown to the user as it is.
type hostnameError struct {
	msg      string
	reserved bool
}

func (e *hostnameError) Error() string { return e.msg }

func (e *hostnameError) Is(target error) bool {
	return target == ErrInvalidHostname || (e.reserved && target == ErrReservedHostname)
}

const (
	maxHostnameLength = 253
	maxLabelLength    = 63
)

// NormalizeHostname turns user input into the canonical form of a custom
// domain: surrounding space trimmed, lowercase, without a trailing dot.
//
// It rejects empty input, IP addresses, wildcards, anything that is not a
// bare hostname (scheme, port, path, user info), names that are not valid DNS
// hostnames and names equal to or under baseDomain: the automatic
// <slug>.<baseDomain> names cannot be registered as custom domains. An empty
// baseDomain disables that last check.
//
// Internationalized names must be given in their punycode (xn--) form.
// Every returned error matches ErrInvalidHostname and has a short message
// that is safe to show to the user.
func NormalizeHostname(hostname, baseDomain string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(hostname))
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return "", invalid("hostname is empty")
	}
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return "", invalid("an IP address cannot be used as a domain")
	}
	if strings.Contains(h, "*") {
		return "", invalid("wildcard domains are not supported")
	}
	if strings.ContainsAny(h, "/:@?#") {
		return "", invalid("enter the hostname only, without scheme, port or path")
	}
	if len(h) > maxHostnameLength {
		return "", invalid("hostname is longer than %d characters", maxHostnameLength)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", invalid("hostname must be fully qualified, like www.example.com")
	}
	for _, label := range labels {
		if err := checkLabel(label); err != nil {
			return "", err
		}
	}
	if isNumeric(labels[len(labels)-1]) {
		return "", invalid("the last label of a hostname cannot be a number")
	}

	base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(baseDomain)), ".")
	if base != "" && (h == base || strings.HasSuffix(h, "."+base)) {
		return "", &hostnameError{
			msg:      fmt.Sprintf("%s and its subdomains are assigned automatically and cannot be added as custom domains", base),
			reserved: true,
		}
	}
	return h, nil
}

func checkLabel(label string) error {
	switch {
	case label == "":
		return invalid("hostname has an empty label")
	case len(label) > maxLabelLength:
		return invalid("hostname has a label longer than %d characters", maxLabelLength)
	case label[0] == '-' || label[len(label)-1] == '-':
		return invalid("hostname labels cannot start or end with a hyphen")
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		case r > 127:
			return invalid("internationalized names must be entered in punycode (xn--) form")
		default:
			return invalid("hostname contains the invalid character %q", r)
		}
	}
	return nil
}

func isNumeric(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func invalid(format string, args ...any) error {
	return &hostnameError{msg: fmt.Sprintf(format, args...)}
}

// Record is one DNS record the user may create for a custom domain.
type Record struct {
	// Type is the record type: "CNAME" or "A".
	Type string
	// Name is the hostname the record is created for.
	Name string
	// Value is the record target: a hostname for CNAME, an address for A.
	Value string
}

// Instructions returns the DNS records that make hostname point at the app:
// a CNAME to the automatic <slug>.<baseDomain> name, which is the choice for
// subdomains, and an A record to publicIP, which is the only choice for an
// apex domain. Exactly one of them is to be created; which one applies depends
// on whether hostname is an apex, which this package does not try to detect.
//
// The A record is left out when publicIP is empty.
func Instructions(hostname, slug, baseDomain, publicIP string) []Record {
	records := []Record{{Type: "CNAME", Name: hostname, Value: slug + "." + baseDomain}}
	if publicIP != "" {
		records = append(records, Record{Type: "A", Name: hostname, Value: publicIP})
	}
	return records
}
