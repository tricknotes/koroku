package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

const imageWorkflow = ".github/workflows/image.yml"

// workflowRunBody is a workflow_run delivery for a run of path in
// octo/blog (repository ID 99) on branch at sha.
func workflowRunBody(action, conclusion, path, branch, sha string) string {
	return fmt.Sprintf(`{"action": %q,
		"workflow": {"id": 7, "name": "Image", "path": %q},
		"workflow_run": {"id": 1234, "name": "Image", "path": %q, "event": "push",
			"head_branch": %q, "head_sha": %q, "status": "completed", "conclusion": %q,
			"html_url": "https://github.com/octo/blog/actions/runs/1234",
			"head_repository": {"id": 99, "full_name": "octo/blog"},
			"actor": {"login": "octo"}, "triggering_actor": {"login": "octo"},
			"head_commit": {"id": %q, "message": "Fix the thing"}},
		"repository": {"id": 99, "full_name": "octo/blog"},
		"sender": {"login": "octo"}}`, action, path, path+"@refs/heads/"+branch, branch, sha, conclusion, sha)
}

// TestWorkflowRunDecisions is the decision table of ARCHITECTURE 5.2 for
// apps that wait for a workflow.
func TestWorkflowRunDecisions(t *testing.T) {
	const newer = "dddddddddddddddddddddddddddddddddddddddd"
	tests := []struct {
		name  string
		event string
		body  string
		// head is the branch head GitHub reports; "" leaves main at pushSHA.
		head string
		// result is the response's result; status the app's status in it.
		result string
		status string
		// deploys is whether a deployment is queued.
		deploys bool
	}{
		{name: "success", event: "workflow_run",
			body:   workflowRunBody("completed", "success", imageWorkflow, "main", pushSHA),
			result: "deployed", status: "queued", deploys: true},
		{name: "failure", event: "workflow_run",
			body:   workflowRunBody("completed", "failure", imageWorkflow, "main", pushSHA),
			result: "deployed", status: "skipped"},
		{name: "cancelled", event: "workflow_run",
			body:   workflowRunBody("completed", "cancelled", imageWorkflow, "main", pushSHA),
			result: "deployed", status: "skipped"},
		{name: "not completed", event: "workflow_run",
			body:   workflowRunBody("in_progress", "", imageWorkflow, "main", pushSHA),
			result: "ignored"},
		{name: "another workflow", event: "workflow_run",
			body:   workflowRunBody("completed", "success", ".github/workflows/ci.yml", "main", pushSHA),
			result: "ignored"},
		{name: "another branch", event: "workflow_run",
			body:   workflowRunBody("completed", "success", imageWorkflow, "feature", pushSHA),
			result: "ignored"},
		{name: "another repository", event: "workflow_run",
			body:   strings.ReplaceAll(workflowRunBody("completed", "success", imageWorkflow, "main", pushSHA), `"id": 99, "full_name": "octo/blog"`, `"id": 5, "full_name": "octo/other"`),
			result: "ignored"},
		{name: "from a fork", event: "workflow_run",
			body:   strings.Replace(workflowRunBody("completed", "success", imageWorkflow, "main", pushSHA), `"head_repository": {"id": 99, "full_name": "octo/blog"}`, `"head_repository": {"id": 6, "full_name": "mallory/blog"}`, 1),
			result: "ignored"},
		{name: "stale commit", event: "workflow_run",
			body: workflowRunBody("completed", "success", imageWorkflow, "main", pushSHA), head: newer,
			result: "deployed", status: "stale"},
		{name: "push waits for the workflow", event: "push",
			body:   pushBody(99, "octo/blog", "refs/heads/main", pushSHA),
			result: "deployed", status: "waiting_workflow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newWebhookFixture(t, testWebhookSecret)
			app := f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", RepoID: int64p(99),
				AutoDeploy: true, AutoDeployWorkflow: imageWorkflow})
			if tt.head != "" {
				f.heads["main"] = tt.head
			}
			rec := f.deliver(tt.event, "d1", tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			got := decode[webhookResultJSON](t, rec)
			if got.Result != tt.result {
				t.Fatalf("result = %s", rec.Body)
			}
			if tt.status != "" && (len(got.Apps) != 1 || got.Apps[0].Status != tt.status) {
				t.Fatalf("apps = %s", rec.Body)
			}
			calls := f.pushes.takeCalls()
			if tt.deploys != (len(calls) == 1) {
				t.Fatalf("calls = %v", calls)
			}
			if tt.deploys && calls[0] != `blog main `+pushSHA+` octo "Resolved commit" after `+imageWorkflow {
				t.Errorf("call = %s", calls[0])
			}
			activity, err := f.srv.store.ListActivity(context.Background(), app.ID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if tt.status == "skipped" {
				if len(activity) != 1 || activity[0].Action != actAutoDeploySkipped || activity[0].Actor != nil {
					t.Fatalf("activity = %+v", activity)
				}
				details := string(activity[0].Details)
				for _, want := range []string{imageWorkflow, pushSHA, `"branch":"main"`, "actions/runs/1234"} {
					if !strings.Contains(details, want) {
						t.Errorf("details %s lack %s", details, want)
					}
				}
			} else if len(activity) != 0 {
				t.Errorf("activity = %+v", activity)
			}
		})
	}
}

// TestPushWithoutWorkflowDeploysAsBefore: an app that waits for nothing
// deploys the push, and a workflow run does nothing for it.
func TestPushWithoutWorkflowDeploysAsBefore(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true})
	f.app(store.App{Slug: "blog-images", RepoFullName: "octo/blog", AutoDeploy: true, AutoDeployWorkflow: imageWorkflow})

	rec := f.deliver("push", "d1", pushBody(99, "octo/blog", "refs/heads/main", pushSHA))
	got := decode[webhookResultJSON](t, rec)
	if rec.Code != http.StatusOK || len(got.Apps) != 2 ||
		got.Apps[0].App != "blog" || got.Apps[0].Status != "queued" ||
		got.Apps[1].App != "blog-images" || got.Apps[1].Status != "waiting_workflow" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 1 || !strings.HasPrefix(calls[0], "blog main ") {
		t.Fatalf("calls = %v", calls)
	}

	rec = f.deliver("workflow_run", "d2", workflowRunBody("completed", "success", imageWorkflow, "main", pushSHA))
	got = decode[webhookResultJSON](t, rec)
	if len(got.Apps) != 1 || got.Apps[0].App != "blog-images" || got.Apps[0].Status != "queued" {
		t.Fatalf("workflow run: %s", rec.Body)
	}
	f.pushes.takeCalls()
}

// TestChainedWorkflowRunDeploys: a workflow started by another one's
// completion carries the original push's branch and commit.
func TestChainedWorkflowRunDeploys(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true, AutoDeployWorkflow: imageWorkflow})
	body := strings.Replace(workflowRunBody("completed", "success", imageWorkflow, "main", pushSHA),
		`"event": "push"`, `"event": "workflow_run"`, 1)
	rec := f.deliver("workflow_run", "d1", body)
	if got := decode[webhookResultJSON](t, rec); len(got.Apps) != 1 || got.Apps[0].Status != "queued" {
		t.Fatalf("%s", rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestWorkflowRunDuplicatesAndFailures(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	app := f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true, AutoDeployWorkflow: imageWorkflow})
	body := workflowRunBody("completed", "success", imageWorkflow, "main", pushSHA)

	// GitHub could not be asked for the branch head: redeliverable.
	delete(f.heads, "main")
	rec := f.deliver("workflow_run", "d1", body)
	if got := decode[webhookResultJSON](t, rec); rec.Code != http.StatusServiceUnavailable || got.Apps[0].Error != "branch_unresolved" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "secret-remote-detail") {
		t.Error("the response shows the resolver's error")
	}
	f.heads["main"] = pushSHA
	f.pushes.setErr(deploy.ErrBusy)
	if rec := f.deliver("workflow_run", "d1", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy: %d %s", rec.Code, rec.Body)
	}
	f.pushes.setErr(nil)
	if rec := f.deliver("workflow_run", "d1", body); decode[webhookResultJSON](t, rec).Apps[0].Status != "queued" {
		t.Fatalf("redelivery: %s", rec.Body)
	}
	if rec := f.deliver("workflow_run", "d1", body); decode[webhookResultJSON](t, rec).Apps[0].Status != "duplicate" {
		t.Fatalf("duplicate: %s", rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}

	// A stopped app neither deploys nor records a failed run.
	if err := f.srv.store.SetAppDesiredState(context.Background(), app.ID, store.DesiredStopped, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = f.deliver("workflow_run", "d2", workflowRunBody("completed", "failure", imageWorkflow, "main", pushSHA))
	if got := decode[webhookResultJSON](t, rec); got.Apps[0].Status != "stopped" {
		t.Fatalf("stopped: %s", rec.Body)
	}
	if activity, _ := f.srv.store.ListActivity(context.Background(), app.ID, 0, 10); len(activity) != 0 {
		t.Errorf("activity = %+v", activity)
	}
}

func TestAutoDeployWorkflowSetting(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"auto_deploy": true, "auto_deploy_workflow": " .github/workflows/image.yaml "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := decode[appJSON](t, rec); got.AutoDeployWorkflow != ".github/workflows/image.yaml" {
		t.Errorf("%+v", got)
	}
	for _, bad := range []string{"image.yml", ".github/workflows/image.json", ".github/workflows/sub/image.yml", "../.github/workflows/x.yml", ".github/workflows/.yml@main"} {
		rec := request(t, h, http.MethodPatch, "/api/apps/blog", fmt.Sprintf(`{"auto_deploy_workflow": %q}`, bad))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "auto_deploy_workflow") {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"auto_deploy_workflow": ""}`); rec.Code != http.StatusOK {
		t.Errorf("clearing: %d %s", rec.Code, rec.Body)
	}
	// The change is in the activity.
	rec = request(t, h, http.MethodGet, "/api/apps/blog/activity", "")
	if !strings.Contains(rec.Body.String(), `"field":"auto_deploy_workflow"`) {
		t.Errorf("activity: %s", rec.Body)
	}
}
