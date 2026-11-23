package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

func TestBlueGreenSettings(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	created := createApp(t, h, "blog")
	if created.BlueGreen || created.BlueGreenDrainSeconds != 10 || created.AltHostPort != nil || created.WebColor != nil {
		t.Errorf("defaults = %+v", created)
	}
	createApp(t, h, "shop")

	// A production app turns it on without a recent login: only the
	// console's confirmation asks (ARCHITECTURE 7.2).
	app, err := srv.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	app.Environment = store.EnvironmentProduction
	if err := srv.store.UpdateApp(context.Background(), app, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"blue_green": true, "blue_green_drain_seconds": 30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	got := decode[appJSON](t, rec)
	if !got.BlueGreen || got.BlueGreenDrainSeconds != 30 || got.AltHostPort == nil || *got.AltHostPort == got.HostPort {
		t.Fatalf("enabled = %+v", got)
	}
	port := *got.AltHostPort

	// Turning it off keeps the port for the next time.
	rec = request(t, h, http.MethodPatch, "/api/apps/blog", `{"blue_green": false}`)
	if got := decode[appJSON](t, rec); rec.Code != http.StatusOK || got.BlueGreen || got.AltHostPort == nil || *got.AltHostPort != port {
		t.Errorf("disabled: %d %s", rec.Code, rec.Body)
	}
	rec = request(t, h, http.MethodPatch, "/api/apps/blog", `{"blue_green": true}`)
	if got := decode[appJSON](t, rec); *got.AltHostPort != port {
		t.Errorf("enabled again: second port %d, want %d", *got.AltHostPort, port)
	}

	for _, seconds := range []string{"-1", "301"} {
		rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"blue_green_drain_seconds": `+seconds+`}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "blue_green_drain_seconds") {
			t.Errorf("%s seconds: %d %s", seconds, rec.Code, rec.Body)
		}
	}
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"blue_green_drain_seconds": 0}`); rec.Code != http.StatusOK {
		t.Errorf("0 seconds: %d %s", rec.Code, rec.Body)
	}

	// Set on an existing app only.
	rec = request(t, h, http.MethodPost, "/api/apps", `{"slug": "new", "repo_full_name": "octo/new", "web_port": 3000, "blue_green": true}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "blue_green") {
		t.Errorf("create: %d %s", rec.Code, rec.Body)
	}

	// The colours of the public service are no task services.
	rec = request(t, h, http.MethodPut, "/api/apps/blog/tasks", `{"tasks": [{"name": "migrate", "service": "web-green", "command": "bin/rails db:migrate"}]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "colour of the public service") {
		t.Errorf("task in a colour: %d %s", rec.Code, rec.Body)
	}
	// Another app may have a service of that name.
	rec = request(t, h, http.MethodPut, "/api/apps/shop/tasks", `{"tasks": [{"name": "x", "service": "web-green", "command": "true"}]}`)
	if rec.Code != http.StatusOK {
		t.Errorf("task of an app without blue/green: %d %s", rec.Code, rec.Body)
	}
}

func TestDeploymentJSONHasTheColour(t *testing.T) {
	switched := time.Date(2026, 11, 18, 12, 3, 41, 0, time.UTC)
	j := deploymentToJSON(store.Deployment{ID: 1, WebColor: store.ColorGreen, SwitchedAt: &switched}, "blog")
	if j.WebColor == nil || *j.WebColor != "green" || j.SwitchedAt == nil || !j.SwitchedAt.Equal(switched) {
		t.Errorf("json = %+v", j)
	}
	if j := deploymentToJSON(store.Deployment{ID: 2}, "blog"); j.WebColor != nil || j.SwitchedAt != nil {
		t.Errorf("plain deployment = %+v", j)
	}
}
