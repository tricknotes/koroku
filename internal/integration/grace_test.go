//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// graceCompose has a worker that, on SIGTERM, takes a few seconds to finish
// its job and records that it did, in a named volume the next container
// sees. The job takes longer than Docker's default 10 s, so without the
// stop_grace_period the worker would be killed before writing the record.
// The worker reads the commit, so every deploy recreates it (ARCHITECTURE 6).
const graceCompose = `services:
  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"]
  worker:
    image: busybox:1.37
    stop_grace_period: 20s
    environment:
      KOROKU_COMMIT_SHA: ${KOROKU_COMMIT_SHA}
    volumes:
      - jobs:/data
    command:
      - sh
      - -c
      - 'trap "sleep 12; echo finished >> /data/stops; exit 0" TERM; while :; do sleep 1 & wait $$!; done'
volumes:
  jobs:
`

// TestStopGracePeriodIsWaitedFor: a redeploy recreates the worker, and
// Docker lets a worker with a stop_grace_period finish its job (12 s, more
// than Docker's default 10 s) before replacing it. The start step says
// what it may wait for (ARCHITECTURE 6).
func TestStopGracePeriodIsWaitedFor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "")
	source.write("compose.yaml", graceCompose)
	first := source.commit("Worker with a grace period")
	slug := e.slug("grace")
	e.createApp(slug, source.url())

	d := e.deploy(slug, map[string]string{"sha": first})
	if d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if log := e.logText(d.ID); !strings.Contains(log, "This release sets a longer stop grace period: worker 20s") {
		t.Errorf("first deploy log:\n%s", log)
	}

	source.write("NOTES.md", "A new commit, so the worker is recreated.\n")
	second := source.commit("Redeploy")
	started := time.Now()
	d = e.deploy(slug, map[string]string{"sha": second})
	if d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if took := time.Since(started); took < 12*time.Second {
		t.Errorf("the redeploy took %s; it did not wait for the worker's job", took)
	}
	log := e.logText(d.ID)
	if !strings.Contains(log, "Stopping the running containers may wait up to 20s: worker 20s") {
		t.Errorf("the start step does not say what it waits for:\n%s", log)
	}

	out, err := exec.Command("docker", "ps", "--quiet",
		"--filter", "label=com.docker.compose.project=koroku-"+slug,
		"--filter", "label=com.docker.compose.service=worker").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("find the worker: %v", err)
	}
	stops, err := exec.Command("docker", "exec", strings.Fields(string(out))[0], "cat", "/data/stops").CombinedOutput()
	if err != nil || strings.TrimSpace(string(stops)) != "finished" {
		t.Errorf("the old worker did not finish its job before it was replaced: %v %q", err, stops)
	}
}

// TestPolicyLimitsStopGracePeriods: grace periods that add up to more than
// 10 minutes are refused before any container is touched.
func TestPolicyLimitsStopGracePeriods(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "")
	source.write("compose.yaml", `services:
  web:
    image: busybox:1.37
    stop_grace_period: 1m
    command: ["sleep", "infinity"]
  scheduler:
    image: busybox:1.37
    stop_grace_period: 9m30s
    command: ["sleep", "infinity"]
`)
	sha := source.commit("Too long to stop")
	slug := e.slug("gracelimit")
	e.createApp(slug, source.url())
	// The relax flag does not lift this limit.
	if code := e.call("PATCH", "/api/apps/"+slug, map[string]any{"relax_policy": true}, nil); code != 200 {
		t.Fatalf("relax the policy: %d", code)
	}

	d := e.deploy(slug, map[string]string{"sha": sha})
	if d.Status != "failed" || !strings.Contains(d.Error, "stop_grace_period") || !strings.Contains(d.Error, "10m30s") {
		t.Fatalf("deployment = %+v\n%s", d, e.logText(d.ID))
	}
	if states := containerStates(t, "koroku-"+slug); len(states) != 0 {
		t.Errorf("containers were created despite the policy: %v", states)
	}
}
