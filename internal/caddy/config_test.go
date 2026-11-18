package caddy

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/caddy")

const goldenDir = "../../testdata/caddy"

func baseInput() Input {
	return Input{
		ConsoleHost:     "console.dev.example.com",
		ConsoleUpstream: "127.0.0.1:8080",
	}
}

func goldenInputs() map[string]Input {
	minimal := baseInput()

	apps := baseInput()
	apps.ACMECA = "https://acme-staging-v02.api.letsencrypt.org/directory"
	apps.ACMEEmail = "admin@example.com"
	apps.AccessLogDir = "/var/log/caddy/apps"
	apps.Apps = []App{
		{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com"}},
		{Slug: "shop-staging", HostPort: 20002, Hostnames: []string{"shop-staging.dev.example.com", "staging.example.org"}},
		{Slug: "shop", HostPort: 20003, Hostnames: []string{"shop.dev.example.com"}},
	}

	primary := baseInput()
	primary.AccessLogDir = "/var/log/caddy/apps"
	primary.Apps = []App{
		{
			Slug:      "shop",
			HostPort:  20003,
			Hostnames: []string{"shop.dev.example.com", "example.org", "www.example.org"},
			Primary:   "www.example.org",
		},
		// A primary with nothing else to redirect is served like any other app.
		{Slug: "solo", HostPort: 20004, Hostnames: []string{"solo.dev.example.com"}, Primary: "solo.dev.example.com"},
	}

	stopped := baseInput()
	stopped.AdminListen = "127.0.0.1:2020"
	stopped.Apps = []App{
		{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com"}},
		{
			Slug:      "shop",
			HostPort:  20003,
			Hostnames: []string{"shop.dev.example.com", "www.example.org"},
			Primary:   "www.example.org",
			Stopped:   true,
		},
	}

	// Local development: loopback only, certificates from Caddy's own CA.
	local := baseInput()
	local.ConsoleHost = "console.dev.localhost"
	local.ListenIP = "127.0.0.1"
	local.InternalCA = true
	local.ACMECA = "https://acme-staging-v02.api.letsencrypt.org/directory" // ignored
	local.Apps = []App{{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.localhost"}}}

	return map[string]Input{
		"local":            local,
		"minimal":          minimal,
		"apps":             apps,
		"primary_redirect": primary,
		"stopped":          stopped,
	}
}

func TestGenerateGolden(t *testing.T) {
	for name, in := range goldenInputs() {
		t.Run(name, func(t *testing.T) {
			got, err := Generate(in)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			path := filepath.Join(goldenDir, name+".json")
			if *update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden file (run the test with -update to create it): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("output differs from %s (run the test with -update after reviewing the change)\n--- got ---\n%s", path, got)
			}
		})
	}
}

func TestGenerateIsIndependentOfInputOrder(t *testing.T) {
	in := goldenInputs()["primary_redirect"]
	in.Apps = append(in.Apps, App{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com", "blog.example.net"}})
	want, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	shuffled := in
	shuffled.Apps = slices.Clone(in.Apps)
	slices.Reverse(shuffled.Apps)
	for i := range shuffled.Apps {
		shuffled.Apps[i].Hostnames = slices.Clone(shuffled.Apps[i].Hostnames)
		slices.Reverse(shuffled.Apps[i].Hostnames)
	}
	snapshot := slices.Clone(shuffled.Apps)
	firstHostnames := slices.Clone(shuffled.Apps[0].Hostnames)

	got, err := Generate(shuffled)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output depends on the input order\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if !reflect.DeepEqual(shuffled.Apps, snapshot) || !slices.Equal(shuffled.Apps[0].Hostnames, firstHostnames) {
		t.Error("Generate modified its input")
	}
}

func TestGenerateNormalizesCase(t *testing.T) {
	lower := baseInput()
	lower.Apps = []App{{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com", "example.org"}, Primary: "example.org"}}
	upper := baseInput()
	upper.ConsoleHost = "Console.Dev.Example.COM"
	upper.Apps = []App{{Slug: "blog", HostPort: 20001, Hostnames: []string{"Blog.dev.example.com", "EXAMPLE.org"}, Primary: "Example.Org"}}

	want, err := Generate(lower)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, err := Generate(upper)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("hostnames were not lowercased:\n%s", got)
	}
}

// decoded mirrors the parts of the generated configuration the tests inspect.
type decoded struct {
	Admin struct {
		Listen string `json:"listen"`
	} `json:"admin"`
	Apps struct {
		HTTP struct {
			Servers map[string]struct {
				Listen []string `json:"listen"`
				Routes []struct {
					Match []struct {
						Host []string `json:"host"`
					} `json:"match"`
					Handle []struct {
						Handler    string `json:"handler"`
						StatusCode int    `json:"status_code"`
					} `json:"handle"`
					Terminal bool `json:"terminal"`
				} `json:"routes"`
			} `json:"servers"`
		} `json:"http"`
	} `json:"apps"`
}

// Only requests for a hostname in the input may be proxied or get a
// certificate: Caddy manages certificates for exactly the names found in host
// matchers, so every proxying route must be bound to known hostnames and the
// only route without a matcher must be the final 404.
func TestGenerateServesOnlyKnownHosts(t *testing.T) {
	for name, in := range goldenInputs() {
		t.Run(name, func(t *testing.T) {
			out, err := Generate(in)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			var cfg decoded
			if err := json.Unmarshal(out, &cfg); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}

			want := []string{in.ConsoleHost}
			for _, app := range in.Apps {
				want = append(want, app.Hostnames...)
			}
			slices.Sort(want)

			https := cfg.Apps.HTTP.Servers["https"]
			if !slices.Equal(https.Listen, []string{in.ListenIP + ":443"}) {
				t.Errorf("https listen = %v", https.Listen)
			}
			var got []string
			for i, route := range https.Routes {
				last := i == len(https.Routes)-1
				if last {
					if len(route.Match) != 0 || len(route.Handle) != 1 || route.Handle[0].StatusCode != 404 {
						t.Errorf("the last route must be the catch-all 404: %+v", route)
					}
					continue
				}
				if len(route.Match) != 1 || len(route.Match[0].Host) == 0 {
					t.Errorf("route %d has no host matcher: %+v", i, route)
					continue
				}
				if !route.Terminal {
					t.Errorf("route %d is not terminal", i)
				}
				got = append(got, route.Match[0].Host...)
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("matched hosts = %v, want %v", got, want)
			}

			plain := cfg.Apps.HTTP.Servers["http"]
			if !slices.Equal(plain.Listen, []string{in.ListenIP + ":80"}) {
				t.Errorf("http listen = %v", plain.Listen)
			}
			if len(plain.Routes) != 1 || len(plain.Routes[0].Match) != 0 || plain.Routes[0].Handle[0].StatusCode != 404 {
				t.Errorf("the http server must only hold the catch-all 404: %+v", plain.Routes)
			}
			if len(cfg.Apps.HTTP.Servers) != 2 {
				t.Errorf("unexpected servers: %v", cfg.Apps.HTTP.Servers)
			}
		})
	}
}

// Caddy routes log entries to a log by logger name prefix. The access log of
// one app must not swallow the entries of another app whose slug merely
// starts with the same characters (shop / shop-staging).
func TestGenerateAccessLoggersArePrefixFree(t *testing.T) {
	in := baseInput()
	in.AccessLogDir = "/var/log/caddy/apps"
	in.Apps = []App{
		{Slug: "shop", HostPort: 20001, Hostnames: []string{"shop.dev.example.com"}},
		{Slug: "shop-staging", HostPort: 20002, Hostnames: []string{"shop-staging.dev.example.com"}},
		{Slug: "shop2", HostPort: 20003, Hostnames: []string{"shop2.dev.example.com"}},
	}
	out, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var cfg struct {
		Logging struct {
			Logs map[string]struct {
				Include []string `json:"include"`
				Writer  struct {
					Filename string `json:"filename"`
				} `json:"writer"`
			} `json:"logs"`
		} `json:"logging"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	var loggers []string
	for name, log := range cfg.Logging.Logs {
		if name == "default" {
			continue
		}
		if len(log.Include) != 1 {
			t.Fatalf("log %s: include = %v", name, log.Include)
		}
		loggers = append(loggers, log.Include[0])
	}
	if len(loggers) != len(in.Apps) {
		t.Fatalf("loggers = %v", loggers)
	}
	for _, a := range loggers {
		for _, b := range loggers {
			if a != b && strings.HasPrefix(a, b) {
				t.Errorf("logger %q would also be written to the log of %q", a, b)
			}
		}
	}
	if got := cfg.Logging.Logs["access-shop-staging"].Writer.Filename; got != "/var/log/caddy/apps/shop-staging.log" {
		t.Errorf("filename = %q", got)
	}
}

func TestStoppedPageHasNoPlaceholders(t *testing.T) {
	if strings.ContainsAny(stoppedPage, "{}") {
		t.Error("the stopped page contains braces, which Caddy would treat as placeholders")
	}
}

func TestGenerateAdminListen(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"", DefaultAdminListen},
		{"127.0.0.1:2020", "127.0.0.1:2020"},
		{"[::1]:2019", "[::1]:2019"},
		{"localhost:2019", "localhost:2019"},
	} {
		in := baseInput()
		in.AdminListen = tc.listen
		out, err := Generate(in)
		if err != nil {
			t.Fatalf("Generate(%q): %v", tc.listen, err)
		}
		var cfg decoded
		if err := json.Unmarshal(out, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Admin.Listen != tc.want {
			t.Errorf("admin listen = %q, want %q", cfg.Admin.Listen, tc.want)
		}
	}
}

func TestGenerateRejectsInvalid(t *testing.T) {
	app := func(mutate func(*App)) Input {
		in := baseInput()
		a := App{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com"}}
		mutate(&a)
		in.Apps = []App{a}
		return in
	}
	two := func(a, b App) Input {
		in := baseInput()
		in.Apps = []App{a, b}
		return in
	}
	with := func(mutate func(*Input)) Input {
		in := baseInput()
		mutate(&in)
		return in
	}

	cases := map[string]struct {
		in   Input
		want string
	}{
		"missing console host":     {with(func(in *Input) { in.ConsoleHost = "" }), "console host is required"},
		"console host with port":   {with(func(in *Input) { in.ConsoleHost = "console.dev.example.com:443" }), "console host"},
		"missing console upstream": {with(func(in *Input) { in.ConsoleUpstream = "" }), "console upstream is required"},
		"console upstream no port": {with(func(in *Input) { in.ConsoleUpstream = "127.0.0.1" }), "must be host:port"},
		"console upstream port":    {with(func(in *Input) { in.ConsoleUpstream = "127.0.0.1:99999" }), "invalid port"},
		"public admin listen":      {with(func(in *Input) { in.AdminListen = "0.0.0.0:2019" }), "must be a loopback address"},
		"admin listen all ifaces":  {with(func(in *Input) { in.AdminListen = ":2019" }), "admin listen"},
		"listen IP not an IP":      {with(func(in *Input) { in.ListenIP = "localhost" }), "listen IP"},
		"ACME CA not a URL":        {with(func(in *Input) { in.ACMECA = "acme-staging" }), "must be an http(s) URL"},
		"ACME email":               {with(func(in *Input) { in.ACMEEmail = "nobody" }), "ACME email"},
		"relative access log dir":  {with(func(in *Input) { in.AccessLogDir = "logs" }), "must be an absolute path"},
		"empty slug":               {app(func(a *App) { a.Slug = "" }), "slug is required"},
		"slug with a dot":          {app(func(a *App) { a.Slug = "my.app" }), "slug must be a lowercase DNS label"},
		"slug with a path":         {app(func(a *App) { a.Slug = "../etc" }), "slug must be a lowercase DNS label"},
		"uppercase slug":           {app(func(a *App) { a.Slug = "Blog" }), "slug must be a lowercase DNS label"},
		"port zero":                {app(func(a *App) { a.HostPort = 0 }), "host port 0 is out of range"},
		"port too large":           {app(func(a *App) { a.HostPort = 70000 }), "host port 70000 is out of range"},
		"no hostnames":             {app(func(a *App) { a.Hostnames = nil }), "at least one hostname is required"},
		"empty hostname":           {app(func(a *App) { a.Hostnames = []string{""} }), "fully qualified hostname"},
		"wildcard hostname":        {app(func(a *App) { a.Hostnames = []string{"*.example.org"} }), `invalid label "*"`},
		"hostname with a port":     {app(func(a *App) { a.Hostnames = []string{"example.org:8443"} }), "invalid label"},
		"hostname placeholder":     {app(func(a *App) { a.Hostnames = []string{"{http.request.host}.example.org"} }), "invalid label"},
		"hostname trailing dot":    {app(func(a *App) { a.Hostnames = []string{"example.org."} }), "invalid label"},
		"single label hostname":    {app(func(a *App) { a.Hostnames = []string{"localhost"} }), "fully qualified hostname"},
		"IP address hostname":      {app(func(a *App) { a.Hostnames = []string{"203.0.113.10"} }), "not an IP address"},
		"duplicate within an app":  {app(func(a *App) { a.Hostnames = []string{"example.org", "EXAMPLE.org"} }), `hostname "example.org" is already served by app "blog"`},
		"hostname is the console":  {app(func(a *App) { a.Hostnames = []string{"console.dev.example.com"} }), "is already served by the console"},
		"primary not a hostname":   {app(func(a *App) { a.Primary = "www.example.org" }), `primary "www.example.org" is not one of its hostnames`},
		"duplicate slug": {
			two(
				App{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com"}},
				App{Slug: "blog", HostPort: 20002, Hostnames: []string{"blog2.dev.example.com"}},
			),
			"duplicate slug",
		},
		"duplicate port": {
			two(
				App{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com"}},
				App{Slug: "shop", HostPort: 20001, Hostnames: []string{"shop.dev.example.com"}},
			),
			`host port 20001 is already used by app "blog"`,
		},
		"duplicate hostname across apps": {
			two(
				App{Slug: "blog", HostPort: 20001, Hostnames: []string{"blog.dev.example.com", "example.org"}},
				App{Slug: "shop", HostPort: 20002, Hostnames: []string{"shop.dev.example.com", "example.org"}},
			),
			`app "shop": hostname "example.org" is already served by app "blog"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := Generate(tc.in)
			if err == nil {
				t.Fatalf("expected an error, got:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestGenerateReportsAllProblems(t *testing.T) {
	in := Input{Apps: []App{{Slug: "blog"}}}
	_, err := Generate(in)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"console host is required", "console upstream is required", "host port 0 is out of range", "at least one hostname is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
