//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type taskRun struct {
	ID       int64  `json:"id"`
	TaskName string `json:"task_name"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Error    string `json:"error"`
}

func (e *env) runTask(slug string, body map[string]string) taskRun {
	e.t.Helper()
	var run taskRun
	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/task-runs", body, &run); status != http.StatusAccepted {
		e.t.Fatalf("run %v: status %d", body, status)
	}
	return run
}

func (e *env) waitTask(id int64) taskRun {
	e.t.Helper()
	var run taskRun
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		e.call(http.MethodGet, fmt.Sprintf("/api/task-runs/%d", id), nil, &run)
		if run.Status != "running" {
			return run
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatalf("task run %d did not finish:\n%s", id, e.taskLog(id))
	return run
}

func (e *env) taskLog(id int64) string {
	e.t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/task-runs/%d/logs?format=text", e.server.URL, id))
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

// containerMemory returns the memory limit of a container, or "" when the
// container does not exist.
func containerMemory(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("docker", "container", "inspect", "--format", "{{.HostConfig.Memory}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestTasks runs commands in a deployed app (ARCHITECTURE 9.6): a
// registered task and an ad-hoc command get the app's environment with its
// values masked in the output, the exit code is recorded, and a canceled
// run leaves no container behind while the app keeps answering.
func TestTasks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "image-app")
	sha := source.commit("Image-only app")
	slug := e.slug("tasks")
	app := e.createApp(slug, source.url())

	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/task-runs",
		map[string]string{"service": "web", "command": "true"}, nil); status != http.StatusConflict {
		t.Errorf("run before the first deploy: status %d, want 409", status)
	}

	e.setEnv(slug, map[string]string{"GREETING": "greeting-secret-value"})
	if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}

	tasks := map[string]any{"tasks": []map[string]string{{
		"name": "greet", "service": "web",
		"command": `echo "hello $GREETING"; echo "to stderr" >&2; exit 3`,
	}}}
	if status := e.call(http.MethodPut, "/api/apps/"+slug+"/tasks", tasks, nil); status != http.StatusOK {
		t.Fatalf("put tasks: status %d", status)
	}

	// A registered task: the env file reaches the container, the output
	// is masked and the exit code is kept.
	run := e.waitTask(e.runTask(slug, map[string]string{"task": "greet"}).ID)
	log := e.taskLog(run.ID)
	if run.Status != "failed" || run.ExitCode == nil || *run.ExitCode != 3 || run.TaskName != "greet" {
		t.Fatalf("run = %+v\n%s", run, log)
	}
	for _, want := range []string{"$ echo \"hello $GREETING\"", "hello [MASKED]\n", "to stderr\n", "Exited with code 3.\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "greeting-secret-value") {
		t.Errorf("the value must be masked:\n%s", log)
	}

	// An ad-hoc command that succeeds, in a fresh container of the same
	// image (the web container's own files are not there).
	ok := e.waitTask(e.runTask(slug, map[string]string{
		"service": "web", "command": "test ! -e /www/index.html && echo fresh-container",
	}).ID)
	if ok.Status != "succeeded" || ok.ExitCode == nil || *ok.ExitCode != 0 {
		t.Fatalf("run = %+v\n%s", ok, e.taskLog(ok.ID))
	}
	if !strings.Contains(e.taskLog(ok.ID), "fresh-container\n") {
		t.Errorf("log:\n%s", e.taskLog(ok.ID))
	}

	// A long command holds the app, runs with the service's memory limit
	// and can be canceled.
	long := e.runTask(slug, map[string]string{"service": "web", "command": "sleep 300"})
	container := fmt.Sprintf("koroku-%s-task-%d", slug, long.ID)
	deadline := time.Now().Add(time.Minute)
	for containerMemory(t, container) == "" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if got := containerMemory(t, container); got != fmt.Sprint(512<<20) {
		t.Errorf("memory limit of the run container = %q", got)
	}
	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/deployments", map[string]string{"sha": sha}, nil); status != http.StatusConflict {
		t.Errorf("deploy while a command runs: status %d, want 409", status)
	}
	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/stop", nil, nil); status != http.StatusConflict {
		t.Errorf("stop while a command runs: status %d, want 409", status)
	}
	if status := e.call(http.MethodPost, fmt.Sprintf("/api/task-runs/%d/cancel", long.ID), nil, nil); status != http.StatusAccepted {
		t.Fatalf("cancel: status %d", status)
	}
	canceled := e.waitTask(long.ID)
	if canceled.Status != "canceled" {
		t.Errorf("canceled run = %+v\n%s", canceled, e.taskLog(long.ID))
	}
	if containerMemory(t, container) != "" {
		t.Errorf("the container of the canceled run is still there")
	}

	// The app was never touched.
	if got := page(t, app.HostPort)[""]; got != "image-app greeting=greeting-secret-value" {
		t.Errorf("page = %q", got)
	}
	var runs struct {
		Runs []taskRun `json:"runs"`
	}
	e.call(http.MethodGet, "/api/apps/"+slug+"/task-runs", nil, &runs)
	if len(runs.Runs) != 3 || runs.Runs[0].ID != long.ID {
		t.Errorf("runs = %+v", runs.Runs)
	}
}
