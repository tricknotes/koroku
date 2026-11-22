package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

func TestMemoryReservationSettings(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")

	ok := decode[appJSON](t, request(t, h, http.MethodPatch, "/api/apps/blog",
		`{"memory_reservation": " 256M ", "service_memory_limits": {"db": "2g"}, "service_memory_reservations": {"db": "1g", "worker": "0"}}`))
	if ok.MemoryReservation != "256m" || ok.ServiceReservations["db"] != "1g" || ok.ServiceReservations["worker"] != "0" {
		t.Errorf("app = %+v", ok)
	}

	cases := map[string]struct{ body, field, message string }{
		// The configured default (512m) is the limit the app runs with.
		"above the default limit":  {`{"memory_reservation": "600m"}`, "memory_reservation", "(512m)"},
		"above the app limit":      {`{"memory_limit": "256m", "memory_reservation": "300m"}`, "memory_reservation", "(256m)"},
		"above the service limit":  {`{"service_memory_limits": {"db": "2g"}, "service_memory_reservations": {"db": "3g"}}`, "service_memory_reservations", "(2g)"},
		"below Docker's minimum":   {`{"service_memory_reservations": {"web": "4m"}}`, "service_memory_reservations", "at least 6m"},
		"not a size":               {`{"memory_reservation": "lots"}`, "memory_reservation", "number"},
		"empty service value":      {`{"service_memory_reservations": {"web": ""}}`, "service_memory_reservations", "empty"},
		"bad service name":         {`{"service_memory_reservations": {"we b": "64m"}}`, "service_memory_reservations", "service name"},
		"plain bytes below 6 MiB":  {`{"memory_reservation": "4096"}`, "memory_reservation", "at least 6m"},
		"negative is not accepted": {`{"memory_reservation": "-1m"}`, "memory_reservation", "number"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := request(t, h, http.MethodPatch, "/api/apps/blog", tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			body := decode[struct {
				Error struct{ Details map[string]string }
			}](t, rec)
			if got := body.Error.Details[tc.field]; !strings.Contains(got, tc.message) {
				t.Errorf("%s = %q, want it to mention %q", tc.field, got, tc.message)
			}
		})
	}
}

// TestSystemMemoryReservation: the system screen totals what the running
// releases reserve against the host's memory (ARCHITECTURE 6.2).
func TestSystemMemoryReservation(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.System = fakeProber{info: system.Info{Memory: &system.Memory{TotalBytes: 4 << 30, AvailableBytes: 1 << 30}}}
	})
	h := srv.Handler()
	ctx := context.Background()
	now := srv.clock.Now()
	release := func(slug string, reservations map[string]int64) {
		t.Helper()
		app, err := srv.store.GetApp(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := srv.store.EnqueueDeployment(ctx, app.ID, "main", testSHA, "", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.StartDeployment(ctx, d.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.SetDeploymentMemoryReservations(ctx, d.ID, reservations); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, slug := range []string{"shop", "blog", "wiki", "docs", "never"} {
		createApp(t, h, slug)
	}
	release("shop", map[string]int64{"web": 1 << 30, "db": 512 << 20})
	release("blog", map[string]int64{"web": 256 << 20})
	release("wiki", map[string]int64{}) // staging
	release("docs", map[string]int64{"web": 1 << 30})
	docs, _ := srv.store.GetApp(ctx, "docs")
	if err := srv.store.SetAppDesiredState(ctx, docs.ID, store.DesiredStopped, now); err != nil {
		t.Fatal(err)
	}

	type summary struct {
		MemoryReservation *memoryReservationJSON `json:"memory_reservation"`
	}
	got := decode[summary](t, request(t, h, http.MethodGet, "/api/system", "")).MemoryReservation
	if got == nil {
		t.Fatal("the owner gets the reservation summary")
	}
	if got.TotalBytes != 1792<<20 || got.MarginBytes != 1<<30 || got.Warning {
		t.Errorf("summary = %+v", got)
	}
	if len(got.Apps) != 2 || got.Apps[0].Slug != "shop" || got.Apps[0].TotalBytes != 1536<<20 ||
		got.Apps[0].Services["db"] != 512<<20 || got.Apps[1].Slug != "blog" {
		t.Errorf("apps = %+v (a stopped app, a staging app and an undeployed app are left out)", got.Apps)
	}

	// Reservations plus the margin above the host's memory warn.
	release("blog", map[string]int64{"web": 1600 << 20})
	got = decode[summary](t, request(t, h, http.MethodGet, "/api/system", "")).MemoryReservation
	if got.TotalBytes != 3136<<20 || !got.Warning {
		t.Errorf("summary = %+v, want a warning above 3 GiB + 1 GiB on a 4 GiB host", got)
	}
}

func TestMemberGetsNoMemoryReservation(t *testing.T) {
	f := newRoleFixture(t)
	rec := f.as[viewerID].do(http.MethodGet, "/api/system", "", nil)
	if !strings.Contains(rec.Body.String(), `"memory_reservation":null`) {
		t.Errorf("a member must not see other apps' reservations: %s", rec.Body)
	}
	rec = f.owner.do(http.MethodGet, "/api/system", "", nil)
	if !strings.Contains(rec.Body.String(), `"memory_reservation":{"total_bytes":0`) {
		t.Errorf("owner: %s", rec.Body)
	}
}
