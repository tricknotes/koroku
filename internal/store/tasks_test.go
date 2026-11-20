package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestReplaceAndListTasks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "shop")

	tasks, err := s.ListTasks(ctx, app.ID)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("ListTasks = %v, %v", tasks, err)
	}
	want := []Task{
		{Name: "migrate status", Service: "web", Command: "bin/rails db:migrate:status"},
		{Name: "vacuum", Service: "db", Command: "vacuumdb -U postgres app"},
	}
	if err := s.ReplaceTasks(ctx, app.ID, want); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, other.ID, want[:1]); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListTasks(ctx, app.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListTasks = %v, %v", got, err)
	}
	task, err := s.GetTask(ctx, app.ID, "vacuum")
	if err != nil || task != want[1] {
		t.Fatalf("GetTask = %v, %v", task, err)
	}
	if _, err := s.GetTask(ctx, app.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask(missing) err = %v", err)
	}

	// The order is the one given; a reordered list replaces the old one.
	reordered := []Task{want[1], want[0]}
	if err := s.ReplaceTasks(ctx, app.ID, reordered); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListTasks(ctx, app.ID); !reflect.DeepEqual(got, reordered) {
		t.Fatalf("after reorder = %v", got)
	}

	// A duplicate name leaves the previous list untouched.
	err = s.ReplaceTasks(ctx, app.ID, []Task{want[0], want[0]})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate err = %v", err)
	}
	if got, _ := s.ListTasks(ctx, app.ID); !reflect.DeepEqual(got, reordered) {
		t.Fatalf("after failed replace = %v", got)
	}

	// Other apps keep theirs; deleting the app removes its tasks.
	if err := s.DeleteApp(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListTasks(ctx, other.ID); len(got) != 1 {
		t.Fatalf("other app tasks = %v", got)
	}
}

func TestTaskRunLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	d := enqueue(t, s, app.ID, "aaa")

	run, err := s.StartTaskRun(ctx, TaskRun{
		AppID: app.ID, TaskName: "migrate", Service: "web", Command: "bin/rails db:migrate", DeploymentID: &d.ID,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusRunning || run.ExitCode != nil || run.FinishedAt != nil ||
		!run.StartedAt.Equal(testNow) || run.DeploymentID == nil || *run.DeploymentID != d.ID {
		t.Fatalf("started = %+v", run)
	}

	code := 3
	later := testNow.Add(time.Minute)
	if err := s.FinishTaskRun(ctx, run.ID, StatusFailed, &code, "exited with code 3", later); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTaskRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.ExitCode == nil || *got.ExitCode != 3 ||
		got.Error != "exited with code 3" || got.FinishedAt == nil || !got.FinishedAt.Equal(later) {
		t.Fatalf("finished = %+v", got)
	}
	// A finished run stays finished.
	if err := s.FinishTaskRun(ctx, run.ID, StatusSucceeded, nil, "", later); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second finish err = %v", err)
	}
	if err := s.FinishTaskRun(ctx, run.ID, StatusRunning, nil, "", later); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("finish as running err = %v", err)
	}
	if _, err := s.GetTaskRun(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTaskRun(missing) err = %v", err)
	}
}

func TestFailInterruptedTaskRuns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	running, _ := s.StartTaskRun(ctx, TaskRun{AppID: app.ID, Service: "web", Command: "sleep 100"}, testNow)
	done, _ := s.StartTaskRun(ctx, TaskRun{AppID: app.ID, Service: "web", Command: "true"}, testNow)
	zero := 0
	if err := s.FinishTaskRun(ctx, done.ID, StatusSucceeded, &zero, "", testNow); err != nil {
		t.Fatal(err)
	}

	runs, err := s.FailInterruptedTaskRuns(ctx, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != running.ID {
		t.Fatalf("interrupted = %+v", runs)
	}
	got, _ := s.GetTaskRun(ctx, running.ID)
	if got.Status != StatusFailed || got.Error != InterruptedTaskError || got.FinishedAt == nil {
		t.Fatalf("interrupted run = %+v", got)
	}
	if got, _ := s.GetTaskRun(ctx, done.ID); got.Status != StatusSucceeded {
		t.Fatalf("finished run changed: %+v", got)
	}
}

func TestPruneTaskRuns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "shop")
	zero := 0

	// The oldest run is still going: it is never pruned.
	oldest, _ := s.StartTaskRun(ctx, TaskRun{AppID: app.ID, Service: "web", Command: "sleep"}, testNow)
	var ids []int64
	for i := 0; i < 4; i++ {
		run, err := s.StartTaskRun(ctx, TaskRun{AppID: app.ID, Service: "web", Command: "true"}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishTaskRun(ctx, run.ID, StatusSucceeded, &zero, "", testNow); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	if _, err := s.StartTaskRun(ctx, TaskRun{AppID: other.ID, Service: "web", Command: "true"}, testNow); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.PruneTaskRuns(ctx, app.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []int64{ids[1], ids[0]}) {
		t.Fatalf("deleted = %v, want %v", deleted, []int64{ids[1], ids[0]})
	}
	runs, _ := s.ListTaskRuns(ctx, app.ID, 50)
	var left []int64
	for _, r := range runs {
		left = append(left, r.ID)
	}
	if !reflect.DeepEqual(left, []int64{ids[3], ids[2], oldest.ID}) {
		t.Fatalf("left = %v", left)
	}
	if runs, _ := s.ListTaskRuns(ctx, other.ID, 50); len(runs) != 1 {
		t.Fatalf("other app runs = %v", runs)
	}
}
