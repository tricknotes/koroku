package store

import (
	"context"
	"testing"
	"time"
)

func TestIdleSleepSettingsAndState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	app := mustCreateApp(t, s, "blog")
	if app.IdleSleep || app.IdleSleepMinutes != DefaultIdleSleepMinutes || app.SleepingSince != nil {
		t.Fatalf("new app = idle %v, %d min, sleeping %v", app.IdleSleep, app.IdleSleepMinutes, app.SleepingSince)
	}
	app.IdleSleep, app.IdleSleepMinutes = true, 60
	if err := s.UpdateApp(ctx, app, testNow); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}

	// A never deployed app cannot sleep.
	if ok, err := s.MarkAppSleeping(ctx, app.ID, testNow); err != nil || ok {
		t.Fatalf("MarkAppSleeping before a deployment = %v, %v", ok, err)
	}
	d := enqueue(t, s, app.ID, "aaa")
	if err := s.StartDeployment(ctx, d.ID, testNow); err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	if err := s.FinishDeployment(ctx, d.ID, StatusSucceeded, "", testNow); err != nil {
		t.Fatalf("FinishDeployment: %v", err)
	}

	at := testNow.Add(time.Hour)
	if ok, err := s.MarkAppSleeping(ctx, app.ID, at); err != nil || !ok {
		t.Fatalf("MarkAppSleeping = %v, %v", ok, err)
	}
	if ok, err := s.MarkAppSleeping(ctx, app.ID, at.Add(time.Minute)); err != nil || ok {
		t.Fatalf("MarkAppSleeping twice = %v, %v", ok, err)
	}
	got, err := s.GetApp(ctx, "blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if !got.IdleSleep || got.IdleSleepMinutes != 60 || got.SleepingSince == nil || !got.SleepingSince.Equal(at) {
		t.Fatalf("app = idle %v, %d min, sleeping %v", got.IdleSleep, got.IdleSleepMinutes, got.SleepingSince)
	}
	// Saving the settings leaves the runtime state alone.
	if err := s.UpdateApp(ctx, got, testNow); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	if got, _ = s.GetApp(ctx, "blog"); got.SleepingSince == nil {
		t.Fatal("UpdateApp cleared the sleeping state")
	}

	if ok, err := s.MarkAppAwake(ctx, app.ID); err != nil || !ok {
		t.Fatalf("MarkAppAwake = %v, %v", ok, err)
	}
	if ok, err := s.MarkAppAwake(ctx, app.ID); err != nil || ok {
		t.Fatalf("MarkAppAwake twice = %v, %v", ok, err)
	}

	// Stopping ends a sleep, and a stopped app does not go to sleep.
	if _, err := s.MarkAppSleeping(ctx, app.ID, at); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppDesiredState(ctx, app.ID, DesiredStopped, at); err != nil {
		t.Fatalf("SetAppDesiredState: %v", err)
	}
	if got, _ = s.GetApp(ctx, "blog"); got.SleepingSince != nil {
		t.Fatal("stopping kept the sleeping state")
	}
	if ok, err := s.MarkAppSleeping(ctx, app.ID, at); err != nil || ok {
		t.Fatalf("MarkAppSleeping of a stopped app = %v, %v", ok, err)
	}
}
