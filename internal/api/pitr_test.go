package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/pitr"
	"github.com/tricknotes/koroku/internal/store"
)

type fakePITR struct {
	mu       sync.Mutex
	status   pitr.Status
	disabled chan string
}

func (f *fakePITR) Status(_ context.Context, app store.App) (pitr.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status
	st.Service = app.PITRService
	return st, nil
}

func (f *fakePITR) Disable(_ context.Context, _ store.App, service string) error {
	f.disabled <- service
	return nil
}

func TestPITRSettingAndStatus(t *testing.T) {
	fake := &fakePITR{disabled: make(chan string, 1)}
	srv := newTestServer(t, func(o *Options) { o.PITR = fake })
	h := srv.Handler()
	createApp(t, h, "blog")

	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"pitr_service":"bad name"}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "pitr_service") {
		t.Errorf("bad name: %d %s", rec.Code, rec.Body)
	}
	rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"pitr_service":"db"}`)
	if rec.Code != http.StatusOK || decode[appJSON](t, rec).PITRService != "db" {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}

	earliest := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	end := earliest.Add(time.Hour)
	fake.status = pitr.Status{State: pitr.StateReady, EarliestAt: &earliest, ArchivedUntil: &end,
		Gaps: []store.PITRGap{{StartedAt: earliest, EndedAt: &end, Reason: store.PITRGapMountMissing}}}
	got := decode[pitrStatusJSON](t, request(t, h, http.MethodGet, "/api/apps/blog/pitr", ""))
	if got.Service != "db" || got.State != "ready" || !got.EarliestAt.Equal(earliest) || len(got.Gaps) != 1 || got.Gaps[0].Reason != "mount_missing" {
		t.Errorf("status = %+v", got)
	}

	// Turning it off resets the database's settings, off the request.
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"pitr_service":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	select {
	case service := <-fake.disabled:
		if service != "db" {
			t.Errorf("disabled %q", service)
		}
	case <-time.After(5 * time.Second):
		t.Error("the archiving of db was not turned off")
	}
}

// Without the service (no backups), the status says so.
func TestPITRStatusWithoutBackups(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	if got := decode[pitrStatusJSON](t, request(t, h, http.MethodGet, "/api/apps/blog/pitr", "")); got.State != "off" {
		t.Errorf("off: %+v", got)
	}
	request(t, h, http.MethodPatch, "/api/apps/blog", `{"pitr_service":"db"}`)
	if got := decode[pitrStatusJSON](t, request(t, h, http.MethodGet, "/api/apps/blog/pitr", "")); got.State != "unavailable" {
		t.Errorf("unavailable: %+v", got)
	}
}

type fakeRestorer struct {
	store   *store.Store
	err     error
	targets []time.Time
}

func (f *fakeRestorer) StartPITRRestore(ctx context.Context, app store.App, target time.Time) (store.PITRRestore, error) {
	f.targets = append(f.targets, target)
	if f.err != nil {
		return store.PITRRestore{}, f.err
	}
	return f.store.StartPITRRestore(ctx, app, target, time.Date(2026, 11, 28, 3, 0, 0, 0, time.UTC))
}

func (f *fakeRestorer) PITRRestoreLogPath(slug string, id int64) string { return "/nonexistent" }

func TestStartPITRRestore(t *testing.T) {
	var restorer *fakeRestorer
	srv := newTestServer(t, func(o *Options) {
		restorer = &fakeRestorer{store: o.Store}
		o.PITRRestorer = restorer
	})
	h := srv.Handler()
	createApp(t, h, "blog")
	createApp(t, h, "shop")
	request(t, h, http.MethodPatch, "/api/apps/blog", `{"pitr_service":"db"}`)

	rec := request(t, h, http.MethodPost, "/api/apps/blog/pitr/restores", `{"target_time":"2026-11-27T10:15:30+09:00"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	got := decode[pitrRestoreJSON](t, rec)
	if got.Status != "running" || got.Service != "db" || !got.TargetTime.Equal(time.Date(2026, 11, 27, 1, 15, 30, 0, time.UTC)) {
		t.Errorf("restore = %+v", got)
	}
	list := decode[struct {
		Restores []pitrRestoreJSON `json:"restores"`
	}](t, request(t, h, http.MethodGet, "/api/apps/blog/pitr/restores", ""))
	if len(list.Restores) != 1 {
		t.Errorf("list = %+v", list)
	}
	if rec := request(t, h, http.MethodGet, fmt.Sprintf("/api/apps/blog/pitr/restores/%d", got.ID), ""); rec.Code != http.StatusOK {
		t.Errorf("get: %d", rec.Code)
	}
	// Another app's restore is not found under this one.
	if rec := request(t, h, http.MethodGet, fmt.Sprintf("/api/apps/shop/pitr/restores/%d", got.ID), ""); rec.Code != http.StatusNotFound {
		t.Errorf("other app: %d", rec.Code)
	}

	if rec := request(t, h, http.MethodPost, "/api/apps/blog/pitr/restores", `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("no time: %d", rec.Code)
	}
	request(t, h, http.MethodPatch, "/api/apps/blog", `{"environment":"production"}`)
	if rec := request(t, h, http.MethodPost, "/api/apps/blog/pitr/restores", `{"target_time":"2026-11-27T01:00:00Z"}`); errorCode(t, rec) != "confirmation_mismatch" {
		t.Errorf("production without the slug: %d %s", rec.Code, rec.Body)
	}
	for err, code := range map[error]string{
		pitr.ErrInGap:          "target_in_gap",
		pitr.ErrNoBase:         "target_out_of_range",
		pitr.ErrFutureTarget:   "target_in_future",
		deploy.ErrPITRNotReady: "pitr_not_ready",
		deploy.ErrBusy:         "busy",
	} {
		restorer.err = fmt.Errorf("plan: %w", err)
		rec := request(t, h, http.MethodPost, "/api/apps/blog/pitr/restores", `{"target_time":"2026-11-27T01:00:00Z","confirm":"blog"}`)
		if errorCode(t, rec) != code {
			t.Errorf("%v: %d %s", err, rec.Code, rec.Body)
		}
	}
}
