package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

func (f *fakeDeployer) RunTask(ctx context.Context, app store.App, spec deploy.TaskSpec) (store.TaskRun, error) {
	f.mu.Lock()
	err := f.taskErr
	f.taskSpecs = append(f.taskSpecs, spec)
	f.mu.Unlock()
	if err != nil {
		return store.TaskRun{}, err
	}
	return f.store.StartTaskRun(ctx, store.TaskRun{
		AppID: app.ID, TaskName: spec.TaskName, Service: spec.Service, Command: spec.Command,
		DeploymentID: app.CurrentDeploymentID,
	}, time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC))
}

func (f *fakeDeployer) CancelTask(ctx context.Context, id int64) error {
	run, err := f.store.GetTaskRun(ctx, id)
	if err != nil {
		return err
	}
	if store.IsTerminal(run.Status) {
		return deploy.ErrNotCancelable
	}
	f.mu.Lock()
	f.canceled = append(f.canceled, id)
	f.mu.Unlock()
	return nil
}

func (f *fakeDeployer) TaskLogPath(slug string, id int64) string {
	return filepath.Join(f.logDir, fmt.Sprintf("%s-task-%d.log", slug, id))
}

func TestRegisteredTasks(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")

	if rec := request(t, h, http.MethodGet, "/api/apps/blog/tasks", ""); strings.TrimSpace(rec.Body.String()) != `{"tasks":[]}` {
		t.Errorf("no tasks: %d %s", rec.Code, rec.Body)
	}
	body := `{"tasks": [
		{"name": " マイグレーションの状態 ", "service": "web", "command": "  bin/rails db:migrate:status "},
		{"name": "vacuum", "service": "db", "command": "vacuumdb -U postgres app"}
	]}`
	rec := request(t, h, http.MethodPut, "/api/apps/blog/tasks", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	want := `{"tasks":[{"name":"マイグレーションの状態","service":"web","command":"bin/rails db:migrate:status"},` +
		`{"name":"vacuum","service":"db","command":"vacuumdb -U postgres app"}]}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("put response = %s", got)
	}
	if got := strings.TrimSpace(request(t, h, http.MethodGet, "/api/apps/blog/tasks", "").Body.String()); got != want {
		t.Errorf("get = %s", got)
	}

	for name, bad := range map[string]string{
		"missing list":   `{}`,
		"empty name":     `{"tasks": [{"name": "", "service": "web", "command": "true"}]}`,
		"long name":      fmt.Sprintf(`{"tasks": [{"name": %q, "service": "web", "command": "true"}]}`, strings.Repeat("あ", 61)),
		"duplicate name": `{"tasks": [{"name": "a", "service": "web", "command": "true"}, {"name": "a", "service": "web", "command": "true"}]}`,
		"bad service":    `{"tasks": [{"name": "a", "service": "web db", "command": "true"}]}`,
		"empty command":  `{"tasks": [{"name": "a", "service": "web", "command": " "}]}`,
		"newline":        `{"tasks": [{"name": "a", "service": "web", "command": "true\nfalse"}]}`,
		"long command":   fmt.Sprintf(`{"tasks": [{"name": "a", "service": "web", "command": %q}]}`, strings.Repeat("x", 2001)),
		"too many":       `{"tasks": [` + strings.TrimSuffix(strings.Repeat(`{"name": "n", "service": "web", "command": "true"},`, 21), ",") + `]}`,
	} {
		rec := request(t, h, http.MethodPut, "/api/apps/blog/tasks", bad)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "validation_failed") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	dup := request(t, h, http.MethodPut, "/api/apps/blog/tasks",
		`{"tasks": [{"name": "a", "service": "web", "command": "true"}, {"name": "a", "service": "web", "command": "true"}]}`)
	if !strings.Contains(dup.Body.String(), `"tasks[1].name"`) {
		t.Errorf("the field of the problem: %s", dup.Body)
	}
	// Nothing was saved by the refused requests.
	if got := strings.TrimSpace(request(t, h, http.MethodGet, "/api/apps/blog/tasks", "").Body.String()); got != want {
		t.Errorf("after refusals = %s", got)
	}
	if rec := request(t, h, http.MethodPut, "/api/apps/blog/tasks", `{"tasks": []}`); rec.Code != http.StatusOK {
		t.Errorf("clearing: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/missing/tasks", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing app: %d", rec.Code)
	}
}

func TestCreateTaskRun(t *testing.T) {
	srv := newTestServer(t, nil)
	fake := srv.tasks.(*fakeDeployer)
	h := srv.Handler()
	createApp(t, h, "blog")
	request(t, h, http.MethodPut, "/api/apps/blog/tasks",
		`{"tasks": [{"name": "status", "service": "web", "command": "bin/rails db:migrate:status"}]}`)

	rec := request(t, h, http.MethodPost, "/api/apps/blog/task-runs", `{"task": "status"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run task: %d %s", rec.Code, rec.Body)
	}
	run := decode[taskRunJSON](t, rec)
	if run.AppSlug != "blog" || run.TaskName != "status" || run.Service != "web" ||
		run.Command != "bin/rails db:migrate:status" || run.Status != "running" || run.ExitCode != nil {
		t.Errorf("run = %+v", run)
	}
	adhoc := decode[taskRunJSON](t, request(t, h, http.MethodPost, "/api/apps/blog/task-runs",
		`{"service": " db ", "command": " psql -c 'select 1' "}`))
	if adhoc.TaskName != "" || adhoc.Service != "db" || adhoc.Command != "psql -c 'select 1'" {
		t.Errorf("ad-hoc run = %+v", adhoc)
	}
	if len(fake.taskSpecs) != 2 || fake.taskSpecs[0] != (deploy.TaskSpec{
		TaskName: "status", Service: "web", Command: "bin/rails db:migrate:status",
	}) {
		t.Errorf("specs = %+v", fake.taskSpecs)
	}

	list := decode[struct {
		Runs []taskRunJSON `json:"runs"`
	}](t, request(t, h, http.MethodGet, "/api/apps/blog/task-runs", ""))
	if len(list.Runs) != 2 || list.Runs[0].ID != adhoc.ID || list.Runs[1].ID != run.ID {
		t.Errorf("runs = %+v", list.Runs)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/blog/task-runs?limit=0", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0: %d", rec.Code)
	}
	if got := decode[taskRunJSON](t, request(t, h, http.MethodGet, fmt.Sprintf("/api/task-runs/%d", run.ID), "")); got.ID != run.ID {
		t.Errorf("get = %+v", got)
	}
	for _, target := range []string{"/api/task-runs/999", "/api/task-runs/x"} {
		if rec := request(t, h, http.MethodGet, target, ""); rec.Code != http.StatusNotFound ||
			!strings.Contains(rec.Body.String(), "task_run_not_found") {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}

	for name, bad := range map[string]string{
		"nothing":       `{}`,
		"both":          `{"task": "status", "service": "web", "command": "true"}`,
		"empty task":    `{"task": ""}`,
		"no command":    `{"service": "web"}`,
		"no service":    `{"command": "true"}`,
		"bad service":   `{"service": "-web", "command": "true"}`,
		"multi-line":    `{"service": "web", "command": "true\nfalse"}`,
		"control chars": `{"service": "web", "command": "a\u0000b"}`,
	} {
		rec := request(t, h, http.MethodPost, "/api/apps/blog/task-runs", bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := request(t, h, http.MethodPost, "/api/apps/blog/task-runs", `{"task": "missing"}`); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "task_not_found") {
		t.Errorf("missing task: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateTaskRunErrors(t *testing.T) {
	srv := newTestServer(t, nil)
	fake := srv.tasks.(*fakeDeployer)
	h := srv.Handler()
	createApp(t, h, "blog")
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{deploy.ErrBusy, http.StatusConflict, "busy"},
		{deploy.ErrNotDeployed, http.StatusConflict, "not_deployed"},
		{&deploy.UnknownServiceError{Service: "worker"}, http.StatusUnprocessableEntity, `"service"`},
		{&compose.PolicyError{Violations: []compose.Violation{{Rule: "privileged", Service: "web"}}},
			http.StatusUnprocessableEntity, "policy_violation"},
		{fmt.Errorf("docker: secret-detail"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		fake.taskErr = tc.err
		rec := request(t, h, http.MethodPost, "/api/apps/blog/task-runs", `{"service": "web", "command": "true"}`)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
			t.Errorf("%v: %d %s", tc.err, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "secret-detail") {
			t.Errorf("internals leaked: %s", rec.Body)
		}
	}
}

func TestCancelTaskRunAndLogs(t *testing.T) {
	srv := newTestServer(t, nil)
	fake := srv.tasks.(*fakeDeployer)
	h := srv.Handler()
	createApp(t, h, "blog")
	run := decode[taskRunJSON](t, request(t, h, http.MethodPost, "/api/apps/blog/task-runs",
		`{"service": "web", "command": "sleep 100"}`))

	cancel := request(t, h, http.MethodPost, fmt.Sprintf("/api/task-runs/%d/cancel", run.ID), "")
	if cancel.Code != http.StatusAccepted || len(fake.canceled) != 1 || fake.canceled[0] != run.ID {
		t.Fatalf("cancel: %d %s %v", cancel.Code, cancel.Body, fake.canceled)
	}

	path := fake.TaskLogPath("blog", run.ID)
	if err := os.WriteFile(path, []byte("$ sleep 100\nCanceled.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishTaskRun(context.Background(), run.ID, store.StatusCanceled, nil, "canceled by user", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := request(t, h, http.MethodPost, fmt.Sprintf("/api/task-runs/%d/cancel", run.ID), ""); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "not_cancelable") {
		t.Errorf("cancel after the end: %d %s", rec.Code, rec.Body)
	}

	sse := request(t, h, http.MethodGet, fmt.Sprintf("/api/task-runs/%d/logs", run.ID), "")
	want := "id: 12\ndata: $ sleep 100\n\nid: 22\ndata: Canceled.\n\nevent: end\ndata: canceled\n\n"
	if sse.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" || sse.Body.String() != want {
		t.Errorf("sse = %q", sse.Body)
	}
	text := request(t, h, http.MethodGet, fmt.Sprintf("/api/task-runs/%d/logs?format=text", run.ID), "")
	if text.Body.String() != "$ sleep 100\nCanceled.\n" {
		t.Errorf("text = %q", text.Body)
	}
}

func TestDeploymentIsRefusedWhileACommandRuns(t *testing.T) {
	srv := newTestServer(t, nil)
	fake := srv.deployer.(*fakeDeployer)
	h := srv.Handler()
	createApp(t, h, "blog")
	fake.enqueueErr = deploy.ErrBusy
	rec := request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"ref": "main"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Errorf("deploy: %d %s", rec.Code, rec.Body)
	}
}

func TestProductionTasksNeedRecentLogin(t *testing.T) {
	srv, clk := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	origin := map[string]string{"Origin": consoleURL}
	for _, slug := range []string{"blog", "shop"} {
		create := b.do(http.MethodPost, "/api/apps",
			fmt.Sprintf(`{"slug":%q,"web_port":3000,"repo_full_name":"octo/%s"}`, slug, slug), origin)
		if create.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", create.Code, create.Body)
		}
	}
	if rec := b.do(http.MethodPatch, "/api/apps/shop", `{"environment":"production"}`, origin); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	tasks := `{"tasks":[{"name":"status","service":"web","command":"true"}]}`
	for _, slug := range []string{"blog", "shop"} {
		if rec := b.do(http.MethodPut, "/api/apps/"+slug+"/tasks", tasks, origin); rec.Code != http.StatusOK {
			t.Fatalf("%s tasks within the window: %d %s", slug, rec.Code, rec.Body)
		}
	}

	clk.Advance(ReauthWindow)

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPut, "/api/apps/shop/tasks", `{"tasks":[]}`},
		{http.MethodPost, "/api/apps/shop/task-runs", `{"task":"status"}`},
		{http.MethodPost, "/api/apps/shop/task-runs", `{"service":"web","command":"true"}`},
	} {
		rec := b.do(c.method, c.target, c.body, origin)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "reauth_required") {
			t.Errorf("%s %s: %d %s", c.method, c.target, rec.Code, rec.Body)
		}
	}
	// Reading stays open, and staging needs no fresh login.
	if rec := b.do(http.MethodGet, "/api/apps/shop/tasks", "", nil); rec.Code != http.StatusOK {
		t.Errorf("get production tasks: %d", rec.Code)
	}
	if rec := b.do(http.MethodPut, "/api/apps/blog/tasks", `{"tasks":[]}`, origin); rec.Code != http.StatusOK {
		t.Errorf("staging tasks: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(http.MethodPost, "/api/apps/blog/task-runs", `{"service":"web","command":"true"}`, origin); rec.Code != http.StatusAccepted {
		t.Errorf("staging run: %d %s", rec.Code, rec.Body)
	}
}
