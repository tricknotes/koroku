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

	// Rotation of the per-app access logs. A later collector ships them to
	// object storage; these values only bound the local disk usage.
	accessLogRollSizeMB   = 50
	accessLogRollKeep     = 5
	accessLogRollKeepDays = 14
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

	routes := []obj{consoleRoute(in)}
	for _, app := range in.Apps {
		routes = append(routes, appRoutes(app)...)
	}
	// Requests that reach the HTTPS server with a Host header we do not serve.
	routes = append(routes, notFoundRoute())

	httpsServer := obj{
		"listen": []string{":443"},
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
						"listen": []string{":80"},
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
	}
	return errors.Join(errs...)
}

func consoleRoute(in Input) obj {
	return obj{
		"match": []obj{{"host": []string{in.ConsoleHost}}},
		"handle": []obj{{
			"handler":   "reverse_proxy",
			"upstreams": []obj{{"dial": in.ConsoleUpstream}},
			// The console streams deploy logs over SSE: never buffer responses.
			"flush_interval": -1,
		}},
		"terminal": true,
	}
}

// appRoutes returns the routes of one app: an optional redirect of the
// non-primary hostnames, then the route that serves the app.
func appRoutes(app App) []obj {
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

	var handler obj
	if app.Stopped {
		handler = obj{
			"handler":     "static_response",
			"status_code": 503,
			"headers": obj{
				"Cache-Control": []string{"no-store"},
				"Content-Type":  []string{"text/html; charset=utf-8"},
			},
			"body": stoppedPage,
		}
	} else {
		handler = obj{
			"handler":   "reverse_proxy",
			"upstreams": []obj{{"dial": net.JoinHostPort("127.0.0.1", strconv.Itoa(app.HostPort))}},
			// WebSocket and SSE work with the reverse proxy defaults (upgrades
			// are passed through, event streams are flushed immediately).
			"stream_close_delay": streamCloseDelay,
		}
	}
	return append(routes, obj{
		"match":    []obj{{"host": served}},
		"handle":   []obj{handler},
		"terminal": true,
	})
}

func notFoundRoute() obj {
	return obj{"handle": []obj{{"handler": "static_response", "status_code": 404}}}
}

func acmeIssuer(in Input) obj {
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
			"encoder": obj{"format": "json"},
			"include": []string{logger},
		}
	}
	return obj{"logs": logs}
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
