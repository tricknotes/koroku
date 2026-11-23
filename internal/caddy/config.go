// Package caddy generates the complete Caddy JSON configuration from plain
// input and loads it through the Caddy admin API.
//
// The database is the single source of truth: the caller reads it, fills an
// Input and calls Generate. The output is deterministic (the same Input always
// yields the same bytes), so re-loading an unchanged configuration is a no-op
// on the Caddy side.
package caddy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DefaultAdminListen is the admin API address used when Input.AdminListen is empty.
const DefaultAdminListen = "127.0.0.1:2019"

const (
	// streamCloseDelay keeps upgraded connections (WebSocket) of the previous
	// configuration open for a while after a reload. Without it Caddy closes
	// every proxied WebSocket the moment a new configuration is loaded.
	streamCloseDelay = "5m"

	// While an app's containers are recreated (deploy, rollback, restart)
	// nothing answers on its loopback port. Caddy holds such a request and
	// tries the upstream again every upstreamTryInterval until
	// upstreamTryDuration has passed, so a short restart becomes a slow
	// response instead of a 502. Caddy retries any method when the
	// connection could not be made (nothing was sent), but only GET once
	// a connection was made, which is what docker-proxy gives while the
	// app inside the container is not listening (ARCHITECTURE 8).
	upstreamTryDuration = "30s"
	upstreamTryInterval = "250ms"

	// Rotation of the per-app access logs. A later collector ships them to
	// object storage; these values only bound the local disk usage.
	accessLogRollSizeMB   = 50
	accessLogRollKeep     = 5
	accessLogRollKeepDays = 14

	// What a client chooses is cut in the access log to this many bytes
	// (see cutField), so that an entry stays small however the request is
	// made: about 2.5 KiB at most, far below the line length the log
	// collector keeps (16 KiB).
	accessLogMaxMethod    = 32
	accessLogMaxHost      = 300
	accessLogMaxURI       = 1000
	accessLogMaxUserAgent = 200
	accessLogMaxLocation  = 500
)

// Fields added to an access log entry in place of the headers, which are not
// logged. internal/idle reads accessLogLocationField.
const (
	accessLogUserAgentField = "user_agent"
	accessLogLocationField  = "location"
)

// stoppedPage is served with status 503 for apps that are stopped. Caddy
// expands {placeholders} in response bodies, so the page must not contain
// braces (hence inline styles instead of a style sheet).
const stoppedPage = `<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>停止中</title>
</head>
<body style="font-family: sans-serif; margin: 20vh auto 0; max-width: 32rem; padding: 0 1rem; color: #333;">
<h1 style="font-size: 1.25rem;">このアプリは停止中です</h1>
<p>しばらくしてから、もう一度アクセスしてください。</p>
</body>
</html>
`

// Input is everything needed to generate the Caddy configuration.
type Input struct {
	// ConsoleHost is the hostname of the management UI, e.g. console.dev.example.com.
	ConsoleHost string
	// ConsoleUpstream is the address koroku listens on, e.g. 127.0.0.1:8080.
	ConsoleUpstream string
	// AdminListen is the Caddy admin API address. It is always written to the
	// generated configuration so the admin API survives a load. It must be a
	// loopback address. Empty means DefaultAdminListen.
	AdminListen string
	// ACMECA is an optional ACME directory URL (for example the Let's Encrypt
	// staging endpoint during development). Empty means the Caddy default.
	ACMECA string
	// ACMEEmail is an optional contact address for the ACME account.
	ACMEEmail string
	// AccessLogDir is optional. When set, each app writes its access log to
	// <AccessLogDir>/<slug>.log.
	AccessLogDir string
	// ListenIP is optional. When set, Caddy binds ports 80 and 443 on this
	// address only (for example 127.0.0.1 for local development) instead of
	// on every interface.
	ListenIP string
	// InternalCA makes Caddy issue certificates from its own local CA
	// instead of obtaining them over ACME. For local development, where
	// hostnames such as *.localhost cannot get a public certificate. ACMECA
	// and ACMEEmail are ignored.
	InternalCA bool
	// Apps are the applications to route. Order does not matter.
	Apps []App
}

// App is one routed application.
type App struct {
	// Slug identifies the app. It must be a single lowercase DNS label.
	Slug string
	// HostPort is the loopback port the app's public service is published on.
	HostPort int
	// Hostnames are all hostnames to serve: the automatic <slug>.<base> name
	// plus the active custom domains. The caller filters out pending ones.
	Hostnames []string
	// Primary is optional. When set, requests to the other hostnames are
	// answered with a 308 redirect to https://<Primary><uri>.
	Primary string
	// Stopped keeps the route but answers 503 with a fixed page instead of proxying.
	Stopped bool
	// Sleeping sends the requests that would reach the app to koroku, at
	// Input.ConsoleUpstream, which wakes the app and then serves them
	// (ARCHITECTURE 8.4). Everything in front of the app (the redirect to
	// the primary hostname, the access protection) stays as it is.
	// Stopped wins over it.
	Sleeping bool
	// Protected lets only browsers with a valid gate cookie through; others
	// are sent to the console to log in (ARCHITECTURE 8.3). koroku, at
	// Input.ConsoleUpstream, decides. It does not apply to a stopped app.
	Protected bool
	// ExemptPaths are path prefixes of a protected app that skip the gate,
	// for requests that spell them exactly as written here (exemptMatcher).
	ExemptPaths []string
	// NoIndex adds X-Robots-Tag: noindex to every response (staging apps).
	NoIndex bool
}

// obj is a JSON object. encoding/json writes map keys in sorted order, which
// keeps the output stable.
type obj = map[string]any

// Generate validates in and returns the complete Caddy JSON configuration.
// Every problem found in the input is reported at once.
func Generate(in Input) ([]byte, error) {
	in = normalize(in)
	if err := validate(in); err != nil {
		return nil, fmt.Errorf("generate caddy config: %w", err)
	}

	var routes []obj
	if fields := accessLogFieldsRoute(in); fields != nil {
		routes = append(routes, fields)
	}
	routes = append(routes, consoleRoute(in))
	for _, app := range in.Apps {
		routes = append(routes, appRoutes(app, in.ConsoleUpstream)...)
	}
	// Requests that reach the HTTPS server with a Host header we do not serve.
	routes = append(routes, notFoundRoute())

	httpsServer := obj{
		"listen": []string{listenAddr(in, 443)},
		"routes": routes,
	}
	if logs := serverLogs(in); logs != nil {
		httpsServer["logs"] = logs
	}

	cfg := obj{
		"admin": obj{"listen": in.AdminListen},
		"apps": obj{
			"http": obj{
				"servers": obj{
					// Caddy's automatic HTTPS inserts the HTTP->HTTPS redirects
					// for every served hostname in front of this catch-all, so
					// unknown hosts get a 404 and nothing else.
					"http": obj{
						"listen": []string{listenAddr(in, 80)},
						"routes": []obj{notFoundRoute()},
					},
					"https": httpsServer,
				},
			},
			"tls": obj{
				"automation": obj{
					// One catch-all policy: certificates are obtained per
					// hostname from a single ACME CA. No on-demand TLS, so only
					// the hostnames in the routes above ever get a certificate.
					"policies": []obj{{"issuers": []obj{acmeIssuer(in)}}},
				},
			},
		},
	}
	if in.InternalCA {
		// The container or service Caddy runs in cannot change the trust
		// store of the machine the browser runs on; the operator imports
		// the root certificate by hand.
		cfg["apps"].(obj)["pki"] = obj{
			"certificate_authorities": obj{"local": obj{"install_trust": false}},
		}
	}
	if logging := logging(in); logging != nil {
		cfg["logging"] = logging
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("generate caddy config: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// normalize applies defaults and puts apps and hostnames in a stable order.
// It never modifies the caller's slices.
func normalize(in Input) Input {
	in.ConsoleHost = strings.ToLower(in.ConsoleHost)
	if in.AdminListen == "" {
		in.AdminListen = DefaultAdminListen
	}
	if in.AccessLogDir != "" {
		in.AccessLogDir = path.Clean(in.AccessLogDir)
	}
	apps := make([]App, len(in.Apps))
	for i, app := range in.Apps {
		hostnames := make([]string, len(app.Hostnames))
		for j, h := range app.Hostnames {
			hostnames[j] = strings.ToLower(h)
		}
		slices.Sort(hostnames)
		app.Hostnames = hostnames
		app.Primary = strings.ToLower(app.Primary)
		exempt := slices.Clone(app.ExemptPaths)
		slices.Sort(exempt)
		app.ExemptPaths = slices.Compact(exempt)
		apps[i] = app
	}
	slices.SortStableFunc(apps, func(a, b App) int { return strings.Compare(a.Slug, b.Slug) })
	in.Apps = apps
	return in
}

func validate(in Input) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	// owners maps every hostname to what serves it, to detect duplicates.
	owners := map[string]string{}
	if in.ConsoleHost == "" {
		fail("console host is required")
	} else if err := checkHostname(in.ConsoleHost); err != nil {
		fail("console host %q: %w", in.ConsoleHost, err)
	} else {
		owners[in.ConsoleHost] = "the console"
	}
	if in.ConsoleUpstream == "" {
		fail("console upstream is required")
	} else if err := checkHostPort(in.ConsoleUpstream); err != nil {
		fail("console upstream %q: %w", in.ConsoleUpstream, err)
	}
	if err := checkAdminListen(in.AdminListen); err != nil {
		fail("admin listen %q: %w", in.AdminListen, err)
	}
	if in.ACMECA != "" {
		if u, err := url.Parse(in.ACMECA); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			fail("ACME CA %q must be an http(s) URL", in.ACMECA)
		}
	}
	if in.ACMEEmail != "" {
		if at := strings.IndexByte(in.ACMEEmail, '@'); at <= 0 || at == len(in.ACMEEmail)-1 || strings.ContainsAny(in.ACMEEmail, " \t\r\n{}") {
			fail("ACME email is not an e-mail address")
		}
	}
	if in.ListenIP != "" && net.ParseIP(in.ListenIP) == nil {
		fail("listen IP %q is not an IP address", in.ListenIP)
	}
	if in.AccessLogDir != "" && (!path.IsAbs(in.AccessLogDir) || strings.ContainsAny(in.AccessLogDir, "{}")) {
		fail("access log dir %q must be an absolute path without placeholders", in.AccessLogDir)
	}

	slugs := map[string]bool{}
	ports := map[int]string{}
	for i, app := range in.Apps {
		name := fmt.Sprintf("app %q", app.Slug)
		switch {
		case app.Slug == "":
			name = fmt.Sprintf("app #%d", i)
			fail("%s: slug is required", name)
		case !validLabel(app.Slug):
			fail("%s: slug must be a lowercase DNS label", name)
		case slugs[app.Slug]:
			fail("%s: duplicate slug", name)
		}
		slugs[app.Slug] = true

		if app.HostPort < 1 || app.HostPort > 65535 {
			fail("%s: host port %d is out of range", name, app.HostPort)
		} else if other, ok := ports[app.HostPort]; ok {
			fail("%s: host port %d is already used by %s", name, app.HostPort, other)
		} else {
			ports[app.HostPort] = name
		}

		if len(app.Hostnames) == 0 {
			fail("%s: at least one hostname is required", name)
		}
		for _, h := range app.Hostnames {
			if err := checkHostname(h); err != nil {
				fail("%s: hostname %q: %w", name, h, err)
				continue
			}
			if other, ok := owners[h]; ok {
				fail("%s: hostname %q is already served by %s", name, h, other)
				continue
			}
			owners[h] = name
		}
		if app.Primary != "" && !slices.Contains(app.Hostnames, app.Primary) {
			fail("%s: primary %q is not one of its hostnames", name, app.Primary)
		}
		for _, p := range app.ExemptPaths {
			if err := CheckExemptPath(p); err != nil {
				fail("%s: exempt path %q: %w", name, p, err)
			}
		}
	}
	return errors.Join(errs...)
}

func consoleRoute(in Input) obj {
	return obj{
		"match": []obj{{"host": []string{in.ConsoleHost}}},
		"handle": []obj{{
			"handler":   "reverse_proxy",
			"upstreams": []obj{{"dial": in.ConsoleUpstream}},
			"headers":   dropWakeHeader(),
			// The console streams deploy logs over SSE: never buffer responses.
			"flush_interval": -1,
		}},
		"terminal": true,
	}
}

// appRoutes returns the routes of one app: an optional redirect of the
// non-primary hostnames, then the route that serves the app.
func appRoutes(app App, gateUpstream string) []obj {
	served := app.Hostnames
	var routes []obj
	if app.Primary != "" {
		var others []string
		for _, h := range app.Hostnames {
			if h != app.Primary {
				others = append(others, h)
			}
		}
		served = []string{app.Primary}
		if len(others) > 0 {
			routes = append(routes, obj{
				"match": []obj{{"host": others}},
				"handle": []obj{{
					"handler":     "static_response",
					"status_code": 308,
					"headers": obj{
						"Location": []string{"https://" + app.Primary + "{http.request.uri}"},
					},
				}},
				"terminal": true,
			})
		}
	}

	var handlers []obj
	if app.NoIndex {
		// Deferred so that it is set when the response is written and
		// replaces whatever the app sends.
		handlers = append(handlers, obj{
			"handler":  "headers",
			"response": obj{"set": obj{"X-Robots-Tag": []string{"noindex"}}, "deferred": true},
		})
	}
	switch {
	case app.Stopped:
		handlers = append(handlers, obj{
			"handler":     "static_response",
			"status_code": 503,
			"headers": obj{
				"Cache-Control": []string{"no-store"},
				"Content-Type":  []string{"text/html; charset=utf-8"},
			},
			"body": stoppedPage,
		})
	case app.Protected:
		handlers = append(handlers, obj{"handler": "subroute", "routes": gateRoutes(app, gateUpstream)})
	default:
		handlers = append(handlers, upstream(app, gateUpstream))
	}
	return append(routes, obj{
		"match":    []obj{{"host": served}},
		"handle":   handlers,
		"terminal": true,
	})
}

// appProxy forwards to the app's public service on its loopback port.
func appProxy(app App) obj {
	return obj{
		"handler":   "reverse_proxy",
		"upstreams": []obj{{"dial": net.JoinHostPort("127.0.0.1", strconv.Itoa(app.HostPort))}},
		// WebSocket and SSE work with the reverse proxy defaults (upgrades
		// are passed through, event streams are flushed immediately).
		"stream_close_delay": streamCloseDelay,
		"load_balancing": obj{
			"try_duration": upstreamTryDuration,
			"try_interval": upstreamTryInterval,
		},
	}
}

// Paths and names of the access protection (ARCHITECTURE 8.3), shared with
// the koroku handlers that answer them.
const (
	// GateCookie is the host-only cookie that admits a browser to one
	// protected hostname.
	GateCookie = "__Host-koroku_gate"
	// GateCheckPath is the koroku endpoint forward_auth asks.
	GateCheckPath = "/auth/gate/check"
	// ReservedPrefix is the path prefix on app hosts that koroku owns.
	ReservedPrefix = "/.koroku/"
	// GateAuthPrefix is the part of ReservedPrefix that reaches koroku.
	GateAuthPrefix = ReservedPrefix + "auth/"
)

// gateRoutes put a protected app behind koroku: its own login endpoints go
// to koroku, the rest of /.koroku/ is refused, every other request outside
// the exempt prefixes is checked with forward_auth, and what passes is
// proxied to the app without the gate cookie. Nothing a client sends in
// WakeHeader reaches koroku through the login endpoints or the check.
func gateRoutes(app App, gateUpstream string) []obj {
	toKoroku := []obj{{"dial": gateUpstream}}
	routes := []obj{
		{
			"match": []obj{{"path": []string{GateAuthPrefix + "*"}}},
			"handle": []obj{{
				"handler":   "reverse_proxy",
				"upstreams": toKoroku,
				"headers":   dropWakeHeader(),
			}},
			"terminal": true,
		},
		{
			"match":    []obj{{"path": []string{strings.TrimSuffix(ReservedPrefix, "/"), ReservedPrefix + "*"}}},
			"handle":   []obj{{"handler": "static_response", "status_code": 404}},
			"terminal": true,
		},
	}

	// The expansion of Caddy's forward_auth directive: ask koroku with a
	// GET (the body stays for the app), pass the original method and URI,
	// and go on to the next route on a 2xx. Any other answer (a redirect to
	// the console, 401, 403) goes to the browser as it is.
	check := obj{
		"handler":   "reverse_proxy",
		"upstreams": toKoroku,
		"rewrite":   obj{"method": "GET", "uri": GateCheckPath},
		"headers": obj{"request": obj{
			"set": obj{
				"X-Forwarded-Method": []string{"{http.request.method}"},
				"X-Forwarded-Uri":    []string{"{http.request.uri}"},
			},
			"delete": []string{WakeHeader},
		}},
		"handle_response": []obj{{
			"match":  obj{"status_code": []int{2}},
			"routes": []obj{{"handle": []obj{{"handler": "vars"}}}},
		}},
	}
	checkRoute := obj{"handle": []obj{check}}
	if len(app.ExemptPaths) > 0 {
		checkRoute["match"] = []obj{{"not": []obj{exemptMatcher(app.ExemptPaths)}}}
	}
	routes = append(routes, checkRoute)

	// The gate cookie is koroku's; the app never needs it. Browsers send
	// one Cookie header (HTTP/2 joins its fields), so two replacements cover
	// the cookie at the start and further along the list.
	strip := obj{
		"handler": "headers",
		"request": obj{"replace": obj{"Cookie": []obj{
			{"search_regexp": "^" + GateCookie + "=[^;]*(;\\s*)?", "replace": ""},
			{"search_regexp": ";\\s*" + GateCookie + "=[^;]*", "replace": ""},
		}}},
	}
	return append(routes, obj{"handle": []obj{strip, upstream(app, gateUpstream)}})
}

// rawURI is the Caddy placeholder of the request target exactly as the
// client sent it: still percent-encoded, with its dot segments and its
// query.
const rawURI = "{http.request.orig_uri}"

// ambiguousRawPath is a regular expression that matches a request target
// whose path the servers it passes could read in more than one way: a "."
// or ".." segment (also with path parameters, as in "..;x"), an empty
// segment ("//"), a backslash, or a percent-encoded ".", "/" or "\". The
// query is not looked at. Such a request never skips the access protection.
const ambiguousRawPath = `^[^?]*(?://|[\\]|%2[eEfF]|%5[cC]|/[.][.]?(?:[/;?]|$))`

// exemptRawPrefix is the regular expression of the request targets that
// start with one of the exempt path prefixes, spelled exactly as configured.
func exemptRawPrefix(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, regexp.QuoteMeta(p))
	}
	return "^(?:" + strings.Join(quoted, "|") + ")"
}

// exemptMatcher matches the requests that skip the access protection. Three
// conditions must hold together:
//
//   - the path as Caddy reads it (decoded, cleaned, compared without case)
//     is under an exempt prefix;
//   - the request target as sent starts with that prefix in the configured
//     spelling, so /WEBHOOKS/ and /%77ebhooks/ are not exempt for /webhooks/;
//   - the path as sent is not ambiguous (ambiguousRawPath). Caddy forwards
//     the target as sent, and the app may resolve /admin/..%2Fwebhooks/x to
//     another path than Caddy did.
//
// The expression reads the raw target through the placeholder function of
// CEL, which returns the value as it is; the vars_regexp matcher would
// expand placeholders that a client wrote into the URI. Exempt paths never
// contain a quote, a brace or a backslash (CheckExemptPath), so they are safe
// inside the raw string literal and are not taken for placeholders.
func exemptMatcher(paths []string) obj {
	prefixes := make([]string, 0, len(paths))
	for _, p := range paths {
		prefixes = append(prefixes, p+"*")
	}
	return obj{
		"path": prefixes,
		"expression": rawURI + ".matches(r'" + exemptRawPrefix(paths) + "') && !" +
			rawURI + ".matches(r'" + ambiguousRawPath + "')",
	}
}

// MatchedPath returns the decoded request path p the way Caddy's path
// matcher compares it: lower-cased, with "." and ".." segments resolved and
// repeated slashes merged, keeping a trailing slash. koroku uses it to tell
// which of its own paths Caddy routed a request as.
func MatchedPath(p string) string {
	return cleanPath(strings.ToLower(p))
}

// IsCleanPath reports whether the decoded request path p has no "." or ".."
// segments and no repeated slashes.
func IsCleanPath(p string) bool {
	return cleanPath(p) == p
}

// cleanPath is path.Clean that keeps a trailing slash.
func cleanPath(p string) string {
	cleaned := path.Clean(p)
	if cleaned != "/" && strings.HasSuffix(p, "/") {
		cleaned += "/"
	}
	return cleaned
}

// WakeHeader names the app a request forwarded to koroku is for, while
// the app sleeps (ARCHITECTURE 8.4). Only wakeProxy sets it, replacing
// whatever the client sent; every other route to koroku deletes it.
const WakeHeader = "X-Koroku-Wake"

// dropWakeHeader is the header manipulation of the proxies to koroku that
// are not the wake route: a client cannot make koroku take its request for
// one Caddy forwarded for a sleeping app.
func dropWakeHeader() obj {
	return obj{"request": obj{"delete": []string{WakeHeader}}}
}

// upstream is where the requests that pass everything in front of the app
// go: the app itself, or koroku while the app sleeps.
func upstream(app App, korokuUpstream string) obj {
	if app.Sleeping {
		return wakeProxy(app, korokuUpstream)
	}
	return appProxy(app)
}

// wakeProxy forwards to koroku, which wakes the app, waits for it and
// proxies the request on. Responses stream like the app's own. It has no
// retries: koroku holds the request itself.
func wakeProxy(app App, korokuUpstream string) obj {
	return obj{
		"handler":   "reverse_proxy",
		"upstreams": []obj{{"dial": korokuUpstream}},
		"headers": obj{"request": obj{"set": obj{
			WakeHeader: []string{app.Slug},
		}}},
		"flush_interval":     -1,
		"stream_close_delay": streamCloseDelay,
	}
}

// CheckExemptPath accepts a path prefix that may skip the access protection:
// an absolute path of a few safe characters, so that it is never read as a
// Caddy wildcard, escape or placeholder, and neither the whole site nor
// koroku's own prefix.
func CheckExemptPath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return errors.New("must start with /")
	case p == "/":
		return errors.New("must not be / (turn the protection off instead)")
	case len(p) > 200:
		return errors.New("must be at most 200 characters")
	case strings.HasPrefix(strings.ToLower(p), strings.TrimSuffix(ReservedPrefix, "/")):
		return errors.New("must not be under /.koroku")
	case strings.Contains(p, "//"):
		return errors.New("must not contain //")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return errors.New("must not contain . or .. segments")
		}
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._~-/@:+", c) >= 0) {
			return fmt.Errorf("must not contain %q", string(c))
		}
	}
	return nil
}

func notFoundRoute() obj {
	return obj{"handle": []obj{{"handler": "static_response", "status_code": 404}}}
}

// listenAddr is the address of a server on port.
func listenAddr(in Input, port int) string {
	return net.JoinHostPort(in.ListenIP, strconv.Itoa(port))
}

func acmeIssuer(in Input) obj {
	if in.InternalCA {
		return obj{"module": "internal"}
	}
	issuer := obj{"module": "acme"}
	if in.ACMECA != "" {
		issuer["ca"] = in.ACMECA
	}
	if in.ACMEEmail != "" {
		issuer["email"] = in.ACMEEmail
	}
	return issuer
}

// accessLogNamespace is the logger namespace of all access logs.
const accessLogNamespace = "http.log.access"

// accessLoggerName is the suffix of the access logger of an app; Caddy names
// the logger http.log.access.<suffix>. Caddy selects the loggers of a log by
// name prefix, so the names must be prefix-free: with the bare slug, the log
// of "shop" would also receive the entries of "shop-staging". The ".app"
// suffix cannot occur inside a slug and prevents that.
func accessLoggerName(slug string) string { return slug + ".app" }

// serverLogs enables access logging on the HTTPS server and maps every app
// hostname to the logger of its app. Requests for other hosts (the console,
// unknown names) are not logged.
func serverLogs(in Input) obj {
	if in.AccessLogDir == "" {
		return nil
	}
	names := obj{}
	for _, app := range in.Apps {
		for _, h := range app.Hostnames {
			names[h] = accessLoggerName(app.Slug)
		}
	}
	return obj{
		"logger_names":        names,
		"skip_unmapped_hosts": true,
	}
}

// logging defines one file log per app and keeps the access logs out of
// Caddy's default log, which is otherwise left as it is.
func logging(in Input) obj {
	if in.AccessLogDir == "" || len(in.Apps) == 0 {
		return nil
	}
	logs := obj{
		// Every access log entry goes to a per-app file; nothing else about
		// the default log changes.
		"default": obj{"exclude": []string{accessLogNamespace}},
	}
	for _, app := range in.Apps {
		logger := accessLogNamespace + "." + accessLoggerName(app.Slug)
		logs["access-"+app.Slug] = obj{
			"writer": obj{
				"output":         "file",
				"filename":       path.Join(in.AccessLogDir, app.Slug+".log"),
				"roll_size_mb":   accessLogRollSizeMB,
				"roll_keep":      accessLogRollKeep,
				"roll_keep_days": accessLogRollKeepDays,
			},
			"encoder": accessLogEncoder(),
			"include": []string{logger},
		}
	}
	return obj{"logs": logs}
}

// accessLogFieldsRoute adds the User-Agent of the request and the Location
// of the response to the access log entry of every request for an app
// hostname. It sits in front of the routes that answer and is not terminal;
// the values are read once the rest of the chain has answered. It is nil
// when no access log is written.
func accessLogFieldsRoute(in Input) obj {
	if in.AccessLogDir == "" {
		return nil
	}
	var hosts []string
	for _, app := range in.Apps {
		hosts = append(hosts, app.Hostnames...)
	}
	if len(hosts) == 0 {
		return nil
	}
	slices.Sort(hosts)
	return obj{
		"match": []obj{{"host": hosts}},
		"handle": []obj{
			{"handler": "log_append", "key": accessLogUserAgentField, "value": "{http.request.header.User-Agent}"},
			{"handler": "log_append", "key": accessLogLocationField, "value": "{http.response.header.Location}"},
		},
	}
}

// accessLogEncoder writes an access log entry as JSON without the request
// and response headers and the TLS details, and with everything a client
// chooses cut to a fixed length.
//
// The headers are what makes an entry large (a client can send many, up to
// the server's limit) and they carry cookies and credentials. The two values
// worth keeping are added by accessLogFieldsRoute.
func accessLogEncoder() obj {
	return obj{
		"format": "filter",
		"wrap":   obj{"format": "json"},
		"fields": obj{
			"request>headers":       obj{"filter": "delete"},
			"request>tls":           obj{"filter": "delete"},
			"resp_headers":          obj{"filter": "delete"},
			"request>method":        cutField(accessLogMaxMethod),
			"request>host":          cutField(accessLogMaxHost),
			"request>uri":           cutField(accessLogMaxURI),
			accessLogUserAgentField: cutField(accessLogMaxUserAgent),
			accessLogLocationField:  cutField(accessLogMaxLocation),
		},
	}
}

// plainLogChar matches one character that takes one byte in a JSON log
// entry: printable ASCII other than the double quote and the backslash.
// Anything else is written as an escape of up to six bytes.
const plainLogChar = `[ !#-\[\]-~]`

// cutField is a log field filter that keeps a value up to its first max
// plain characters, or up to the first character that is not plain (URLs and
// well-formed header values have none), and marks the cut with "...". A value
// of at most max plain characters stays as it is. So the field takes at most
// max+3 bytes of an entry. max is between 1 and 1000, the largest repeat
// count a Go regular expression takes.
func cutField(max int) obj {
	long := "(" + plainLogChar + "{" + strconv.Itoa(max) + "}).+"
	other := "(" + plainLogChar + "{0," + strconv.Itoa(max-1) + "})[^" + plainLogChar[1:] + ".*"
	return obj{
		"filter": "regexp",
		"regexp": "(?s)^(?:" + long + "|" + other + ")$",
		"value":  "${1}${2}...",
	}
}

// checkHostname accepts a lowercase fully qualified hostname: no wildcard, no
// port, no trailing dot. The strict character set also keeps Caddy
// placeholders ({...}) out of the generated configuration.
func checkHostname(h string) error {
	if len(h) > 253 {
		return errors.New("is longer than 253 characters")
	}
	if net.ParseIP(h) != nil {
		return errors.New("must be a hostname, not an IP address")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return errors.New("must be a fully qualified hostname")
	}
	for _, label := range labels {
		if !validLabel(label) {
			return fmt.Errorf("has an invalid label %q", label)
		}
	}
	return nil
}

// validLabel reports whether s is a lowercase DNS label (letters, digits and
// inner hyphens, at most 63 characters).
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func checkHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be host:port")
	}
	if host == "" || strings.ContainsAny(host, "{} ") {
		return errors.New("must have a host")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("has an invalid port")
	}
	return nil
}

// checkAdminListen requires a loopback address: the admin API can replace the
// whole configuration and must never be reachable from outside the host.
func checkAdminListen(addr string) error {
	if err := checkHostPort(addr); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(addr)
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("must be a loopback address")
	}
	return nil
}
