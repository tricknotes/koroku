//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// httpService is a busybox service that answers with body on port 8080.
func httpService(body string) string {
	return fmt.Sprintf(`    image: busybox:1.37
    stop_signal: SIGKILL
    command:
      - sh
      - -c
      - 'mkdir -p /www && echo %s > /www/index.html && exec httpd -f -p 8080 -h /www'
`, body)
}

const sleeper = `    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sleep", "3600"]
`

func groupRepo(t *testing.T, services map[string]string) (*repo, string) {
	t.Helper()
	r := newRepo(t, "")
	var b strings.Builder
	b.WriteString("services:\n")
	for _, name := range []string{"web", "api", "db"} {
		if def, ok := services[name]; ok {
			b.WriteString("  " + name + ":\n" + def)
		}
	}
	r.write("compose.yaml", b.String())
	return r, r.commit("Initial")
}

func execIn(container string, args ...string) (string, error) {
	out, err := exec.Command("docker", append([]string{"exec", container}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestGroupNetwork covers ARCHITECTURE 6.1 against a real daemon: joined
// services reach each other by their group hostname, nothing else of the
// apps is shared, a bare service name never resolves to another member,
// and leaving cuts the connection at once.
func TestGroupNetwork(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	repoA, shaA := groupRepo(t, map[string]string{"web": httpService("a-web"), "db": sleeper})
	repoB, shaB := groupRepo(t, map[string]string{"web": httpService("b-web"), "api": httpService("b-api"), "db": sleeper})
	slugA, slugB := e.slug("grp-a"), e.slug("grp-b")
	e.createApp(slugA, repoA.url())
	e.createApp(slugB, repoB.url())

	var group struct {
		ID      int64  `json:"id"`
		Network string `json:"network"`
	}
	if status := e.call(http.MethodPost, "/api/groups", map[string]any{"name": "it-" + e.suffix, "environment": "staging"}, &group); status != http.StatusCreated {
		t.Fatalf("create group: %d", status)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", group.Network).Run() })
	join := func(slug string, services ...string) int {
		return e.call(http.MethodPut, "/api/apps/"+slug+"/group", map[string]any{"group_id": group.ID, "services": services}, nil)
	}
	if status := join(slugA, "web"); status != http.StatusOK {
		t.Fatalf("join A: %d", status)
	}
	// B also puts its "db" on the network: A's "db" must still never
	// resolve to it.
	if status := join(slugB, "api", "db"); status != http.StatusOK {
		t.Fatalf("join B: %d", status)
	}
	for slug, sha := range map[string]string{slugA: shaA, slugB: shaB} {
		if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
			t.Fatalf("deploy %s %s: %s\n%s", slug, d.Status, d.Error, e.logText(d.ID))
		}
	}

	webA := "koroku-" + slugA + "-web-1"
	apiHost := fmt.Sprintf("http://api.%s.koroku.internal:8080/", slugB)
	if out, err := execIn(webA, "wget", "-q", "-T", "3", "-O", "-", apiHost); err != nil || out != "b-api" {
		t.Errorf("A's web -> B's api by group hostname: %q, %v", out, err)
	}
	if out, err := execIn(webA, "ping", "-c", "1", "-W", "2", "web."+slugB+".koroku.internal"); err == nil {
		t.Errorf("A's web resolves B's web, which did not join:\n%s", out)
	}
	if out, err := execIn("koroku-"+slugB+"-api-1", "ping", "-c", "1", "-W", "2", "koroku-"+slugA+"-db-1"); err == nil {
		t.Errorf("B's api reaches A's db, which did not join:\n%s", out)
	}

	// The experiment behind the design: with A's own db down, "db" must
	// not resolve to B's db on the group network.
	if out, err := exec.Command("docker", "stop", "koroku-"+slugA+"-db-1").CombinedOutput(); err != nil {
		t.Fatalf("stop A's db: %v\n%s", err, out)
	}
	if out, err := execIn(webA, "ping", "-c", "1", "-W", "2", "db"); err == nil {
		t.Errorf("with A's db stopped, \"db\" still resolves from A's web:\n%s", out)
	}

	// The network is internal and attached by koroku, not by Compose.
	internal, err := exec.Command("docker", "network", "inspect", "--format", "{{.Internal}}", group.Network).Output()
	if err != nil || strings.TrimSpace(string(internal)) != "true" {
		t.Errorf("group network internal = %q, %v", internal, err)
	}

	// Leaving disconnects at once, without a deploy.
	if status := e.call(http.MethodPut, "/api/apps/"+slugA+"/group", map[string]any{"group_id": nil, "services": []string{}}, nil); status != http.StatusOK {
		t.Fatalf("leave: %d", status)
	}
	if out, err := execIn(webA, "wget", "-q", "-T", "3", "-O", "-", apiHost); err == nil {
		t.Errorf("A's web still reaches B's api after leaving: %q", out)
	}

	// Leave B too, then the group can be deleted.
	if status := e.call(http.MethodPut, "/api/apps/"+slugB+"/group", map[string]any{"group_id": nil, "services": []string{}}, nil); status != http.StatusOK {
		t.Fatalf("leave B: %d", status)
	}
	if status := e.call(http.MethodDelete, fmt.Sprintf("/api/groups/%d", group.ID), nil, nil); status != http.StatusNoContent {
		t.Errorf("delete group: %d", status)
	}
	if err := exec.Command("docker", "network", "inspect", group.Network).Run(); err == nil {
		t.Errorf("the group network is still there after deleting the group")
	}
}
