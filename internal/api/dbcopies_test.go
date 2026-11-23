package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// fakeCopier records copy requests and answers with a scripted error.
type fakeCopier struct {
	store  *store.Store
	logDir string

	mu       sync.Mutex
	err      error
	requests []deploy.CopyRequest
}

func (f *fakeCopier) StartCopy(ctx context.Context, req deploy.CopyRequest) (store.DBCopy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return store.DBCopy{}, f.err
	}
	return f.store.StartDBCopy(ctx, req.Target, req.Source, req.Kind, time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC))
}

func (f *fakeCopier) CopyLogPath(slug string, id int64) string {
	return filepath.Join(f.logDir, fmt.Sprintf("%s-%d.log", slug, id))
}

func newCopyServer(t *testing.T) (http.Handler, *fakeCopier, *Server) {
	t.Helper()
	var copier *fakeCopier
	srv := newTestServer(t, func(o *Options) {
		copier = &fakeCopier{store: o.Store, logDir: t.TempDir()}
		o.Copier = copier
	})
	h := srv.Handler()
	createApp(t, h, "blog")
	createApp(t, h, "blog-staging")
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"environment":"production"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	return h, copier, srv
}

func TestLoadHookSettings(t *testing.T) {
	h, _, _ := newCopyServer(t)
	rec := request(t, h, http.MethodPatch, "/api/apps/blog-staging",
		`{"load_hook_service":"db","load_hook_command":"pg_restore -d app","post_load_service":"web","post_load_command":"bin/rails db:anonymize"}`)
	app := decode[appJSON](t, rec)
	if app.LoadHookService != "db" || app.LoadHookCommand != "pg_restore -d app" ||
		app.PostLoadService != "web" || app.PostLoadCommand != "bin/rails db:anonymize" {
		t.Errorf("app = %+v", app)
	}
	for body, field := range map[string]string{
		`{"load_hook_service":"","load_hook_command":"pg_restore"}`:                          "load_hook_service",
		`{"load_hook_service":"db","load_hook_command":""}`:                                  "load_hook_command",
		`{"post_load_service":"bad name","post_load_command":"x"}`:                           "post_load_service",
		`{"post_load_service":"web","post_load_command":"a\nb"}`:                             "post_load_command",
		`{"load_hook_service":"db","load_hook_command":"` + strings.Repeat("x", 1001) + `"}`: "load_hook_command",
	} {
		rec := request(t, h, http.MethodPatch, "/api/apps/blog-staging", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), field) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestStartDBCopy(t *testing.T) {
	h, copier, _ := newCopyServer(t)
	rec := request(t, h, http.MethodPost, "/api/apps/blog-staging/db-copies",
		`{"source":"blog","confirm":"blog-staging"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	c := decode[dbCopyJSON](t, rec)
	if c.TargetApp != "blog-staging" || c.SourceApp != "blog" || c.SourceKind != "live" || c.Status != "running" {
		t.Errorf("copy = %+v", c)
	}
	if len(copier.requests) != 1 || copier.requests[0].Source.Slug != "blog" ||
		copier.requests[0].Target.Slug != "blog-staging" || copier.requests[0].Kind != store.CopySourceLive {
		t.Errorf("requests = %+v", copier.requests)
	}

	snap := request(t, h, http.MethodPost, "/api/apps/blog-staging/db-copies",
		`{"source":"blog","source_kind":"snapshot","confirm":"blog-staging"}`)
	if snap.Code != http.StatusAccepted || decode[dbCopyJSON](t, snap).SourceKind != "snapshot" {
		t.Errorf("snapshot: %d %s", snap.Code, snap.Body)
	}

	list := decode[struct {
		Copies []dbCopyJSON `json:"copies"`
	}](t, request(t, h, http.MethodGet, "/api/apps/blog-staging/db-copies", ""))
	if len(list.Copies) != 2 || list.Copies[0].SourceKind != "snapshot" {
		t.Errorf("list = %+v", list)
	}
	if got := decode[dbCopyJSON](t, request(t, h, http.MethodGet, fmt.Sprintf("/api/db-copies/%d", c.ID), "")); got.ID != c.ID {
		t.Errorf("get = %+v", got)
	}
	if rec := request(t, h, http.MethodGet, "/api/db-copies/999", ""); rec.Code != http.StatusNotFound ||
		errorCode(t, rec) != "db_copy_not_found" {
		t.Errorf("missing copy: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/blog-staging/db-copies?limit=0", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0: %d", rec.Code)
	}
}

func TestStartDBCopyRefusals(t *testing.T) {
	h, copier, _ := newCopyServer(t)
	cases := []struct {
		target, body string
		status       int
		code         string
	}{
		{"blog", `{"source":"blog-staging","confirm":"blog"}`, http.StatusUnprocessableEntity, "target_not_staging"},
		{"blog-staging", `{"source":"blog","confirm":"blog"}`, http.StatusUnprocessableEntity, "confirmation_mismatch"},
		{"blog-staging", `{"source":"blog"}`, http.StatusUnprocessableEntity, "confirmation_mismatch"},
		{"blog-staging", `{"source":"blog-staging","confirm":"blog-staging"}`, http.StatusUnprocessableEntity, "validation_failed"},
		{"blog-staging", `{"source":"blog","source_kind":"disk","confirm":"blog-staging"}`, http.StatusUnprocessableEntity, "validation_failed"},
		{"blog-staging", `{"source":"missing","confirm":"blog-staging"}`, http.StatusNotFound, "source_not_found"},
		{"missing", `{"source":"blog","confirm":"missing"}`, http.StatusNotFound, "app_not_found"},
	}
	for _, tc := range cases {
		rec := request(t, h, http.MethodPost, "/api/apps/"+tc.target+"/db-copies", tc.body)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
			t.Errorf("%s %s: %d %s", tc.target, tc.body, rec.Code, rec.Body)
		}
	}
	if len(copier.requests) != 0 {
		t.Errorf("refused requests reached the copier: %+v", copier.requests)
	}

	engineErrors := []struct {
		err    error
		status int
		code   string
	}{
		{deploy.ErrBusy, http.StatusConflict, "busy"},
		{deploy.ErrNotDeployed, http.StatusConflict, "not_deployed"},
		{deploy.ErrTargetNotRunning, http.StatusConflict, "target_not_running"},
		{deploy.ErrSourceNotRunning, http.StatusConflict, "source_not_running"},
		{deploy.ErrNoDumpSnapshot, http.StatusConflict, "no_dump_snapshot"},
		{deploy.ErrNoDumpHook, http.StatusUnprocessableEntity, "source_dump_hook_missing"},
		{deploy.ErrNoLoadHook, http.StatusUnprocessableEntity, "target_load_hook_missing"},
		{deploy.ErrBackupsNotConfigured, http.StatusServiceUnavailable, "not_configured"},
		{fmt.Errorf("wrapped: %w", deploy.ErrBusy), http.StatusConflict, "busy"},
		{errors.New("docker: secret-detail"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range engineErrors {
		copier.err = tc.err
		rec := request(t, h, http.MethodPost, "/api/apps/blog-staging/db-copies", `{"source":"blog","confirm":"blog-staging"}`)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) || strings.Contains(rec.Body.String(), "secret-detail") {
			t.Errorf("%v: %d %s", tc.err, rec.Code, rec.Body)
		}
	}
}

func TestDBCopyLogStream(t *testing.T) {
	old := logPollInterval
	logPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { logPollInterval = old })
	h, copier, srv := newCopyServer(t)
	c := decode[dbCopyJSON](t, request(t, h, http.MethodPost, "/api/apps/blog-staging/db-copies",
		`{"source":"blog","confirm":"blog-staging"}`))
	path := copier.CopyLogPath("blog-staging", c.ID)
	if err := os.WriteFile(path, []byte("==> 2026-11-21T03:00:00Z check Checking blog and blog-staging\nload | done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishDBCopy(context.Background(), c.ID, true, "", 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := request(t, h, http.MethodGet, fmt.Sprintf("/api/db-copies/%d/logs", c.ID), "")
	body := rec.Body.String()
	if !strings.Contains(body, "data: load | done\n") || !strings.HasSuffix(body, "event: end\ndata: succeeded\n\n") {
		t.Errorf("stream = %q", body)
	}
	text := request(t, h, http.MethodGet, fmt.Sprintf("/api/db-copies/%d/logs?format=text", c.ID), "")
	if !strings.HasPrefix(text.Body.String(), "==> ") {
		t.Errorf("text = %q", text.Body)
	}
}
