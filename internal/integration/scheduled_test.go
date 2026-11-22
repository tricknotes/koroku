//go:build integration

package integration

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// A web service that depends on a db service, and a worker nothing
// depends on.
const scheduledCompose = `services:
  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo web > /www/index.html && exec httpd -f -p 8080 -h /www"]
    depends_on:
      - db
  db:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo db-alive > /www/index.html && exec httpd -f -p 5432 -h /www"]
  worker:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sleep", "infinity"]
`

func containerStartedAt(t *testing.T, project, service string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--all", "--quiet",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service,
		"--filter", "label=com.docker.compose.oneoff=False").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return ""
	}
	out, err = exec.Command("docker", "container", "inspect", "--format", "{{.State.StartedAt}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestScheduledTaskOfASleepingApp runs a scheduled task while the app
// sleeps (ARCHITECTURE 9.6, 8.4): only the service the task's service
// depends on starts, the command reaches it, and everything is stopped
// again with the app still asleep. The web service and the worker never
// start.
func TestScheduledTaskOfASleepingApp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "image-app")
	source.write("compose.yaml", scheduledCompose)
	sha := source.commit("Web with a db")
	slug := e.slug("cron")
	project := "koroku-" + slug
	e.createApp(slug, source.url())
	if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if status := e.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"idle_sleep": true}, nil); status != http.StatusOK {
		t.Fatalf("enable idle sleep: status %d", status)
	}
	ctx := context.Background()
	app, err := e.db.GetApp(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.engine.Sleep(ctx, app); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	app, err = e.db.GetApp(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	sleepingSince := app.SleepingSince
	webStarted := containerStartedAt(t, project, "web")
	workerStarted := containerStartedAt(t, project, "worker")
	if webStarted == "" || workerStarted == "" {
		t.Fatalf("the containers must stay while asleep: %v", containerStates(t, project))
	}

	run, err := e.engine.RunTask(ctx, app, deploy.TaskSpec{
		TaskName: "nightly", Service: "web", Command: "wget -qO- http://db:5432/",
		Scheduled: true, DueAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if err := e.engine.WaitTask(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	done, err := e.db.GetTaskRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	log := e.taskLog(run.ID)
	if done.Status != store.StatusSucceeded || done.Trigger != store.TaskTriggerScheduled {
		t.Fatalf("run = %+v\n%s", done, log)
	}
	for _, want := range []string{"starting only db.", "db-alive\n", "Stopped db again.", "Exited with code 0."} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}

	for service, state := range containerStates(t, project) {
		if state != "exited" {
			t.Errorf("after the run, %s is %s", service, state)
		}
	}
	if got := containerStartedAt(t, project, "web"); got != webStarted {
		t.Errorf("the web service was started (%s, before %s)", got, webStarted)
	}
	if got := containerStartedAt(t, project, "worker"); got != workerStarted {
		t.Errorf("the worker was started (%s, before %s)", got, workerStarted)
	}
	after, err := e.db.GetApp(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if after.SleepingSince == nil || !after.SleepingSince.Equal(*sleepingSince) {
		t.Errorf("the app must stay asleep: %v, before %v", after.SleepingSince, sleepingSince)
	}
}
