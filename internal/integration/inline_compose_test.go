//go:build integration

package integration

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInlineComposeApp deploys a repository that has no Compose file with
// one written in koroku (ARCHITECTURE 6.3): relative paths resolve from the
// root of the checkout, a kept release starts again from its own copy, and
// the blue/green override copies the public service from it.
func TestInlineComposeApp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "build-app")
	if err := os.Remove(filepath.Join(source.dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	v1 := source.commit("Version 1, without a Compose file")
	slug := e.slug("inline")

	const content = `services:
  web:
    build: .
    environment:
      FROM_REPO_DOTENV: "${GREETING:-unset}"
    ports:
      - "8080:8080"
    volumes:
      - data:/data
volumes:
  data:
`
	var app struct {
		HostPort    int     `json:"host_port"`
		AltHostPort *int    `json:"alt_host_port"`
		WebColor    *string `json:"web_color"`
	}
	if status := e.call(http.MethodPost, "/api/apps", map[string]any{
		"slug": slug, "repo_url": source.url(), "web_port": 8080,
		"compose_source": "inline", "compose_content": content,
	}, &app); status != http.StatusCreated {
		t.Fatalf("create app: status %d", status)
	}
	e.setEnv(slug, map[string]string{"GREETING": "hello"})

	first := e.deploy(slug, map[string]string{"sha": v1})
	if first.Status != "succeeded" {
		t.Fatalf("first deployment %s: %s\n%s", first.Status, first.Error, e.logText(first.ID))
	}
	if got := page(t, app.HostPort); got["version"] != "v1" || got["dotenv"] != "hello" {
		t.Errorf("page after the first deploy = %v", got)
	}
	if log := e.logText(first.ID); !strings.Contains(log, "Compose file: koroku.compose.yaml (written in koroku") {
		t.Errorf("the log does not name the Compose file:\n%s", log)
	}

	// A saved change does not reach the kept release: stopping and starting
	// brings the same containers up from the release's own file.
	broken := strings.Replace(content, "build: .", "image: example.invalid/nothing:1", 1)
	if status := e.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"compose_content": broken}, nil); status != http.StatusOK {
		t.Fatalf("save the changed file: status %d", status)
	}
	for _, op := range []string{"stop", "start"} {
		if status := e.call(http.MethodPost, "/api/apps/"+slug+"/"+op, nil, nil); status >= 300 {
			t.Fatalf("%s: status %d", op, status)
		}
	}
	if got := page(t, app.HostPort); got["version"] != "v1" {
		t.Errorf("page after stop and start = %v", got)
	}

	// Blue/green copies the public service with extends.file from the
	// written file; its build context is still the checkout.
	if status := e.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"compose_content": content, "blue_green": true}, nil); status != http.StatusOK {
		t.Fatalf("turn blue/green on: status %d", status)
	}
	source.write("VERSION", "v2\n")
	v2 := source.commit("Version 2")
	second := e.deploy(slug, map[string]string{"sha": v2})
	if second.Status != "succeeded" {
		t.Fatalf("second deployment %s: %s\n%s", second.Status, second.Error, e.logText(second.ID))
	}
	e.call(http.MethodGet, "/api/apps/"+slug, nil, &app)
	port := app.HostPort
	if app.WebColor == nil {
		t.Fatalf("the app has no colour after a blue/green deployment: %+v", app)
	}
	if *app.WebColor == "green" && app.AltHostPort != nil {
		port = *app.AltHostPort
	}
	if got := page(t, port); got["version"] != "v2" || got["boots"] != "3" {
		t.Errorf("page after the blue/green deploy = %v (boots=3 proves the volume survived)", got)
	}
}
