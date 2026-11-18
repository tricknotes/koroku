package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

type fakeLogs struct {
	mu        sync.Mutex
	tail      []LogLine
	subs      map[string][]chan LogLine
	canceled  int
	lastQuery LogQuery
	lastSlug  string
	archives  []LogArchive
	archErr   error
	truncated bool
	status    LogArchiveStatus
}

func (f *fakeLogs) Subscribe(slug string) (<-chan LogLine, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[string][]chan LogLine{}
	}
	ch := make(chan LogLine, 16)
	f.subs[slug] = append(f.subs[slug], ch)
	return ch, func() {
		f.mu.Lock()
		f.canceled++
		f.mu.Unlock()
	}
}

func (f *fakeLogs) publish(slug string, line LogLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs[slug] {
		ch <- line
	}
}

func (f *fakeLogs) subscribers(slug string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs[slug])
}

func (f *fakeLogs) Tail(_, service string, n int) ([]LogLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []LogLine
	for _, l := range f.tail {
		if service == "" || l.Service == service {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

func (f *fakeLogs) Search(_ context.Context, slug string, q LogQuery) ([]LogLine, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSlug, f.lastQuery = slug, q
	return f.tail, f.truncated, nil
}

func (f *fakeLogs) ListArchives(_ context.Context, _, service string) ([]LogArchive, error) {
	if f.archErr != nil {
		return nil, f.archErr
	}
	var out []LogArchive
	for _, a := range f.archives {
		if service == "" || a.Service == service {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeLogs) ArchiveStatus() LogArchiveStatus { return f.status }

func (f *fakeLogs) ArchiveURL(_ context.Context, slug, key string) (string, error) {
	if f.archErr != nil {
		return "", f.archErr
	}
	if !strings.HasPrefix(key, "logs/"+slug+"/") || strings.Contains(key, "..") {
		return "", ErrInvalidArchiveKey
	}
	return "https://storage.example.com/" + key + "?X-Amz-Signature=abc", nil
}

func newLogsServer(t *testing.T, logs *fakeLogs) *Server {
	t.Helper()
	srv := newTestServer(t, func(o *Options) {
		if logs != nil {
			o.Logs = logs
		}
	})
	createApp(t, srv.Handler(), "blog")
	return srv
}

func at(sec int) time.Time { return time.Date(2026, 11, 18, 3, 0, sec, 0, time.UTC) }

func TestLiveLogs(t *testing.T) {
	logs := &fakeLogs{tail: []LogLine{
		{Service: "db", Time: at(1), Line: "database system is ready"},
		{Service: "web", Time: at(2), Line: "Listening on 0.0.0.0:3000"},
		{Service: "web", Time: at(3), Line: `GET / 200 "quoted" <html>`},
	}}
	srv := newLogsServer(t, logs)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/apps/blog/logs?service=web&tail=10")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	reader := bufio.NewReader(resp.Body)
	next := func() LogLine {
		t.Helper()
		ev := readEvent(t, reader)
		var line LogLine
		if err := json.Unmarshal([]byte(ev.Data), &line); err != nil {
			t.Fatalf("event data %q: %v", ev.Data, err)
		}
		return line
	}

	// The tail comes first, filtered by service.
	if l := next(); l.Line != "Listening on 0.0.0.0:3000" || l.Service != "web" || !l.Time.Equal(at(2)) {
		t.Errorf("first = %+v", l)
	}
	if l := next(); l.Line != `GET / 200 "quoted" <html>` {
		t.Errorf("second = %+v", l)
	}

	// Then live lines: other services are filtered, and a line already sent
	// with the tail is not repeated.
	logs.publish("blog", LogLine{Service: "web", Time: at(3), Line: `GET / 200 "quoted" <html>`})
	logs.publish("blog", LogLine{Service: "db", Time: at(4), Line: "checkpoint complete"})
	logs.publish("blog", LogLine{Service: "web", Time: at(5), Line: "GET /up 200"})
	if l := next(); l.Line != "GET /up 200" {
		t.Errorf("live line = %+v", l)
	}

	// Closing the connection releases the subscription.
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs.mu.Lock()
		canceled := logs.canceled
		logs.mu.Unlock()
		if canceled == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the subscription was not canceled after the client left")
}

func TestLiveLogsValidation(t *testing.T) {
	srv := newLogsServer(t, &fakeLogs{})
	h := srv.Handler()
	for _, target := range []string{
		"/api/apps/blog/logs?tail=0", "/api/apps/blog/logs?tail=999999", "/api/apps/blog/logs?tail=abc",
		"/api/apps/blog/logs?service=" + url.QueryEscape("../etc"), "/api/apps/blog/logs?service=" + url.QueryEscape("a b"),
	} {
		if rec := request(t, h, http.MethodGet, target, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d", target, rec.Code)
		}
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/missing/logs", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing app: %d", rec.Code)
	}
}

func TestLogHistory(t *testing.T) {
	logs := &fakeLogs{tail: []LogLine{{Service: "web", Time: at(1), Line: "error: boom"}}, truncated: true}
	srv := newLogsServer(t, logs)
	h := srv.Handler()

	rec := request(t, h, http.MethodGet,
		"/api/apps/blog/logs/history?service=web&from=2026-11-18T00:00:00Z&to=2026-11-18T12:00:00%2B09:00&q=error&limit=50", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	want := `{"lines":[{"service":"web","time":"2026-11-18T03:00:01Z","line":"error: boom"}],"truncated":true}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("body = %s", rec.Body)
	}
	q := logs.lastQuery
	if logs.lastSlug != "blog" || q.Service != "web" || q.Contains != "error" || q.Limit != 50 ||
		!q.From.Equal(time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)) || !q.To.Equal(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("query = %+v", q)
	}

	// Defaults: everything, default limit.
	logs.tail, logs.truncated = nil, false
	rec = request(t, h, http.MethodGet, "/api/apps/blog/logs/history", "")
	if strings.TrimSpace(rec.Body.String()) != `{"lines":[],"truncated":false}` {
		t.Errorf("empty history = %s", rec.Body)
	}
	if q := logs.lastQuery; q.Limit != defaultHistorySize || !q.From.IsZero() || !q.To.IsZero() || q.Service != "" {
		t.Errorf("default query = %+v", q)
	}

	for name, target := range map[string]string{
		"bad from":    "/api/apps/blog/logs/history?from=yesterday",
		"to < from":   "/api/apps/blog/logs/history?from=2026-11-18T03:00:00Z&to=2026-11-18T02:00:00Z",
		"limit zero":  "/api/apps/blog/logs/history?limit=0",
		"limit huge":  "/api/apps/blog/logs/history?limit=100000",
		"bad service": "/api/apps/blog/logs/history?service=%2Fetc",
		"long query":  "/api/apps/blog/logs/history?q=" + strings.Repeat("x", 201),
	} {
		if rec := request(t, h, http.MethodGet, target, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d", name, rec.Code)
		}
	}
}

func TestLogArchives(t *testing.T) {
	hour := time.Date(2026, 11, 18, 4, 0, 0, 0, time.FixedZone("JST", 9*3600))
	logs := &fakeLogs{archives: []LogArchive{
		{Key: "logs/blog/web/2026/11/18/04.log.gz", Service: "web", Hour: hour, Size: 12345},
		{Key: "logs/blog/db/2026/11/18/04.log.gz", Service: "db", Hour: hour, Size: 99},
	}}
	srv := newLogsServer(t, logs)
	h := srv.Handler()

	rec := request(t, h, http.MethodGet, "/api/apps/blog/logs/archives?service=web", "")
	want := `{"archives":[{"key":"logs/blog/web/2026/11/18/04.log.gz","service":"web","hour":"2026-11-18T04:00:00+09:00","size":12345}]}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("archives = %s", rec.Body)
	}
	all := decode[struct{ Archives []LogArchive }](t, request(t, h, http.MethodGet, "/api/apps/blog/logs/archives", ""))
	if len(all.Archives) != 2 {
		t.Errorf("all archives = %+v", all)
	}

	ok := request(t, h, http.MethodGet, "/api/apps/blog/logs/archives/url?key="+url.QueryEscape("logs/blog/web/2026/11/18/04.log.gz"), "")
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), "X-Amz-Signature") {
		t.Errorf("url: %d %s", ok.Code, ok.Body)
	}
	// Keys of other apps or outside the log prefix are refused.
	for _, key := range []string{"logs/shop/web/2026/11/18/04.log.gz", "restic/config", "logs/blog/../shop/x", ""} {
		rec := request(t, h, http.MethodGet, "/api/apps/blog/logs/archives/url?key="+url.QueryEscape(key), "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("key %q: status = %d", key, rec.Code)
		}
	}

	logs.archErr = ErrNotConfigured
	for _, target := range []string{"/api/apps/blog/logs/archives", "/api/apps/blog/logs/archives/url?key=x"} {
		rec := request(t, h, http.MethodGet, target, "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not_configured") {
			t.Errorf("%s without storage: %d %s", target, rec.Code, rec.Body)
		}
	}
}

func TestLogsNotAvailable(t *testing.T) {
	srv := newLogsServer(t, nil)
	h := srv.Handler()
	for _, target := range []string{"/api/apps/blog/logs", "/api/apps/blog/logs/history", "/api/apps/blog/logs/archives", "/api/apps/blog/logs/archives/url?key=x"} {
		rec := request(t, h, http.MethodGet, target, "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not_configured") {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}
}

type fakeBackups struct {
	store      *store.Store
	mu         sync.Mutex
	running    bool
	busy       bool
	snapshots  []BackupSnapshot
	restored   []string
	restoreErr error
	dump       string
	dumpErr    error
	nextRun    time.Time
}

func (f *fakeBackups) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeBackups) NextRun() time.Time { return f.nextRun }

func (f *fakeBackups) StartManual(ctx context.Context) (store.BackupRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		return store.BackupRun{}, ErrBackupBusy
	}
	f.running = true
	return f.store.StartBackupRun(ctx, store.BackupTriggerManual, time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
}

func (f *fakeBackups) Snapshots(context.Context, store.App) ([]BackupSnapshot, error) {
	return f.snapshots, nil
}

func (f *fakeBackups) RestoreVolume(_ context.Context, app store.App, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.restored = append(f.restored, app.Slug+":"+id)
	return nil
}

func (f *fakeBackups) OpenDump(_ context.Context, _ store.App, id string) (string, func(io.Writer) error, error) {
	if f.dumpErr != nil {
		return "", nil, f.dumpErr
	}
	return "blog-db-" + id + ".sql", func(w io.Writer) error {
		_, err := io.WriteString(w, f.dump)
		return err
	}, nil
}

func newBackupServer(t *testing.T, configured bool) (*Server, *fakeBackups) {
	t.Helper()
	backups := &fakeBackups{}
	srv := newTestServer(t, func(o *Options) {
		backups.store = o.Store
		if configured {
			o.Backups = backups
		}
	})
	createApp(t, srv.Handler(), "blog")
	return srv, backups
}

func TestBackupStatus(t *testing.T) {
	srv, backups := newBackupServer(t, true)
	h := srv.Handler()
	ctx := context.Background()
	now := srv.clock.Now()

	type status struct {
		NextRunAt     *time.Time      `json:"next_run_at"`
		Configured    bool            `json:"configured"`
		Running       bool            `json:"running"`
		LastSuccessAt *time.Time      `json:"last_success_at"`
		Warning       bool            `json:"warning"`
		Runs          []backupRunJSON `json:"runs"`
	}
	// Never backed up: warn.
	backups.nextRun = time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	got := decode[status](t, request(t, h, http.MethodGet, "/api/backups", ""))
	if got.NextRunAt == nil || !got.NextRunAt.Equal(backups.nextRun) {
		t.Errorf("next_run_at = %v", got.NextRunAt)
	}
	if !got.Configured || got.Running || got.LastSuccessAt != nil || !got.Warning || got.Runs == nil || len(got.Runs) != 0 {
		t.Errorf("initial status = %+v", got)
	}

	// A success 23 hours ago is fine; 25 hours ago is not.
	run, _ := srv.store.StartBackupRun(ctx, store.BackupTriggerScheduled, now.Add(-23*time.Hour-time.Minute))
	_ = srv.store.FinishBackupRun(ctx, run.ID, true, "", now.Add(-23*time.Hour))
	got = decode[status](t, request(t, h, http.MethodGet, "/api/backups", ""))
	if got.Warning || got.LastSuccessAt == nil || len(got.Runs) != 1 || got.Runs[0].Status != "succeeded" || got.Runs[0].Trigger != "scheduled" {
		t.Errorf("status after a recent success = %+v", got)
	}
	srv.clock = fixedClock{now.Add(2*time.Hour + time.Minute)}
	if got = decode[status](t, request(t, h, http.MethodGet, "/api/backups", "")); !got.Warning {
		t.Errorf("no warning 25 hours after the last success: %+v", got)
	}

	// Manual run.
	started := request(t, h, http.MethodPost, "/api/backups", "")
	if started.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", started.Code, started.Body)
	}
	if r := decode[backupRunJSON](t, started); r.Status != "running" || r.Trigger != "manual" || r.FinishedAt != nil {
		t.Errorf("run = %+v", r)
	}
	if got = decode[status](t, request(t, h, http.MethodGet, "/api/backups", "")); !got.Running || len(got.Runs) != 2 {
		t.Errorf("status while running = %+v", got)
	}
	backups.busy = true
	if rec := request(t, h, http.MethodPost, "/api/backups", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "busy") {
		t.Errorf("second start: %d %s", rec.Code, rec.Body)
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestBackupsNotConfigured(t *testing.T) {
	srv, _ := newBackupServer(t, false)
	h := srv.Handler()
	// The status endpoint still answers, without a warning.
	status := request(t, h, http.MethodGet, "/api/backups", "")
	if !strings.Contains(status.Body.String(), `"next_run_at":null`) {
		t.Errorf("status without backups = %s", status.Body)
	}
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"configured":false`) ||
		!strings.Contains(status.Body.String(), `"warning":false`) {
		t.Errorf("status: %d %s", status.Code, status.Body)
	}
	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/backups", ""},
		{http.MethodGet, "/api/apps/blog/backups", ""},
		{http.MethodPost, "/api/apps/blog/backups/restore", `{"snapshot_id": "4bba301e"}`},
		{http.MethodGet, "/api/apps/blog/backups/4bba301e/download", ""},
	} {
		rec := request(t, h, c.method, c.target, c.body)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not_configured") {
			t.Errorf("%s %s: %d %s", c.method, c.target, rec.Code, rec.Body)
		}
	}
}

func TestSnapshotsRestoreAndDownload(t *testing.T) {
	srv, backups := newBackupServer(t, true)
	h := srv.Handler()
	size := int64(1 << 20)
	backups.snapshots = []BackupSnapshot{
		{ID: "4bba301e", Time: at(0), Kind: "volume", Target: "pgdata", SizeBytes: &size},
		{ID: "9c0ffee1", Time: at(1), Kind: "dump", Target: "db"},
	}
	list := request(t, h, http.MethodGet, "/api/apps/blog/backups", "")
	want := `{"snapshots":[{"id":"4bba301e","time":"2026-11-18T03:00:00Z","kind":"volume","target":"pgdata","size_bytes":1048576},{"id":"9c0ffee1","time":"2026-11-18T03:00:01Z","kind":"dump","target":"db","size_bytes":null}]}`
	if strings.TrimSpace(list.Body.String()) != want {
		t.Errorf("snapshots = %s", list.Body)
	}

	restored := request(t, h, http.MethodPost, "/api/apps/blog/backups/restore", `{"snapshot_id": "4bba301e"}`)
	if restored.Code != http.StatusOK || strings.TrimSpace(restored.Body.String()) != `{"restored":true}` {
		t.Fatalf("restore: %d %s", restored.Code, restored.Body)
	}
	if len(backups.restored) != 1 || backups.restored[0] != "blog:4bba301e" {
		t.Errorf("restored = %v", backups.restored)
	}
	for _, id := range []string{"", "latest", "../x", "4bba301e; rm -rf /", "XYZ"} {
		rec := request(t, h, http.MethodPost, "/api/apps/blog/backups/restore", fmt.Sprintf(`{"snapshot_id": %q}`, id))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("snapshot id %q: status = %d", id, rec.Code)
		}
	}
	for err, want := range map[error]int{
		ErrSnapshotNotFound: http.StatusNotFound,
		ErrBackupBusy:       http.StatusConflict,
		errors.New("restic: secret-internal-detail"): http.StatusInternalServerError,
	} {
		backups.restoreErr = err
		rec := request(t, h, http.MethodPost, "/api/apps/blog/backups/restore", `{"snapshot_id": "4bba301e"}`)
		if rec.Code != want || strings.Contains(rec.Body.String(), "secret-internal-detail") {
			t.Errorf("restore with %v: %d %s", err, rec.Code, rec.Body)
		}
	}

	backups.dump = "-- PostgreSQL database dump\nCREATE TABLE posts ();\n"
	download := request(t, h, http.MethodGet, "/api/apps/blog/backups/9c0ffee1/download", "")
	if download.Code != http.StatusOK || download.Body.String() != backups.dump {
		t.Fatalf("download: %d %q", download.Code, download.Body)
	}
	if cd := download.Header().Get("Content-Disposition"); cd != "attachment; filename=blog-db-9c0ffee1.sql" {
		t.Errorf("Content-Disposition = %q", cd)
	}
	backups.dumpErr = ErrSnapshotNotFound
	if rec := request(t, h, http.MethodGet, "/api/apps/blog/backups/9c0ffee1/download", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown dump: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/blog/backups/not-an-id/download", ""); rec.Code != http.StatusNotFound {
		t.Errorf("malformed id: %d", rec.Code)
	}
}
