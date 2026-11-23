package compose

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func renderPolicy(r PolicyResult) []byte {
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

// TestPolicyEscapes covers settings that reach the host or another app
// without any of the settings the policy judges by value: a program run on
// the host (provider), the Docker socket (use_api_socket), host directories
// through an overlay volume, host networking and a host key for the build,
// a privileged hook, another app's network and image. The fixture is what
// a real `docker compose config` prints for escape.compose.yaml.
func TestPolicyEscapes(t *testing.T) {
	p := loadProject(t, "escape")
	params := PolicyParams{
		ReleaseDir:  "/var/lib/koroku/apps/evil/releases/10",
		ProjectName: "koroku-evil",
		EnvFile:     "/var/lib/koroku/apps/evil/env",
		WebService:  "web", WebPort: 3000, HostPort: 20001,
	}
	strict := CheckPolicy(p, params)
	checkGolden(t, "escape.policy.txt", renderPolicy(strict))

	// Each service of the fixture is there for one way out; none may pass.
	blocked := map[string]bool{}
	for _, v := range strict.Violations {
		blocked[v.Service] = true
	}
	for _, name := range p.ServiceNames() {
		if !blocked[name] {
			t.Errorf("service %q passes the policy", name)
		}
	}

	params.Relax = true
	if relaxed := CheckPolicy(p, params); !relaxed.OK() || len(relaxed.Waived) != len(strict.Violations) {
		t.Errorf("relaxed: violations %v, waived %d of %d", relaxed.Violations, len(relaxed.Waived), len(strict.Violations))
	}
}

// TestPolicyAllowsOrdinarySettings checks that the list of allowed settings
// covers what ordinary Compose files use. The fixture is what a real
// `docker compose config` prints for allowed.compose.yaml, without koroku's
// override: the published port is the only finding.
func TestPolicyAllowsOrdinarySettings(t *testing.T) {
	p := loadProject(t, "allowed")
	result := CheckPolicy(p, PolicyParams{
		ReleaseDir:  "/var/lib/koroku/apps/blog/releases/43",
		ProjectName: "koroku-blog",
		EnvFile:     "/var/lib/koroku/apps/blog/env",
		WebService:  "web", WebPort: 3000, HostPort: 20001,
	})
	var got []string
	for _, v := range result.Violations {
		got = append(got, v.String())
	}
	want := []string{`[port_exposure] service "web": unexpected published port 0.0.0.0:3000->3000 (only the web service may publish, on loopback)`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("violations:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestUnsupportedSettings(t *testing.T) {
	doc := `{
		"name": "koroku-x",
		"x-top": {"anything": true},
		"models": {"llm": {"model": "ai/model"}},
		"services": {
			"web": {
				"image": "busybox",
				"x-note": {"provider": "ignored"},
				"use_api_socket": false,
				"gpus": [],
				"cgroup_parent": "",
				"provider": null,
				"build": {"context": "/r", "ssh": ["default"], "tags": [], "isolation": ""},
				"post_start": [{"command": ["true"], "privileged": false}, {"command": ["true"], "privileged": true}],
				"pre_stop": [{"command": ["true"], "privileged": true}, {"command": ["true"], "privileged": true}],
				"volumes": [{"type": "bind", "source": "/r/a", "target": "/a", "bind": {"propagation": "rshared", "create_host_path": true}}],
				"deploy": {"resources": {"reservations": {"devices": [{"capabilities": ["gpu"]}], "memory": "1"}}, "placement": {}},
				"logging": "json-file"
			},
			"web-blue": {"image": "busybox", "provider": {"type": "x"}},
			"other": {"image": "busybox", "provider": {"type": "x"}, "future_setting": 1}
		},
		"networks": {"lan": {"name": "koroku-x_lan", "driver_opts": {"parent": "eth0"}}},
		"secrets": {"s": {"driver": "vault"}}
	}`
	got := unsupportedSettings(json.RawMessage(doc), func(service string) bool { return service == "web-blue" })
	want := []unsupported{
		{"", "models"},
		{"", "networks.lan.driver_opts"},
		{"", "secrets.s.driver"},
		{"other", "future_setting"},
		{"other", "provider"},
		{"web", "build.ssh"},
		{"web", "deploy.resources.reservations.devices"},
		{"web", "logging"},
		{"web", "post_start.privileged"},
		{"web", "pre_stop.privileged"},
		{"web", "volumes.bind.propagation"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unsupportedSettings:\n got %v\nwant %v", got, want)
	}
	if got := unsupportedSettings(nil, nil); got != nil {
		t.Errorf("without the raw project: %v", got)
	}
}

func TestPolicyValuesOfAllowedSettings(t *testing.T) {
	check := func(services, rest string) []string {
		t.Helper()
		p, err := ParseProject([]byte(`{"name": "koroku-app", "services": {"web": {"image": "busybox"}, ` + services + `}` + rest + `}`))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, v := range CheckPolicy(p, PolicyParams{ReleaseDir: "/r", ProjectName: "koroku-app", WebService: "web", WebPort: 80, HostPort: 20000}).Violations {
			out = append(out, v.Rule+" "+v.Service)
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		services string
		rest     string
		want     []string
	}{
		{"network of another app", `"a": {"image": "busybox", "network_mode": "koroku-blog_default"}`, "", []string{"foreign_network a"}},
		{"the default bridge", `"a": {"image": "busybox", "network_mode": "bridge"}`, "", []string{"foreign_network a"}},
		{"a service that is not there", `"a": {"image": "busybox", "network_mode": "service:gone", "ipc": "service:gone"}`, "", []string{"foreign_namespace a", "foreign_network a"}},
		{"inside the app", `"a": {"image": "busybox", "network_mode": "service:web", "pid": "service:web", "ipc": "shareable", "cgroup": "private"}, "b": {"image": "busybox", "network_mode": "none", "ipc": "service:a"}`, "", nil},
		{"uts and userns", `"a": {"image": "busybox", "uts": "service:web", "userns_mode": "keep-id"}`, "", []string{"foreign_namespace a", "foreign_namespace a"}},
		{"seccomp profile from a file", `"a": {"image": "busybox", "security_opt": ["seccomp=./allow.json", "no-new-privileges", "no-new-privileges=true", "apparmor=mine"]}`, "", []string{"security_opt a", "security_opt a"}},
		{"another app's image", `"a": {"image": "koroku-blog-web:r41"}, "b": {"image": "docker.io/library/koroku-blog-web@sha256:00"}`, "", []string{"foreign_image a", "foreign_image b"}},
		{"the app's own image", `"a": {"image": "koroku-app-web:r7", "pull_policy": "never"}, "b": {"image": "ghcr.io/octo/koroku-thing:1"}, "c": {"image": "octo/koroku-thing"}`, "", nil},
		{"a built service keeps its name", `"a": {"image": "koroku-blog-web:r41", "build": {"context": "/r"}}`, "", nil},
		{"Dockerfile outside through ..", `"a": {"build": {"context": "/r/app", "dockerfile": "../../etc/koroku/master.key"}}`, "", []string{"file_outside_release a"}},
		{"Dockerfile inside", `"a": {"build": {"context": "/r/app", "dockerfile": "../docker/Dockerfile"}}, "b": {"build": {"context": "https://example.com/octo/app.git", "dockerfile": "../x"}}`, "", nil},
		{"local build cache", `"a": {"build": {"context": "/r", "cache_to": ["type=local,dest=/etc/cron.d"], "cache_from": ["type=registry,ref=ghcr.io/octo/app:cache", "type = local,src=/x"]}}`, "", []string{"unsupported_setting a", "unsupported_setting a"}},
		{"build network", `"a": {"build": {"context": "/r", "network": "koroku-blog_default"}}, "b": {"build": {"context": "/r", "network": "none"}}`, "", []string{"unsupported_setting a"}},
		{"logging elsewhere", `"a": {"image": "busybox", "logging": {"driver": "syslog"}}, "b": {"image": "busybox", "logging": {"driver": "local"}}`, "", []string{"unsupported_setting a"}},
		{"image mount", `"a": {"image": "busybox", "volumes": [{"type": "image", "source": "postgres:17", "target": "/x"}, {"type": "tmpfs", "target": "/t"}]}`, "", []string{"unsupported_setting a"}},
		{"overlay volume", "", `, "volumes": {"v": {"name": "koroku-app_v", "driver_opts": {"type": "overlay", "device": "overlay", "o": "lowerdir=/etc:/usr"}}}`, []string{"volume_host_path "}},
		{"bind volume with a relative device", "", `, "volumes": {"v": {"name": "koroku-app_v", "driver_opts": {"type": "none", "o": "bind", "device": "etc"}}}`, []string{"volume_host_path "}},
		{"block device volume", "", `, "volumes": {"v": {"name": "koroku-app_v", "driver_opts": {"type": "ext4", "device": "/dev/sda1"}}}`, []string{"volume_host_path "}},
		{"nfs and tmpfs volumes", "", `, "volumes": {"v": {"name": "koroku-app_v", "driver": "local", "driver_opts": {"type": "nfs", "o": "addr=203.0.113.20", "device": ":/exports"}}, "t": {"name": "koroku-app_t", "driver_opts": {"type": "tmpfs", "device": "tmpfs"}}}`, nil},
		{"volume plugin", "", `, "volumes": {"v": {"name": "koroku-app_v", "driver": "example/plugin"}}`, []string{"unsupported_setting "}},
		{"macvlan network", "", `, "networks": {"n": {"name": "koroku-app_n", "driver": "macvlan"}, "b": {"name": "koroku-app_b", "driver": "bridge"}}`, []string{"unsupported_setting "}},
	} {
		services := tc.services
		if services == "" {
			services = `"z": {"image": "busybox"}`
		}
		if got := check(services, tc.rest); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestScanFiles(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "other.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		links map[string]string
		want  []string
	}{
		{name: "no include", files: map[string]string{"compose.yaml": "services:\n  web:\n    image: busybox\n    ports: !reset []\n    extends:\n      service: base\n  base:\n    image: busybox\n"}},
		{name: "includes inside the checkout", files: map[string]string{
			"compose.yaml":       "include:\n  - infra/db.yaml\n  - path: [infra/cache.yaml]\nservices:\n  web:\n    extends:\n      file: common/base.yaml\n      service: base\n",
			"infra/db.yaml":      "services:\n  db:\n    image: postgres:17\n",
			"infra/cache.yaml":   "include: [./db.yaml]\nservices:\n  cache:\n    image: valkey/valkey:9\n",
			"common/base.yaml":   "services:\n  base:\n    image: busybox\n",
			"infra/unrelated.md": "include: nothing here is read\n",
		}},
		{name: "env file of an include", files: map[string]string{
			"compose.yaml": "include:\n  - path: inc.yaml\n    env_file: /var/lib/koroku/apps/blog/env\n    project_directory: /\n",
			"inc.yaml":     "services: {}\n",
		}, want: []string{
			"unsupported_setting include in compose.yaml sets env_file, which is not allowed (an included file reads the app's variables like the file that includes it)",
			"unsupported_setting include in compose.yaml sets project_directory, which is not allowed (an included file reads the app's variables like the file that includes it)",
		}},
		{name: "nested include", files: map[string]string{
			"compose.yaml": "include: [a/one.yaml]\n",
			"a/one.yaml":   "include:\n  - path: two.yaml\n    env_file: ../../x\n",
			"a/two.yaml":   "services: {}\n",
		}, want: []string{"unsupported_setting include in a/one.yaml sets env_file, which is not allowed (an included file reads the app's variables like the file that includes it)"}},
		{name: "remote and variable paths", files: map[string]string{
			"compose.yaml": "include:\n  - https://example.com/octo/infra.git\n  - git@example.com:octo/infra.git\n  - oci://registry.example.com/octo/infra:1\n  - ${WHERE}/compose.yaml\nservices:\n  web:\n    extends:\n      file: ssh://example.com/octo/base.git\n      service: base\n",
		}, want: []string{
			"unsupported_setting extends.file in compose.yaml loads ssh://example.com/octo/base.git from outside the repository, which is not allowed",
			"unsupported_setting include in compose.yaml loads git@example.com:octo/infra.git from outside the repository, which is not allowed",
			"unsupported_setting include in compose.yaml loads https://example.com/octo/infra.git from outside the repository, which is not allowed",
			"unsupported_setting include in compose.yaml loads oci://registry.example.com/octo/infra:1 from outside the repository, which is not allowed",
			"unsupported_setting include in compose.yaml uses a variable (${WHERE}/compose.yaml); write the path itself",
		}},
		{name: "paths outside the checkout", files: map[string]string{
			"compose.yaml": "include:\n  - ../other.yaml\n  - " + filepath.Join(outside, "other.yaml") + "\n  - linked.yaml\n  - sub\nservices:\n  web:\n    extends:\n      file: ../../etc/base.yaml\n      service: base\n",
			"sub/x.yaml":   "services: {}\n",
		}, links: map[string]string{"linked.yaml": filepath.Join(outside, "other.yaml")}, want: []string{
			"file_outside_release extends.file in compose.yaml reads ../../etc/base.yaml, which is outside the repository checkout",
			"unsupported_setting include in compose.yaml names the directory sub; name the Compose file",
			"file_outside_release include in compose.yaml reads ../other.yaml, which is outside the repository checkout",
			"file_outside_release include in compose.yaml reads " + filepath.Join(outside, "other.yaml") + ", which is outside the repository checkout",
			"file_outside_release include in compose.yaml reads linked.yaml, which is outside the repository checkout",
		}},
		{name: "include hidden from a text search", files: map[string]string{
			"compose.yaml": "x-a: &a\n  \"\\x69nclude\":\n    - path: inc.yaml\n      env_file: /etc/hostname\n<<: *a\nservices: {}\n---\ninclude: [https://example.com/x.git]\n",
			"inc.yaml":     "services: {}\n",
		}, want: []string{
			"unsupported_setting include in compose.yaml loads https://example.com/x.git from outside the repository, which is not allowed",
			"unsupported_setting include in compose.yaml sets env_file, which is not allowed (an included file reads the app's variables like the file that includes it)",
		}},
		{name: "not YAML", files: map[string]string{"compose.yaml": "services:\n  web: [\n"},
			want: []string{"unsupported_setting compose.yaml is not a YAML file koroku can read"}},
		{name: "malformed include", files: map[string]string{"compose.yaml": "include: inc.yaml\n"},
			want: []string{"unsupported_setting include in compose.yaml is not a list"}},
	} {
		dir := t.TempDir()
		for name, content := range tc.files {
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, target := range tc.links {
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		for _, v := range ScanFiles(dir, "compose.yaml") {
			if !v.Relaxable || v.Service != "" {
				t.Errorf("%s: %+v must be relaxable and without a service", tc.name, v)
			}
			got = append(got, v.Rule+" "+v.Message)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, strings.Join(got, "\n     "), strings.Join(tc.want, "\n     "))
		}
	}
}
