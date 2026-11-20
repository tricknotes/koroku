package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("%s %s: Content-Type = %q", r.Method, r.URL.Path, got)
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func TestCreateDeploymentSendsTheCommitAndEnvironment(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/blog", 7)
	sha := strings.Repeat("a", 40)
	var got map[string]any
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/deployments", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := f.installationOf(w, r); !ok || id != 7 {
			return
		}
		got = decodeBody(t, r)
		writeJSON(w, http.StatusCreated, map[string]any{"id": 991, "sha": sha})
	})

	id, err := f.client().CreateDeployment(context.Background(), "octo", "blog", NewDeployment{
		SHA: sha, Environment: "blog-staging", Description: "Deploying with koroku", Production: false,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if id != 991 {
		t.Errorf("id = %d, want 991", id)
	}
	want := map[string]any{
		"ref": sha, "task": "deploy", "auto_merge": false, "required_contexts": []any{},
		"environment": "blog-staging", "description": "Deploying with koroku",
		"transient_environment": false, "production_environment": false,
	}
	for k, v := range want {
		gotJSON, _ := json.Marshal(got[k])
		wantJSON, _ := json.Marshal(v)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("body[%q] = %s, want %s", k, gotJSON, wantJSON)
		}
	}
	if len(got) != len(want) {
		t.Errorf("body has %d fields, want %d: %v", len(got), len(want), got)
	}
}

func TestCreateDeploymentWithoutPermission(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/blog", 7)
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/deployments", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "Resource not accessible by integration")
	})
	_, err := f.client().CreateDeployment(context.Background(), "octo", "blog", NewDeployment{
		SHA: strings.Repeat("b", 40), Environment: "blog", Production: true,
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	assertNoLeak(t, err, "ghs_fake_7_1")
}

func TestCreateDeploymentNotInstalled(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/blog", 7)
	_, err := f.client().CreateDeployment(context.Background(), "octo", "other", NewDeployment{
		SHA: strings.Repeat("b", 40), Environment: "other",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if n := f.count("POST /repos/octo/other/deployments"); n != 0 {
		t.Errorf("deployments requested %d times without an installation", n)
	}
}

func TestCreateDeploymentStatus(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/blog", 7)
	var bodies []map[string]any
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/deployments/{id}/statuses", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationOf(w, r); !ok {
			return
		}
		if r.PathValue("id") != "991" {
			t.Errorf("deployment id = %s", r.PathValue("id"))
		}
		bodies = append(bodies, decodeBody(t, r))
		writeJSON(w, http.StatusCreated, map[string]any{"id": 1, "state": "success"})
	})
	c := f.client()
	long := strings.Repeat("あ", 200)
	if err := c.CreateDeploymentStatus(context.Background(), "octo", "blog", 991, DeploymentStatus{
		State: DeploymentSuccess, Description: long,
		EnvironmentURL: "https://blog.example.com", LogURL: "https://console.example.com/apps/blog/deploy?deployment=3",
	}); err != nil {
		t.Fatalf("CreateDeploymentStatus: %v", err)
	}
	if err := c.CreateDeploymentStatus(context.Background(), "octo", "blog", 991, DeploymentStatus{
		State: DeploymentInProgress, Description: "Deploying",
	}); err != nil {
		t.Fatalf("CreateDeploymentStatus: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d status requests, want 2", len(bodies))
	}
	first := bodies[0]
	if first["state"] != "success" || first["auto_inactive"] != true ||
		first["environment_url"] != "https://blog.example.com" ||
		first["log_url"] != "https://console.example.com/apps/blog/deploy?deployment=3" {
		t.Errorf("first status = %v", first)
	}
	if d := first["description"].(string); len([]rune(d)) != maxDeploymentDescription || !strings.HasSuffix(d, "...") {
		t.Errorf("description has %d characters: %q", len([]rune(d)), d)
	}
	if _, ok := bodies[1]["environment_url"]; ok {
		t.Errorf("empty environment_url was sent: %v", bodies[1])
	}
	if _, ok := bodies[1]["log_url"]; ok {
		t.Errorf("empty log_url was sent: %v", bodies[1])
	}
}

func TestCreateDeploymentStatusRejectsUnknownStates(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/blog", 7)
	err := f.client().CreateDeploymentStatus(context.Background(), "octo", "blog", 1, DeploymentStatus{State: "done"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	if len(f.seen()) != 0 {
		t.Errorf("requests were sent: %v", f.seen())
	}
}

func TestDeploymentsNeedTheApp(t *testing.T) {
	c, err := NewAnonymous("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDeployment(context.Background(), "octo", "blog", NewDeployment{
		SHA: strings.Repeat("a", 40), Environment: "blog",
	}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("CreateDeployment err = %v", err)
	}
	if err := c.CreateDeploymentStatus(context.Background(), "octo", "blog", 1, DeploymentStatus{State: DeploymentQueued}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("CreateDeploymentStatus err = %v", err)
	}
}
