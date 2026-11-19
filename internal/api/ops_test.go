package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/system"
)

func newOpsServer(t *testing.T) (http.Handler, *fakeDeployer) {
	t.Helper()
	srv := newTestServer(t, nil)
	fake := srv.ops.(*fakeDeployer)
	h := srv.Handler()
	createApp(t, h, "blog")
	return h, fake
}

func TestStartStopRestart(t *testing.T) {
	h, fake := newOpsServer(t)

	stopped := request(t, h, http.MethodPost, "/api/apps/blog/stop", "")
	if stopped.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", stopped.Code, stopped.Body)
	}
	if app := decode[appJSON](t, stopped); app.DesiredState != "stopped" {
		t.Errorf("the response must show the new state: %+v", app)
	}
	started := decode[appJSON](t, request(t, h, http.MethodPost, "/api/apps/blog/start", ""))
	if started.DesiredState != "running" {
		t.Errorf("app = %+v", started)
	}
	if rec := request(t, h, http.MethodPost, "/api/apps/blog/restart", ""); rec.Code != http.StatusOK {
		t.Errorf("restart: %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(fake.ops, []string{"stop:blog", "start:blog", "restart:blog"}) {
		t.Errorf("ops = %v", fake.ops)
	}
	if rec := request(t, h, http.MethodPost, "/api/apps/missing/stop", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing app: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/blog/stop", ""); rec.Code == http.StatusOK {
		t.Error("operations must not be triggered by GET")
	}
}

func TestOperationErrors(t *testing.T) {
	h, fake := newOpsServer(t)
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{deploy.ErrBusy, http.StatusConflict, "busy"},
		{deploy.ErrNotDeployed, http.StatusConflict, "not_deployed"},
		{errors.New("docker compose stop: exit status 1: secret-detail"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		fake.opErr = tc.err
		for _, target := range []string{"/api/apps/blog/start", "/api/apps/blog/stop", "/api/apps/blog/restart"} {
			rec := request(t, h, http.MethodPost, target, "")
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Errorf("%s with %v: %d %s", target, tc.err, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "secret-detail") {
				t.Errorf("internals leaked: %s", rec.Body)
			}
		}
		if rec := request(t, h, http.MethodDelete, "/api/apps/blog", ""); rec.Code != tc.status {
			t.Errorf("delete with %v: %d", tc.err, rec.Code)
		}
	}
}

func TestDeleteApp(t *testing.T) {
	h, fake := newOpsServer(t)
	createApp(t, h, "shop")
	createApp(t, h, "wiki")

	if rec := request(t, h, http.MethodDelete, "/api/apps/blog?volumes=yes", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("ambiguous volumes value: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodDelete, "/api/apps/blog", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodDelete, "/api/apps/shop?volumes=true", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with volumes: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodDelete, "/api/apps/wiki?volumes=false", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with volumes=false: %d", rec.Code)
	}
	want := []string{"delete(volumes=false):blog", "delete(volumes=true):shop", "delete(volumes=false):wiki"}
	if !slices.Equal(fake.ops, want) {
		t.Errorf("ops = %v", fake.ops)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/blog", ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleted app still served: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodDelete, "/api/apps/blog", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", rec.Code)
	}
}

func TestListServices(t *testing.T) {
	h, fake := newOpsServer(t)
	empty := request(t, h, http.MethodGet, "/api/apps/blog/services", "")
	if strings.TrimSpace(empty.Body.String()) != `{"services":[]}` {
		t.Errorf("no containers: %s", empty.Body)
	}
	used, limit := int64(27283947), int64(512<<20)
	fake.services = []deploy.ServiceStatus{
		{Name: "db", State: "running", Status: "Up 2 hours (healthy)", Health: "healthy", MemoryBytes: &used, MemoryLimitBytes: &limit},
		{Name: "web", State: "restarting", Status: "Restarting (1) 3 seconds ago"},
	}
	rec := request(t, h, http.MethodGet, "/api/apps/blog/services", "")
	want := `{"services":[{"name":"db","state":"running","status":"Up 2 hours (healthy)","health":"healthy","memory_bytes":27283947,"memory_limit_bytes":536870912},` +
		`{"name":"web","state":"restarting","status":"Restarting (1) 3 seconds ago","health":"","memory_bytes":null,"memory_limit_bytes":null}]}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("services = %s", rec.Body)
	}
}

func TestPrune(t *testing.T) {
	h, fake := newOpsServer(t)
	rec := request(t, h, http.MethodPost, "/api/system/prune", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Total reclaimed space") {
		t.Fatalf("prune: %d %s", rec.Code, rec.Body)
	}
	request(t, h, http.MethodPost, "/api/system/prune", `{"builder": false}`)
	request(t, h, http.MethodPost, "/api/system/prune", `{"images": false, "builder": true}`)
	want := []string{"prune(images=true,builder=true)", "prune(images=true,builder=false)", "prune(images=false,builder=true)"}
	if !slices.Equal(fake.ops, want) {
		t.Errorf("ops = %v", fake.ops)
	}
	fake.opErr = errors.New("builder prune: exit status 1")
	failed := request(t, h, http.MethodPost, "/api/system/prune", "")
	if failed.Code != http.StatusInternalServerError || !strings.Contains(failed.Body.String(), "prune_failed") {
		t.Errorf("failed prune: %d %s", failed.Code, failed.Body)
	}
}

func TestBackupSettingsOnApp(t *testing.T) {
	h, _ := newOpsServer(t)
	app := decode[appJSON](t, request(t, h, http.MethodGet, "/api/apps/blog", ""))
	if app.BackupExcludeVolumes == nil || len(app.BackupExcludeVolumes) != 0 || app.BackupBeforeDeploy || app.DumpHookService != "" {
		t.Errorf("defaults = %+v", app)
	}

	rec := request(t, h, http.MethodPatch, "/api/apps/blog",
		`{"backup_exclude_volumes": ["pgdata", " pgdata ", ""], "dump_hook_service": "db", "dump_hook_command": "pg_dump -U postgres app", "backup_before_deploy": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	app = decode[appJSON](t, rec)
	if !slices.Equal(app.BackupExcludeVolumes, []string{"pgdata"}) || app.DumpHookService != "db" ||
		app.DumpHookCommand != "pg_dump -U postgres app" || !app.BackupBeforeDeploy {
		t.Errorf("app = %+v", app)
	}

	for name, body := range map[string]string{
		"command without service": `{"dump_hook_service": ""}`,
		"service without command": `{"dump_hook_command": ""}`,
		"multi-line command":      `{"dump_hook_command": "pg_dump\nrm -rf /"}`,
		"bad volume name":         `{"backup_exclude_volumes": ["../etc"]}`,
		"bad service name":        `{"dump_hook_service": "d b"}`,
	} {
		rec := request(t, h, http.MethodPatch, "/api/apps/blog", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// Clearing the hook takes both fields.
	cleared := decode[appJSON](t, request(t, h, http.MethodPatch, "/api/apps/blog", `{"dump_hook_service": "", "dump_hook_command": ""}`))
	if cleared.DumpHookService != "" || cleared.DumpHookCommand != "" {
		t.Errorf("app = %+v", cleared)
	}
}

func TestSystemStatusAndDockerUsage(t *testing.T) {
	loaded := time.Date(2026, 11, 18, 3, 0, 2, 0, time.UTC)
	archived := time.Date(2026, 11, 18, 3, 10, 5, 0, time.UTC)
	nextArchive := time.Date(2026, 11, 18, 7, 10, 0, 0, time.UTC)
	logs := &fakeLogs{status: LogArchiveStatus{Configured: true, LastSuccessAt: &archived, NextRunAt: &nextArchive}}
	srv := newTestServer(t, func(o *Options) {
		o.Logs = logs
		o.System = fakeProber{info: system.Info{Swap: &system.Swap{TotalBytes: 2 << 30, UsedBytes: 100 << 20}}}
		o.RouteStatus = func() RouteStatus {
			return RouteStatus{LastLoadedAt: &loaded, LastError: "load caddy config: connection refused"}
		}
	})
	h := srv.Handler()
	fake := srv.ops.(*fakeDeployer)

	body := decode[struct {
		DiskLimitPercent int              `json:"disk_limit_percent"`
		Swap             *system.Swap     `json:"swap"`
		LogArchive       LogArchiveStatus `json:"log_archive"`
		Caddy            struct {
			Enabled      bool       `json:"enabled"`
			LastLoadedAt *time.Time `json:"last_loaded_at"`
			LastError    string     `json:"last_error"`
		} `json:"caddy"`
	}](t, request(t, h, http.MethodGet, "/api/system", ""))
	if body.DiskLimitPercent != 85 {
		t.Errorf("disk_limit_percent = %d", body.DiskLimitPercent)
	}
	if !body.LogArchive.Configured || body.LogArchive.LastSuccessAt == nil || !body.LogArchive.LastSuccessAt.Equal(archived) {
		t.Errorf("log_archive = %+v", body.LogArchive)
	}
	if body.LogArchive.NextRunAt == nil || !body.LogArchive.NextRunAt.Equal(nextArchive) {
		t.Errorf("log_archive.next_run_at = %v", body.LogArchive.NextRunAt)
	}
	if body.Swap == nil || body.Swap.TotalBytes != 2<<30 || body.Swap.UsedBytes != 100<<20 {
		t.Errorf("swap = %+v", body.Swap)
	}
	if !body.Caddy.Enabled || body.Caddy.LastLoadedAt == nil || !body.Caddy.LastLoadedAt.Equal(loaded) ||
		!strings.Contains(body.Caddy.LastError, "connection refused") {
		t.Errorf("caddy = %+v", body.Caddy)
	}

	// Without the optional parts the fields are present and empty.
	bare := request(t, newTestServer(t, func(o *Options) { o.Config.Caddy.Enabled = false }).Handler(), http.MethodGet, "/api/system", "")
	for _, want := range []string{
		`"log_archive":{"configured":false,"last_success_at":null,"last_error":"","next_run_at":null}`,
		`"swap":null`,
		`"caddy":{"enabled":false,"last_loaded_at":null,"last_error":""}`,
	} {
		if !strings.Contains(bare.Body.String(), want) {
			t.Errorf("bare system response lacks %s: %s", want, bare.Body)
		}
	}

	fake.diskUsage = []deploy.DiskUsageItem{{Type: "images", TotalBytes: 5368709120, ReclaimableBytes: 1073741824, Count: 12}}
	usage := request(t, h, http.MethodGet, "/api/system/docker", "")
	want := `{"items":[{"type":"images","total_bytes":5368709120,"reclaimable_bytes":1073741824,"count":12}]}`
	if strings.TrimSpace(usage.Body.String()) != want {
		t.Errorf("docker usage = %s", usage.Body)
	}
	fake.diskUsage = nil
	if empty := request(t, h, http.MethodGet, "/api/system/docker", ""); strings.TrimSpace(empty.Body.String()) != `{"items":[]}` {
		t.Errorf("empty docker usage = %s", empty.Body)
	}
}

func TestContainerResources(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	fake := srv.ops.(*fakeDeployer)

	// No running containers: an empty list, not null.
	fake.resources = deploy.ContainerResources{CPUCount: 2, MemoryTotalBytes: 4 << 30}
	empty := request(t, h, http.MethodGet, "/api/system/containers", "")
	if want := `{"cpu_count":2,"memory_total_bytes":4294967296,"groups":[]}`; strings.TrimSpace(empty.Body.String()) != want {
		t.Errorf("empty = %s", empty.Body)
	}

	slug, limit := "blog", int64(512<<20)
	fake.resources.Groups = []deploy.ResourceGroup{{
		App: &slug, MemoryBytes: 100 << 20, MemoryLimitBytes: &limit, CPUPercent: 1.5,
		Containers: []deploy.ContainerUsage{{Name: "koroku-blog-web-1", Project: "koroku-blog", Service: "web", MemoryBytes: 100 << 20, MemoryLimitBytes: &limit, CPUPercent: 1.5}},
	}}
	got := request(t, h, http.MethodGet, "/api/system/containers", "")
	want := `{"cpu_count":2,"memory_total_bytes":4294967296,"groups":[{"app":"blog","memory_bytes":104857600,"memory_limit_bytes":536870912,"cpu_percent":1.5,` +
		`"containers":[{"name":"koroku-blog-web-1","project":"koroku-blog","service":"web","memory_bytes":104857600,"memory_limit_bytes":536870912,"cpu_percent":1.5}]}]}`
	if strings.TrimSpace(got.Body.String()) != want {
		t.Errorf("containers = %s", got.Body)
	}

	fake.opErr = errors.New("Cannot connect to the Docker daemon")
	if failed := request(t, h, http.MethodGet, "/api/system/containers", ""); failed.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", failed.Code)
	}
}

func TestAppPrimaryURLAndPendingDomains(t *testing.T) {
	checker := &fakeDomainChecker{ok: map[string]bool{"www.example.org": true}}
	srv := newTestServer(t, func(o *Options) {
		checker.store = o.Store
		o.Domains = checker
	})
	h := srv.Handler()
	app := createApp(t, h, "blog")
	if app.PrimaryURL != "https://blog.dev.example.com" || app.PendingDomains != 0 {
		t.Errorf("new app = %q, %d", app.PrimaryURL, app.PendingDomains)
	}

	active := decode[domainBody](t, request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "www.example.org"}`))
	request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "pending.example.org"}`)
	got := decode[appJSON](t, request(t, h, http.MethodGet, "/api/apps/blog", ""))
	// An active domain that is not primary does not change the primary URL.
	if got.PrimaryURL != "https://blog.dev.example.com" || got.PendingDomains != 1 {
		t.Errorf("app = %q, %d", got.PrimaryURL, got.PendingDomains)
	}

	request(t, h, http.MethodPatch, fmt.Sprintf("/api/apps/blog/domains/%d", active.ID), `{"is_primary": true}`)
	list := decode[struct{ Apps []appJSON }](t, request(t, h, http.MethodGet, "/api/apps", ""))
	if list.Apps[0].PrimaryURL != "https://www.example.org" || list.Apps[0].URL != "https://blog.dev.example.com" || list.Apps[0].PendingDomains != 1 {
		t.Errorf("list app = %+v", list.Apps[0])
	}
}
