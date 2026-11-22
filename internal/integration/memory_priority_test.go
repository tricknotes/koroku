//go:build integration

package integration

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// TestProductionMemoryPriority: the containers Docker creates carry the
// OOM score and memory reservation of the app's environment, and a switch
// of environment takes effect with the next deploy (ARCHITECTURE 6.2).
func TestProductionMemoryPriority(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "image-app")
	first := source.commit("Image-only app")
	slug := e.slug("prio")
	e.createApp(slug, source.url())
	if code := e.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"environment": "production"}, nil); code != http.StatusOK {
		t.Fatalf("switch to production: %d", code)
	}

	d := e.deploy(slug, map[string]string{"sha": first})
	if d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	// The default limit is 512m, so half of it is reserved.
	if got := hostConfig(t, "koroku-"+slug); got != "-400 268435456 536870912" {
		t.Errorf("production web: oom_score_adj, reservation, limit = %s", got)
	}

	if code := e.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"environment": "staging"}, nil); code != http.StatusOK {
		t.Fatalf("switch to staging: %d", code)
	}
	source.write("NOTES.md", "Now a staging app.\n")
	second := source.commit("Redeploy as staging")
	d = e.deploy(slug, map[string]string{"sha": second})
	if d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if got := hostConfig(t, "koroku-"+slug); got != "500 0 536870912" {
		t.Errorf("staging web: oom_score_adj, reservation, limit = %s", got)
	}
}

// hostConfig prints the web container's OomScoreAdj, MemoryReservation and
// Memory as Docker created them.
func hostConfig(t *testing.T, project string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--quiet",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service=web").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("find the web container of %s: %v", project, err)
	}
	id := strings.Fields(string(out))[0]
	out, err = exec.Command("docker", "inspect", "--format",
		"{{.HostConfig.OomScoreAdj}} {{.HostConfig.MemoryReservation}} {{.HostConfig.Memory}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	return strings.TrimSpace(string(out))
}
