//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/compose"
)

const escapeWeb = `  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"]
`

// TestPolicyBlocksWaysOut deploys, through the engine and a real Compose,
// repositories whose Compose file reaches the host or another app with
// settings the policy does not judge by value. Compose accepts and acts on
// every one of them (a provider's program runs on the host, use_api_socket
// mounts the Docker socket, ...); each deployment must fail at validation
// before anything of it runs.
func TestPolicyBlocksWaysOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	tmp := t.TempDir()
	marker := filepath.Join(tmp, "provider-ran")
	otherEnv := filepath.Join(tmp, "other-app-env")
	if err := os.WriteFile(otherEnv, []byte("OTHER_APP_SECRET=stolen-value-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostFile := filepath.Join(tmp, "master.key")
	const hostLine = "0123abcdHOSTSECRETLINE"
	if err := os.WriteFile(hostFile, []byte(hostLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second app whose network and image names the others aim at.
	victimRepo := newRepo(t, "image-app")
	victim := e.slug("victim")
	e.createApp(victim, victimRepo.url())
	if d := e.deploy(victim, map[string]string{"sha": victimRepo.commit("victim")}); d.Status != "succeeded" {
		t.Fatalf("victim: %+v\n%s", d, e.logText(d.ID))
	}

	dockerfile := "FROM busybox:1.37\nSTOPSIGNAL SIGKILL\nCMD [\"sh\", \"-c\", \"mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www\"]\n"
	sidecar := func(name, settings string) string {
		return "services:\n" + escapeWeb + "  " + name + ":\n    image: busybox:1.37\n    stop_signal: SIGKILL\n    command: [\"sleep\", \"infinity\"]\n" + settings
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		exec  string
		want  string // part of the deployment's error
	}{
		{name: "provider", exec: "run.sh", want: "provider is not a setting koroku allows", files: map[string]string{
			"compose.yaml": "services:\n" + escapeWeb + "  x:\n    provider:\n      type: ./run.sh\n",
			"run.sh":       "#!/bin/sh\necho ran >> " + marker + "\n",
		}},
		{name: "api-socket", want: "use_api_socket is not a setting koroku allows", files: map[string]string{
			"compose.yaml": sidecar("sock", "    use_api_socket: true\n"),
		}},
		{name: "build-tags", want: "build.tags is not a setting koroku allows", files: map[string]string{
			"compose.yaml": "services:\n  web:\n    build:\n      context: .\n      tags: [\"koroku-" + victim + "-web:r1\"]\n",
			"Dockerfile":   dockerfile,
		}},
		{name: "build-ssh", want: "build.ssh is not a setting koroku allows", files: map[string]string{
			"compose.yaml": "services:\n  web:\n    build:\n      context: .\n      ssh: [\"k=" + hostFile + "\"]\n",
			"Dockerfile":   dockerfile,
		}},
		{name: "build-entitlements", want: "build.entitlements is not a setting koroku allows", files: map[string]string{
			"compose.yaml": "services:\n  web:\n    build:\n      context: .\n      entitlements: [\"network.host\"]\n",
			"Dockerfile":   dockerfile,
		}},
		{name: "dockerfile-outside", want: compose.RuleFileOutside, files: map[string]string{
			"compose.yaml": "services:\n  web:\n    build:\n      context: .\n      dockerfile: ../../../../../../../../../../../.." + hostFile + "\n",
		}},
		{name: "include-env-file", want: "include in compose.yaml sets env_file", files: map[string]string{
			"compose.yaml": "include:\n  - path: inc.yaml\n    env_file: " + otherEnv + "\nservices:\n" + escapeWeb,
			"inc.yaml":     "services:\n  leak:\n    image: busybox:1.37\n    command: [\"sh\", \"-c\", \"echo ${OTHER_APP_SECRET:-none}; sleep infinity\"]\n",
		}},
		{name: "include-remote", want: "from outside the repository", files: map[string]string{
			"compose.yaml": "include:\n  - https://example.invalid/octo/infra.git\nservices:\n" + escapeWeb,
		}},
		{name: "overlay-volume", want: compose.RuleVolumeHostPath, files: map[string]string{
			"compose.yaml": sidecar("peek", "    volumes:\n      - hostfs:/hostfs:ro\nvolumes:\n  hostfs:\n    driver_opts:\n      type: overlay\n      device: overlay\n      o: \"lowerdir=/usr/share:/usr/lib\"\n"),
		}},
		{name: "network-mode", want: compose.RuleForeignNetwork, files: map[string]string{
			"compose.yaml": sidecar("spy", "    network_mode: koroku-"+victim+"_default\n"),
		}},
		{name: "foreign-image", want: compose.RuleForeignImage, files: map[string]string{
			"compose.yaml": "services:\n" + escapeWeb + "  thief:\n    image: koroku-" + victim + "-web:r1\n    pull_policy: never\n",
		}},
		{name: "hook", want: "post_start.privileged is not a setting koroku allows", files: map[string]string{
			"compose.yaml": "services:\n" + escapeWeb + "    post_start:\n      - command: [\"true\"]\n        privileged: true\n",
		}},
		{name: "seccomp-file", want: compose.RuleSecurityOpt, files: map[string]string{
			"compose.yaml": "services:\n" + escapeWeb + "    security_opt:\n      - seccomp=./allow.json\n",
			"allow.json":   "{\"defaultAction\":\"SCMP_ACT_ALLOW\"}\n",
		}},
	} {
		source := newRepo(t, "")
		for name, content := range tc.files {
			source.write(name, content)
		}
		if tc.exec != "" {
			if err := os.Chmod(filepath.Join(source.dir, tc.exec), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		sha := source.commit(tc.name)
		slug := e.slug(tc.name)
		e.createApp(slug, source.url())
		d := e.deploy(slug, map[string]string{"sha": sha})
		log := e.logText(d.ID)
		if d.Status != "failed" || !strings.Contains(d.Error, "policy check failed") || !strings.Contains(d.Error, tc.want) {
			t.Errorf("%s: deployment = %+v\n%s", tc.name, d, log)
		}
		if strings.Contains(log, "==> ") && (strings.Contains(log, " build Building") || strings.Contains(log, " start ")) {
			t.Errorf("%s: the deployment went past validation:\n%s", tc.name, log)
		}
		if strings.Contains(strings.ToLower(log+d.Error), strings.ToLower(hostLine)) || strings.Contains(log+d.Error, "stolen-value-123") {
			t.Errorf("%s: a host file's content is in the deployment log or error", tc.name)
		}
		if states := containerStates(t, "koroku-"+slug); len(states) != 0 {
			t.Errorf("%s: containers exist: %v", tc.name, states)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the provider's program ran on the host")
	}
	if out, err := exec.Command("docker", "image", "inspect", "koroku-"+victim+"-web:r1").CombinedOutput(); err == nil {
		t.Errorf("an image with another app's name exists: %s", out)
	}

	// The owner's "relax policy" flag waives them, as it does the other
	// rules that protect the host.
	source := newRepo(t, "")
	source.write("compose.yaml", sidecar("gpu", "    cgroup_parent: system.slice\n"))
	slug := e.slug("relaxed")
	e.createApp(slug, source.url())
	if status := e.call("PATCH", "/api/apps/"+slug, map[string]any{"relax_policy": true}, nil); status != 200 {
		t.Fatalf("relax: status %d", status)
	}
	if d := e.deploy(slug, map[string]string{"sha": source.commit("relaxed")}); d.Status != "succeeded" || !strings.Contains(e.logText(d.ID), "Policy waived for this app") {
		t.Errorf("relaxed: %+v\n%s", d, e.logText(d.ID))
	}
}

// TestComposePrintsOnlyAllowedSettingsForOrdinaryFiles runs the Compose of
// this machine over a file of ordinary settings: a Compose release that
// prints a setting under a new name, or adds one to every service, would
// otherwise make koroku refuse every app.
func TestComposePrintsOnlyAllowedSettingsForOrdinaryFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data, err := os.ReadFile("../../testdata/compose/allowed.compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "-p", "koroku-blog", "-f", "compose.yaml", "config", "--format", "json", "--no-env-resolution")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	project, err := compose.ParseProject(out)
	if err != nil {
		t.Fatal(err)
	}
	result := compose.CheckPolicy(project, compose.PolicyParams{
		ReleaseDir: dir, ProjectName: "koroku-blog", WebService: "web", WebPort: 3000, HostPort: 20001,
	})
	for _, v := range result.Violations {
		if v.Rule != compose.RulePortExposure {
			t.Errorf("an ordinary setting is refused: %s", v)
		}
	}
	if found := compose.ScanFiles(dir, "compose.yaml"); len(found) != 0 {
		t.Errorf("ScanFiles: %v", found)
	}
}
