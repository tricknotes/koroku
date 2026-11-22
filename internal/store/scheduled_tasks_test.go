package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestScheduledTasksAndSkips(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "shop")
	if err := s.ReplaceTasks(ctx, app.ID, []Task{
		{Name: "by hand", Service: "web", Command: "true"},
		{Name: "nightly", Service: "web", Command: "bin/rails runner 'Cleanup.perform_now'", Schedule: "0 3 * * *"},
		{Name: "hourly", Service: "web", Command: "true", Schedule: "0 * * * *"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, other.ID, []Task{{Name: "sweep", Service: "web", Command: "true", Schedule: "*/5 * * * *"}}); err != nil {
		t.Fatal(err)
	}
	scheduled, err := s.ListScheduledTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, st := range scheduled {
		names = append(names, st.Name)
	}
	if !reflect.DeepEqual(names, []string{"nightly", "hourly", "sweep"}) || scheduled[0].AppID != app.ID || scheduled[2].AppID != other.ID {
		t.Fatalf("ListScheduledTasks = %+v", scheduled)
	}

	at := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	if err := s.RecordTaskSkip(ctx, app.ID, "nightly", "0 3 * * *", at, TaskSkipStopped, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskSkip(ctx, app.ID, "hourly", "0 * * * *", at, TaskSkipError, "policy"); err != nil {
		t.Fatal(err)
	}
	// A skip for a schedule that changed meanwhile is not recorded.
	if err := s.RecordTaskSkip(ctx, app.ID, "by hand", "0 3 * * *", at, TaskSkipBusy, ""); err != nil {
		t.Fatal(err)
	}
	task, err := s.GetTask(ctx, app.ID, "nightly")
	if err != nil || task.LastSkippedAt == nil || !task.LastSkippedAt.Equal(at) || task.LastSkipReason != TaskSkipStopped {
		t.Fatalf("nightly = %+v, %v", task, err)
	}
	if task, _ := s.GetTask(ctx, app.ID, "by hand"); task.LastSkippedAt != nil {
		t.Fatalf("by hand = %+v", task)
	}

	// Saving again keeps the skip of a task whose name and schedule stay,
	// and drops it when the schedule changes.
	if err := s.ReplaceTasks(ctx, app.ID, []Task{
		{Name: "nightly", Service: "web", Command: "changed", Schedule: "0 3 * * *"},
		{Name: "hourly", Service: "web", Command: "true", Schedule: "30 * * * *"},
	}); err != nil {
		t.Fatal(err)
	}
	if task, _ := s.GetTask(ctx, app.ID, "nightly"); task.LastSkippedAt == nil || task.LastSkipReason != TaskSkipStopped {
		t.Errorf("nightly after save = %+v", task)
	}
	if task, _ := s.GetTask(ctx, app.ID, "hourly"); task.LastSkippedAt != nil || task.LastSkipReason != "" || task.LastSkipDetail != "" {
		t.Errorf("hourly after a new schedule = %+v", task)
	}
}

func TestTaskRunTrigger(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	now := time.Now()
	manual, err := s.StartTaskRun(ctx, TaskRun{AppID: app.ID, Service: "web", Command: "true"}, now)
	if err != nil || manual.Trigger != TaskTriggerManual {
		t.Fatalf("manual = %+v, %v", manual, err)
	}
	scheduled, err := s.StartTaskRun(ctx, TaskRun{AppID: app.ID, TaskName: "nightly", Service: "web", Command: "true", Trigger: TaskTriggerScheduled}, now)
	if err != nil || scheduled.Trigger != TaskTriggerScheduled || scheduled.Actor != nil {
		t.Fatalf("scheduled = %+v, %v", scheduled, err)
	}
}
