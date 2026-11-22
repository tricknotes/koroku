//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestComposeView: the configuration view of a real release keeps the
// values of the app's variables out of the override, the merged
// configuration and the container list, and Compose's config hash tells
// whether the containers were created from the current release
// (ARCHITECTURE 9.10).
func TestComposeView(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "image-app")
	// The repository refers to the variable, repeats its value literally
	// and sets one of its own; all three must stay hidden.
	const saved = "saved-greeting-s3cret-value"
	const onlyInFile = "only-in-the-env-file-s3cret"
	compose, err := os.ReadFile(samples + "/image-app/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source.write("compose.yaml", string(compose)+`    environment:
      FROM_VARIABLE: ${GREETING}
      LITERAL: `+saved+`
    labels:
      com.example.greeting: ${GREETING:-none}
`)
	sha := source.commit("Refer to the greeting")
	slug := e.slug("view")
	e.createApp(slug, source.url())
	if code := e.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"environment": "production"}, nil); code != http.StatusOK {
		t.Fatalf("switch to production: %d", code)
	}
	e.setEnv(slug, map[string]string{"GREETING": saved})
	d := e.deploy(slug, map[string]string{"sha": sha})
	if d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}

	var view struct {
		DeploymentID int64   `json:"deployment_id"`
		Override     string  `json:"override"`
		Config       *string `json:"config"`
		ConfigError  string  `json:"config_error"`
	}
	raw := e.get(t, "/api/apps/"+slug+"/compose", &view)
	if view.DeploymentID != d.ID || view.Config == nil {
		t.Fatalf("compose view: %s", raw)
	}
	for _, want := range []string{
		`FROM_VARIABLE: "[MASKED]"`, `LITERAL: "[MASKED]"`,
		`com.example.greeting: "${GREETING:-none}"`, // not interpolated
		"oom_score_adj: -400",
		"path: " + e.engine.EnvFile(slug), // env_file stays a path
	} {
		if !strings.Contains(*view.Config, want) {
			t.Errorf("merged configuration lacks %q:\n%s", want, *view.Config)
		}
	}
	if !strings.Contains(view.Override, "oom_score_adj: -400") {
		t.Errorf("override:\n%s", view.Override)
	}

	var containers struct {
		Compared   bool `json:"compared"`
		Containers []struct {
			Service    string   `json:"service"`
			Current    *bool    `json:"current"`
			Mismatches []string `json:"mismatches"`
			Memory     int64    `json:"memory_limit_bytes"`
			OomAdj     int      `json:"oom_score_adj"`
		} `json:"containers"`
	}
	raw = e.get(t, "/api/apps/"+slug+"/containers", &containers)
	if !containers.Compared || len(containers.Containers) != 1 {
		t.Fatalf("containers: %s", raw)
	}
	web := containers.Containers[0]
	if web.Service != "web" || web.Current == nil || !*web.Current || len(web.Mismatches) != 0 ||
		web.Memory != 512<<20 || web.OomAdj != -400 {
		t.Errorf("web right after the deploy: %s", raw)
	}
	assertHidden(t, raw, saved)

	// A changed env file changes the configuration Compose would create
	// the container from: the running container is now outdated.
	envFile := e.engine.EnvFile(slug)
	content, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, append(content, []byte("OLD_TOKEN="+onlyInFile+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	raw = e.get(t, "/api/apps/"+slug+"/containers", &containers)
	if web := containers.Containers[0]; web.Current == nil || *web.Current || !slices.Contains(web.Mismatches, "outdated") {
		t.Errorf("web after the env file changed: %s", raw)
	}
	assertHidden(t, raw, saved, onlyInFile)
	assertHidden(t, e.get(t, "/api/apps/"+slug+"/compose", &view), saved, onlyInFile)
}

func (e *env) get(t *testing.T, path string, out any) string {
	t.Helper()
	resp, err := http.Get(e.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertHidden(t *testing.T, body string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(body, s) {
			t.Errorf("response contains %q: %s", s, body)
		}
	}
}
