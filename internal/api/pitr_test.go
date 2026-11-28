package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
