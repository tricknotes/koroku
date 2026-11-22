//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/store"
)

// TestSleepAndWake puts a real app to sleep (ARCHITECTURE 8.4): its
// containers stop and stay, a request forwarded as Caddy does for a sleeping
// app wakes them and is answered by the app, the data volume survives, and a
// backup dump hook's service is started on its own while the app sleeps.
func TestSleepAndWake(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "build-app")
	sha := source.commit("Version 1")
	slug := e.slug("sleepy")
	project := "koroku-" + slug
	app := e.createApp(slug, source.url())
	if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if status := e.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"idle_sleep": true, "idle_sleep_minutes": 15}, nil); status != http.StatusOK {
		t.Fatalf("enable idle sleep: status %d", status)
	}
	if got := page(t, app.HostPort); got["boots"] != "1" {
		t.Fatalf("page = %v", got)
	}

	ctx := context.Background()
	st := e.db
	current, err := st.GetApp(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.engine.Sleep(ctx, current); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	states := containerStates(t, project)
	if len(states) != 2 {
		t.Fatalf("the containers must stay after a sleep: %v", states)
	}
	for service, s := range states {
		if s != "exited" {
			t.Errorf("asleep, %s is %s", service, s)
		}
	}
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", app.HostPort)); err == nil {
		t.Error("the app still answers while asleep")
	}

	// Only the dump service runs for a backup; the app stays asleep.
	err = e.engine.WithServiceRunning(ctx, current.ID, "sidecar", func(context.Context) error {
		if s := containerStates(t, project); s["sidecar"] != "running" || s["web"] != "exited" {
			t.Errorf("during the dump: %v", s)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithServiceRunning: %v", err)
	}
	if s := containerStates(t, project); s["sidecar"] != "exited" {
		t.Errorf("after the dump: %v", s)
	}

	// A request as Caddy forwards it for the sleeping app.
	req, err := http.NewRequest(http.MethodGet, e.server.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = slug + ".dev.example.com"
	req.Header.Set(caddy.WakeHeader, slug)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "boots=2") {
		t.Fatalf("woken answer: %d %q", resp.StatusCode, body)
	}
	woken, err := st.GetApp(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if woken.SleepingSince != nil || woken.DesiredState != store.DesiredRunning {
		t.Errorf("after the wake: sleeping %v, desired %s", woken.SleepingSince, woken.DesiredState)
	}
	if s := containerStates(t, project); s["web"] != "running" || s["sidecar"] != "running" {
		t.Errorf("after the wake: %v", s)
	}
}
