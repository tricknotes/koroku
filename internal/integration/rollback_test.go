//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRollbackToKeptRelease covers ARCHITECTURE 7.1 on a real daemon: a
// rollback starts the kept release of an earlier deployment again with its
// environment, without fetching or building, and the volume keeps its data.
func TestRollbackToKeptRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "build-app")
	v1 := source.commit("Version 1")
	slug := e.slug("rollback")
	app := e.createApp(slug, source.url())

	e.setEnv(slug, map[string]string{"GREETING": "hello-one"})
	first := e.deploy(slug, map[string]string{"sha": v1})
	source.write("VERSION", "v2\n")
	v2 := source.commit("Version 2")
	e.setEnv(slug, map[string]string{"GREETING": "hello-two"})
	second := e.deploy(slug, map[string]string{"sha": v2})
	if first.Status != "succeeded" || second.Status != "succeeded" {
		t.Fatalf("deploys: %s, %s", first.Status, second.Status)
	}
	if got := page(t, app.HostPort); got["version"] != "v2" || got["greeting"] != "hello-two" {
		t.Fatalf("page before the rollback = %v", got)
	}

	var rb deployment
	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/deployments", map[string]any{"rollback_to": first.ID}, &rb); status != http.StatusAccepted {
		t.Fatalf("rollback: status %d", status)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) && rb.Status != "succeeded" && rb.Status != "failed" {
		time.Sleep(300 * time.Millisecond)
		e.call(http.MethodGet, fmt.Sprintf("/api/deployments/%d", rb.ID), nil, &rb)
	}
	log := e.logText(rb.ID)
	if rb.Status != "succeeded" {
		t.Fatalf("rollback %s: %s\n%s", rb.Status, rb.Error, log)
	}
	// The release ran with its own environment, and the volume survived.
	if got := page(t, app.HostPort); got["version"] != "v1" || got["greeting"] != "hello-one" || got["boots"] != "3" {
		t.Errorf("page after the rollback = %v", got)
	}
	for _, step := range []string{" fetch ", " build ", " pull "} {
		if strings.Contains(log, step) {
			t.Errorf("the rollback ran the%sstep:\n%s", step, log)
		}
	}

	var current struct {
		CurrentDeployment struct {
			ID int64 `json:"id"`
		} `json:"current_deployment"`
	}
	e.call(http.MethodGet, "/api/apps/"+slug, nil, &current)
	if current.CurrentDeployment.ID != first.ID {
		t.Errorf("current deployment = %d, want %d", current.CurrentDeployment.ID, first.ID)
	}
}
