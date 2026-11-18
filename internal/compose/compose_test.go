package compose

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const fixtures = "../../testdata/compose"

func loadProject(t *testing.T, name string) Project {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtures, name+".config.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseProject(data)
	if err != nil {
		t.Fatalf("ParseProject(%s): %v", name, err)
	}
	return p
}

func checkGolden(t *testing.T, file string, got []byte) {
	t.Helper()
	path := filepath.Join(fixtures, file)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch.\n--- got ---\n%s\n--- want ---\n%s", file, got, want)
	}
}

func TestParseProject(t *testing.T) {
	p := loadProject(t, "rails-postgres")
	if p.Name != "paas-blog" {
		t.Errorf("Name = %q", p.Name)
	}
	if got := p.ServiceNames(); !reflect.DeepEqual(got, []string{"db", "migrate", "web", "worker"}) {
		t.Errorf("ServiceNames = %v", got)
	}
	if !p.NeedsBuild() || !p.NeedsPull() {
		t.Errorf("NeedsBuild = %v, NeedsPull = %v, want both true", p.NeedsBuild(), p.NeedsPull())
	}
	if p.Services["db"].DeployMemoryLimit() != "1073741824" || p.Services["web"].DeployMemoryLimit() != "" {
		t.Error("DeployMemoryLimit mismatch")
	}
	if p.Volumes["pgdata"].Name != "paas-blog_pgdata" {
		t.Errorf("Volumes = %+v", p.Volumes)
	}

	imageOnly := loadProject(t, "image-only")
	if imageOnly.NeedsBuild() || !imageOnly.NeedsPull() {
		t.Error("image-only project must pull and not build")
	}
}

func TestParseProjectErrors(t *testing.T) {
	_, err := ParseProject([]byte(`{"services": {"web": SECRET_VALUE`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SECRET_VALUE") {
		t.Error("the error must not quote the (possibly secret) input")
	}
	if _, err := ParseProject([]byte(`{"name": "x", "services": {}}`)); err == nil {
		t.Error("a project without services must be rejected")
	}
}

func TestOverrideGolden(t *testing.T) {
	cases := []struct {
		name   string
		golden string
		params OverrideParams
	}{
		{
			name:   "rails-postgres",
			golden: "rails-postgres.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 3000, HostPort: 20001,
				EnvFile:       "/var/lib/paas/apps/blog/env",
				DefaultMemory: "512m",
				ProjectName:   "paas-blog",
				ReleaseTag:    "r42",
			},
		},
		{
			name:   "image-only",
			golden: "image-only.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 80, HostPort: 20002,
				EnvFile:       "/var/lib/paas/apps/static/env",
				DefaultMemory: "512m",
			},
		},
		{
			// App-level settings win over the repository's own limits, in
			// whichever form the repository wrote them.
			name:   "image-only",
			golden: "image-only.memory.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 80, HostPort: 20002,
				EnvFile:       "/var/lib/paas/apps/static/env",
				DefaultMemory: "512m",
				AppMemory:     "256m",
				ServiceMemory: map[string]string{"cache": "96m"},
			},
		},
		{
			name:   "rails-postgres",
			golden: "rails-postgres.memory.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 3000, HostPort: 20001,
				EnvFile:       "/var/lib/paas/apps/blog/env",
				DefaultMemory: "512m",
				ServiceMemory: map[string]string{"db": "2g", "web": "1g"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			tc.params.Project = loadProject(t, tc.name)
			got, err := Override(tc.params)
			if err != nil {
				t.Fatalf("Override: %v", err)
			}
			again, _ := Override(tc.params)
			if string(got) != string(again) {
				t.Error("Override is not deterministic")
			}
			checkGolden(t, tc.golden, got)
		})
	}
}

func TestReleaseImages(t *testing.T) {
	params := OverrideParams{Project: loadProject(t, "rails-postgres"), ProjectName: "paas-blog", ReleaseTag: "r42"}
	want := []string{"paas-blog-migrate:r42", "paas-blog-web:r42", "paas-blog-worker:r42"}
	if got := params.ReleaseImages(); !reflect.DeepEqual(got, want) {
		t.Errorf("ReleaseImages = %v, want %v", got, want)
	}
	params.ReleaseTag = ""
	if got := params.ReleaseImages(); got != nil {
		t.Errorf("without a tag no images are assigned, got %v", got)
	}
	if got := ReleaseImage("paas-Blog", "Web_App", "r1"); got != "paas-blog-web_app:r1" {
		t.Errorf("ReleaseImage = %q (image names must be lowercase)", got)
	}
}

func TestOverrideErrors(t *testing.T) {
	p := loadProject(t, "image-only")
	base := OverrideParams{Project: p, WebService: "web", WebPort: 80, HostPort: 20000, EnvFile: "/env", DefaultMemory: "512m"}

	missing := base
	missing.WebService = "app"
	if _, err := Override(missing); err == nil || !strings.Contains(err.Error(), `"app"`) {
		t.Errorf("missing web service: err = %v", err)
	}
	badPort := base
	badPort.WebPort = 0
	if _, err := Override(badPort); err == nil {
		t.Error("an invalid port must be rejected")
	}
	noEnv := base
	noEnv.EnvFile = ""
	if _, err := Override(noEnv); err == nil {
		t.Error("a missing env file must be rejected")
	}
}

func TestOverrideQuotesUnusualNames(t *testing.T) {
	p := Project{Services: map[string]Service{
		"web":             {Image: "example/app"},
		"yes":             {Image: "example/app"},
		`we"ird: #name`:   {Image: "example/app"},
		"123":             {Image: "example/app"},
		"with space":      {Image: "example/app"},
		"null":            {Image: "example/app"},
		"key-with-dashes": {Image: "example/app"},
	}}
	got, err := Override(OverrideParams{
		Project: p, WebService: "web", WebPort: 80, HostPort: 20000,
		EnvFile: `/var/lib/paas/apps/a b/env`, DefaultMemory: "512m",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	for _, want := range []string{`  "yes":`, `  "we\"ird: #name":`, `  "123":`, `  "null":`, `      - "/var/lib/paas/apps/a b/env"`} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestPolicyGolden(t *testing.T) {
	p := loadProject(t, "violations")
	params := PolicyParams{
		ReleaseDir: "/var/lib/paas/apps/risky/releases/7",
		WebService: "web", WebPort: 3000, HostPort: 20001,
	}

	render := func(r PolicyResult) []byte {
		var b strings.Builder
		b.WriteString("violations:\n")
		for _, v := range r.Violations {
			b.WriteString("  " + v.String() + "\n")
		}
		b.WriteString("waived:\n")
		for _, v := range r.Waived {
			b.WriteString("  " + v.String() + "\n")
		}
		return []byte(b.String())
	}

	strict := CheckPolicy(p, params)
	if strict.OK() {
		t.Fatal("the project must be rejected")
	}
	checkGolden(t, "violations.policy.txt", render(strict))

	params.Relax = true
	relaxed := CheckPolicy(p, params)
	checkGolden(t, "violations.policy.relaxed.txt", render(relaxed))
	for _, v := range relaxed.Violations {
		if v.Rule != RulePortExposure {
			t.Errorf("only port exposure may survive relaxing, got %s", v)
		}
	}
	if relaxed.OK() {
		t.Error("published ports must block the deploy even when the policy is relaxed")
	}

	err := &PolicyError{Violations: strict.Violations}
	if !strings.Contains(err.Error(), "policy check failed") || !strings.Contains(err.Error(), RulePrivileged) {
		t.Errorf("PolicyError = %q", err.Error())
	}
}

func TestPolicyAcceptsGeneratedOverride(t *testing.T) {
	// The fixture is the repository file alone; simulate the merged result.
	p := loadProject(t, "rails-postgres")
	web := p.Services["web"]
	web.Ports = []Port{{HostIP: "127.0.0.1", Target: 3000, Published: "20001", Protocol: "tcp"}}
	p.Services["web"] = web
	db := p.Services["db"]
	db.Ports = nil
	p.Services["db"] = db

	result := CheckPolicy(p, PolicyParams{
		ReleaseDir: "/var/lib/paas/apps/blog/releases/42",
		WebService: "web", WebPort: 3000, HostPort: 20001,
	})
	if !result.OK() || len(result.Waived) != 0 {
		t.Errorf("unexpected findings: %+v", result)
	}
}

func TestPolicyWebServiceMissing(t *testing.T) {
	p := loadProject(t, "image-only")
	result := CheckPolicy(p, PolicyParams{ReleaseDir: "/release", WebService: "app", WebPort: 80, HostPort: 20000, Relax: true})
	var found bool
	for _, v := range result.Violations {
		if v.Rule == RuleWebServiceMissing {
			found = true
			if !strings.Contains(v.Message, "cache, web") {
				t.Errorf("message should list the available services: %q", v.Message)
			}
		}
	}
	if !found {
		t.Errorf("missing web service not reported (even when relaxed): %+v", result)
	}
}

func TestPolicyWebBindingMustMatchExactly(t *testing.T) {
	base := Port{HostIP: "127.0.0.1", Target: 3000, Published: "20001", Protocol: "tcp"}
	cases := map[string]func(*Port){
		"all interfaces":  func(p *Port) { p.HostIP = "0.0.0.0" },
		"no host ip":      func(p *Port) { p.HostIP = "" },
		"ipv6 loopback":   func(p *Port) { p.HostIP = "::1" },
		"other host port": func(p *Port) { p.Published = "20002" },
		"other target":    func(p *Port) { p.Target = 3001 },
		"udp":             func(p *Port) { p.Protocol = "udp" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			port := base
			mutate(&port)
			p := Project{Services: map[string]Service{"web": {Image: "x", Ports: []Port{port}}}}
			result := CheckPolicy(p, PolicyParams{ReleaseDir: "/r", WebService: "web", WebPort: 3000, HostPort: 20001})
			if result.OK() {
				t.Errorf("port %+v must be rejected", port)
			}
		})
	}
	ok := Project{Services: map[string]Service{"web": {Image: "x", Ports: []Port{base}}}}
	if !CheckPolicy(ok, PolicyParams{ReleaseDir: "/r", WebService: "web", WebPort: 3000, HostPort: 20001}).OK() {
		t.Error("the exact loopback binding must be accepted")
	}
}

func TestPolicyFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "release")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{filepath.Join(release, "config"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink committed to the repository that points out of the checkout.
	if err := os.Symlink(outside, filepath.Join(release, "escape")); err != nil {
		t.Fatal(err)
	}
	p := Project{Services: map[string]Service{"web": {Image: "x", Volumes: []Mount{
		{Type: "bind", Source: filepath.Join(release, "config"), Target: "/config"},
		{Type: "bind", Source: filepath.Join(release, "escape"), Target: "/escape"},
		{Type: "bind", Source: filepath.Join(release, "not-yet-created"), Target: "/new"},
	}}}}
	result := CheckPolicy(p, PolicyParams{ReleaseDir: release, WebService: "web", WebPort: 80, HostPort: 20000})
	if len(result.Violations) != 1 || result.Violations[0].Rule != RuleBindOutside ||
		!strings.Contains(result.Violations[0].Message, "escape") {
		t.Errorf("violations = %+v", result.Violations)
	}
}

func TestDetectFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := DetectFile(dir, ""); err == nil {
		t.Error("expected an error when no Compose file exists")
	}
	write("docker-compose.yml")
	if got, err := DetectFile(dir, ""); err != nil || got != "docker-compose.yml" {
		t.Errorf("DetectFile = %q, %v", got, err)
	}
	write("compose.yaml")
	if got, _ := DetectFile(dir, ""); got != "compose.yaml" {
		t.Errorf("compose.yaml must take precedence, got %q", got)
	}
	write("deploy/compose.prod.yaml")
	if got, err := DetectFile(dir, "deploy/compose.prod.yaml"); err != nil || got != "deploy/compose.prod.yaml" {
		t.Errorf("configured path: %q, %v", got, err)
	}
	if got, err := DetectFile(dir, "./deploy/../compose.yaml"); err != nil || got != "compose.yaml" {
		t.Errorf("configured path must be cleaned: %q, %v", got, err)
	}
	for _, bad := range []string{"../compose.yaml", "/etc/passwd", "missing.yaml", "deploy", ".."} {
		if _, err := DetectFile(dir, bad); err == nil {
			t.Errorf("DetectFile(%q) must fail", bad)
		}
	}
	// A symlinked Compose file could point outside the repository.
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectFile(dir, "link.yaml"); err == nil {
		t.Error("a symlinked Compose file must be rejected")
	}
}

func TestInvocationArgs(t *testing.T) {
	inv := Invocation{
		ProjectName: "paas-blog",
		Files:       []string{"/r/compose.yaml", "/r/paas.override.yaml"},
		EnvFile:     "/apps/blog/env",
	}
	got := inv.Args("up", "-d", "--wait")
	want := []string{"compose", "-p", "paas-blog", "-f", "/r/compose.yaml", "-f", "/r/paas.override.yaml",
		"--env-file", "/apps/blog/env", "up", "-d", "--wait"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %v", got)
	}
	bare := Invocation{ProjectName: "paas-blog"}.Args("ps")
	if !reflect.DeepEqual(bare, []string{"compose", "-p", "paas-blog", "ps"}) {
		t.Errorf("Args = %v", bare)
	}
}
