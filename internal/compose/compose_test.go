package compose

import (
	"flag"
	"maps"
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
	if p.Name != "koroku-blog" {
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
	if p.Volumes["pgdata"].Name != "koroku-blog_pgdata" {
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
				EnvFile:       "/var/lib/koroku/apps/blog/env",
				DefaultMemory: "512m",
				ProjectName:   "koroku-blog",
				ReleaseTag:    "r42",
			},
		},
		{
			name:   "image-only",
			golden: "image-only.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 80, HostPort: 20002,
				EnvFile:       "/var/lib/koroku/apps/static/env",
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
				EnvFile:       "/var/lib/koroku/apps/static/env",
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
				EnvFile:       "/var/lib/koroku/apps/blog/env",
				DefaultMemory: "512m",
				ServiceMemory: map[string]string{"db": "2g", "web": "1g"},
			},
		},
		{
			// Production: every service reserves half its limit, the
			// deploy.resources form included, and is killed last.
			name:   "rails-postgres",
			golden: "rails-postgres.production.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 3000, HostPort: 20001,
				EnvFile:       "/var/lib/koroku/apps/blog/env",
				DefaultMemory: "512m",
				ProjectName:   "koroku-blog",
				ReleaseTag:    "r42",
				Production:    true,
			},
		},
		{
			// App settings choose the reservation; one above the limit is
			// cut down to it, "0" reserves nothing.
			name:   "rails-postgres",
			golden: "rails-postgres.reservation.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 3000, HostPort: 20001,
				EnvFile:            "/var/lib/koroku/apps/blog/env",
				DefaultMemory:      "512m",
				Production:         true,
				AppReservation:     "128m",
				ServiceReservation: map[string]string{"db": "4g", "migrate": "0"},
			},
		},
		{
			// The repository's own oom_score_adj and reservations do not
			// survive: production gets koroku's values in the forms the
			// repository used, a too small default is dropped.
			name:   "reservations",
			golden: "reservations.production.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 3000, HostPort: 20003,
				EnvFile:       "/var/lib/koroku/apps/shop/env",
				DefaultMemory: "512m",
				Production:    true,
			},
		},
		{
			// Staging loses the reservations the repository set.
			name:   "reservations",
			golden: "reservations.staging.override.yaml",
			params: OverrideParams{
				WebService: "web", WebPort: 3000, HostPort: 20003,
				EnvFile:       "/var/lib/koroku/apps/shop/env",
				DefaultMemory: "512m",
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
	params := OverrideParams{Project: loadProject(t, "rails-postgres"), ProjectName: "koroku-blog", ReleaseTag: "r42"}
	want := []string{"koroku-blog-migrate:r42", "koroku-blog-web:r42", "koroku-blog-worker:r42"}
	if got := params.ReleaseImages(); !reflect.DeepEqual(got, want) {
		t.Errorf("ReleaseImages = %v, want %v", got, want)
	}
	params.ReleaseTag = ""
	if got := params.ReleaseImages(); got != nil {
		t.Errorf("without a tag no images are assigned, got %v", got)
	}
	if got := ReleaseImage("koroku-Blog", "Web_App", "r1"); got != "koroku-blog-web_app:r1" {
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
		EnvFile: `/var/lib/koroku/apps/a b/env`, DefaultMemory: "512m",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	for _, want := range []string{`  "yes":`, `  "we\"ird: #name":`, `  "123":`, `  "null":`, `      - "/var/lib/koroku/apps/a b/env"`} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestPolicyGolden(t *testing.T) {
	p := loadProject(t, "violations")
	params := PolicyParams{
		ReleaseDir: "/var/lib/koroku/apps/risky/releases/7",
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
		if v.Rule != RulePortExposure && v.Rule != RuleStopGracePeriod {
			t.Errorf("only port exposure and the stop grace limit may survive relaxing, got %s", v)
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

// TestPolicyHostileProject covers the ways a Compose file reaches the host
// or another app besides the ones the original list names. Compose itself
// accepts every one of them (checked with a real `docker compose config`).
func TestPolicyHostileProject(t *testing.T) {
	p := loadProject(t, "hostile")
	params := PolicyParams{
		ReleaseDir:  "/var/lib/koroku/apps/evil/releases/9",
		ProjectName: "koroku-evil",
		EnvFile:     "/var/lib/koroku/apps/evil/env",
		WebService:  "web", WebPort: 3000, HostPort: 20001,
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
	checkGolden(t, "hostile.policy.txt", render(strict))

	// What is fine must not be reported: the platform's own env file, files
	// inside the checkout, a namespace shared with a service of the same
	// app, a default capability, remote build contexts, an NFS volume.
	text := string(render(strict))
	for _, allowed := range []string{
		"apps/evil/env ", "config/app.env", "service:web", "NET_BIND_SERVICE", "no-new-privileges:true",
		"example.com/context", "docker-image://", "good-builder", `"data"`, `"nfs"`, `"default"`, `"ok"`,
	} {
		if strings.Contains(text, allowed) {
			t.Errorf("%q must not be reported:\n%s", allowed, text)
		}
	}

	// Every one of these is a deliberate choice the owner can make per app.
	params.Relax = true
	relaxed := CheckPolicy(p, params)
	if !relaxed.OK() || len(relaxed.Waived) != len(strict.Violations) {
		t.Errorf("with the relax flag: %d violations left, %d waived of %d", len(relaxed.Violations), len(relaxed.Waived), len(strict.Violations))
	}

	// Without a project name the ownership rules are skipped, not misfired.
	params.Relax, params.ProjectName = false, ""
	for _, v := range CheckPolicy(p, params).Violations {
		if v.Rule == RuleForeignVolume || v.Rule == RuleForeignNetwork {
			t.Errorf("ownership rule fired without a project name: %s", v)
		}
	}
}

func TestEnvFileUnmarshal(t *testing.T) {
	p, err := ParseProject([]byte(`{"services": {"web": {"image": "x", "env_file": ["/a/.env", {"path": "/b/.env", "required": true}]}}}`))
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	files := p.Services["web"].EnvFiles
	if len(files) != 2 || files[0].Path != "/a/.env" || files[1].Path != "/b/.env" {
		t.Errorf("EnvFiles = %+v", files)
	}
}

func TestDetectFileRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	release, outside := filepath.Join(root, "release"), filepath.Join(root, "outside")
	for _, dir := range []string{release, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// deploy/ is a symlink that leaves the checkout.
	if err := os.Symlink(outside, filepath.Join(release, "deploy")); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectFile(release, "deploy/compose.yaml"); err == nil {
		t.Error("a Compose file reached through a symlinked directory must be rejected")
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
		ReleaseDir: "/var/lib/koroku/apps/blog/releases/42",
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
		{Type: "bind", Source: filepath.Join(release, "config"), Target: "/config", ReadOnly: true},
		{Type: "bind", Source: filepath.Join(release, "escape"), Target: "/escape", ReadOnly: true},
		{Type: "bind", Source: filepath.Join(release, "not-yet-created"), Target: "/new", ReadOnly: true},
		// The link exists but the rest of the path does not: Docker would
		// create it at the link's target, outside the checkout.
		{Type: "bind", Source: filepath.Join(release, "escape", "made", "by-docker"), Target: "/made", ReadOnly: true},
	}}}}
	result := CheckPolicy(p, PolicyParams{ReleaseDir: release, WebService: "web", WebPort: 80, HostPort: 20000})
	if len(result.Violations) != 2 {
		t.Fatalf("violations = %+v", result.Violations)
	}
	for _, v := range result.Violations {
		if v.Rule != RuleBindOutside || !strings.Contains(v.Message, "escape") {
			t.Errorf("unexpected violation: %s", v)
		}
	}

	// A bind inside the checkout must be read-only: with write access a
	// container could swap another bind source for a symlink.
	writable := Project{Services: map[string]Service{"web": {Image: "x", Volumes: []Mount{
		{Type: "bind", Source: release, Target: "/app"},
		{Type: "bind", Source: filepath.Join(release, "config"), Target: "/config", ReadOnly: true},
	}}}}
	result = CheckPolicy(writable, PolicyParams{ReleaseDir: release, WebService: "web", WebPort: 80, HostPort: 20000})
	if len(result.Violations) != 1 || result.Violations[0].Rule != RuleBindWritable {
		t.Errorf("writable bind: violations = %+v", result.Violations)
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
		ProjectName: "koroku-blog",
		Files:       []string{"/r/compose.yaml", "/r/koroku.override.yaml"},
		EnvFile:     "/apps/blog/env",
	}
	got := inv.Args("up", "-d", "--wait")
	want := []string{"compose", "-p", "koroku-blog", "-f", "/r/compose.yaml", "-f", "/r/koroku.override.yaml",
		"--env-file", "/apps/blog/env", "up", "-d", "--wait"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %v", got)
	}
	bare := Invocation{ProjectName: "koroku-blog"}.Args("ps")
	if !reflect.DeepEqual(bare, []string{"compose", "-p", "koroku-blog", "ps"}) {
		t.Errorf("Args = %v", bare)
	}
}

func TestParseMemory(t *testing.T) {
	cases := map[string]int64{
		"512m": 512 << 20, "1g": 1 << 30, "64G": 64 << 30, "100k": 100 << 10, "2048": 2048,
		"134217728": 134217728, "256mb": 256 << 20, " 1g ": 1 << 30,
	}
	for in, want := range cases {
		if got, ok := ParseMemory(in); !ok || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "lots", "-1", "0", "1.5g", "g"} {
		if _, ok := ParseMemory(bad); ok {
			t.Errorf("ParseMemory(%q) must fail", bad)
		}
	}
}

// TestRepositoryCannotRaiseItsOwnMemoryLimit: the configured default is a
// ceiling for limits a Compose file sets itself; only the app settings raise it.
func TestRepositoryCannotRaiseItsOwnMemoryLimit(t *testing.T) {
	p := Project{Services: map[string]Service{
		"web":   {Image: "x", MemLimit: "68719476736"},                                                       // 64g
		"small": {Image: "x", MemLimit: "134217728"},                                                         // 128m
		"db":    {Image: "x", Deploy: &Deploy{Resources: &Resources{Limits: &Limits{Memory: "2147483648"}}}}, // 2g
		"none":  {Image: "x"},
	}}
	base := OverrideParams{Project: p, WebService: "web", WebPort: 80, HostPort: 20000, EnvFile: "/env", DefaultMemory: "512m"}
	got, err := Override(base)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	section := func(name string) string {
		start := strings.Index(text, `  "`+name+`":`)
		rest := text[start+1:]
		if end := strings.Index(rest, "\n  \""); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	if !strings.Contains(section("web"), `mem_limit: "512m"`) {
		t.Errorf("a 64g limit from the repository must be capped:\n%s", section("web"))
	}
	if strings.Contains(section("small"), "mem_limit") {
		t.Errorf("a limit below the default must be left alone:\n%s", section("small"))
	}
	if !strings.Contains(section("db"), `memory: "512m"`) {
		t.Errorf("a deploy limit above the default must be capped in the same form:\n%s", section("db"))
	}
	if !strings.Contains(section("none"), `mem_limit: "512m"`) {
		t.Errorf("a service without a limit gets the default:\n%s", section("none"))
	}

	// The app settings are how a limit is raised.
	base.ServiceMemory = map[string]string{"db": "2g"}
	got, _ = Override(base)
	if !strings.Contains(string(got), `memory: "2g"`) {
		t.Errorf("the app setting must win:\n%s", got)
	}
}

func TestMemswapLimitFollowsARaisedLimit(t *testing.T) {
	p := Project{Services: map[string]Service{
		// The repository forbids swap with memswap_limit equal to mem_limit.
		"web":    {Image: "x", MemLimit: "536870912", MemswapLimit: "536870912"},
		"worker": {Image: "x", MemLimit: "536870912", MemswapLimit: "4294967296"}, // allows swap up to 4g
		"db":     {Image: "x", MemswapLimit: "-1"},
	}}
	got, err := Override(OverrideParams{
		Project: p, WebService: "web", WebPort: 80, HostPort: 20000, EnvFile: "/env",
		DefaultMemory: "512m", AppMemory: "1g",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	section := func(name string) string {
		start := strings.Index(text, `  "`+name+`":`)
		rest := text[start+1:]
		if end := strings.Index(rest, "\n  \""); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	if !strings.Contains(section("web"), `memswap_limit: "1g"`) {
		t.Errorf("memswap_limit below the raised limit must follow it:\n%s", section("web"))
	}
	if strings.Contains(section("worker"), "memswap_limit") {
		t.Errorf("a memswap_limit above the limit must be left alone:\n%s", section("worker"))
	}
	if strings.Contains(section("db"), "memswap_limit") {
		t.Errorf("memswap_limit -1 is the policy's business, not the override's:\n%s", section("db"))
	}
}

// TestReservations: what Override reserves is what Reservations reports,
// so the deployment record matches the containers.
func TestReservations(t *testing.T) {
	params := OverrideParams{
		Project:    loadProject(t, "reservations"),
		WebService: "web", WebPort: 3000, HostPort: 20003,
		EnvFile: "/env", DefaultMemory: "512m", Production: true,
	}
	want := map[string]int64{"web": 192 << 20, "db": 256 << 20, "cache": 256 << 20}
	if got := params.Reservations(); !maps.Equal(got, want) {
		t.Errorf("Reservations() = %v, want %v (the 8m sidecar's 4m is below Docker's minimum, the one-shot migrate reserves nothing)", got, want)
	}

	params.AppReservation = "0"
	params.ServiceReservation = map[string]string{"web": "1g", "sidecar": "6m", "migrate": "256m"}
	want = map[string]int64{"web": 384 << 20, "sidecar": 6 << 20}
	if got := params.Reservations(); !maps.Equal(got, want) {
		t.Errorf("Reservations() = %v, want %v (capped at the limit, 0 for none, never a one-shot service)", got, want)
	}

	params.Production = false
	if got := params.Reservations(); len(got) != 0 {
		t.Errorf("a staging app must reserve nothing, got %v", got)
	}
}

// TestOneShot: only a service another one waits on to complete is one-shot.
func TestOneShot(t *testing.T) {
	p := loadProject(t, "rails-postgres")
	for name, want := range map[string]bool{"migrate": true, "db": false, "web": false, "worker": false} {
		if got := p.OneShot(name); got != want {
			t.Errorf("OneShot(%q) = %v, want %v", name, got, want)
		}
	}
}
