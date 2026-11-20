//go:build integration

package integration

import "testing"

// TestCommitVariables: koroku adds KOROKU_COMMIT_SHA / KOROKU_COMMIT_SHORT
// to the env file, so they work for interpolation in the Compose file and
// reach the containers (ARCHITECTURE chapter 6).
func TestCommitVariables(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := newRepo(t, "")
	r.write("compose.yaml", `services:
  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    labels:
      commit: ${KOROKU_COMMIT_SHORT:?}
    command:
      - sh
      - -c
      - 'mkdir -p /www && printf "short=%s\nlong=%s\n" "$$KOROKU_COMMIT_SHORT" "$$KOROKU_COMMIT_SHA" > /www/index.html && exec httpd -f -p 8080 -h /www'
`)
	sha := r.commit("Commit variables")
	slug := e.slug("commitvars")
	app := e.createApp(slug, r.url())
	if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
		t.Fatalf("deploy %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	got := page(t, app.HostPort)
	if got["short"] != sha[:7] || got["long"] != sha {
		t.Errorf("page = %v, want the commit %s", got, sha)
	}
}
