package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/logs"
	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testApp(t *testing.T, st *store.Store, slug string) store.App {
	t.Helper()
	app, err := st.CreateApp(context.Background(), store.App{
		Slug: slug, RepoFullName: "octo/" + slug, DefaultBranch: "main",
		WebService: "web", WebPort: 3000, HealthcheckPath: "/",
	}, 20000, 20999, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestToBackupApp(t *testing.T) {
	current := int64(7)
	app := store.App{
		Slug: "blog", DesiredState: store.DesiredRunning, CurrentDeploymentID: &current,
		BackupExcludeVolumes: []string{"pgdata"}, DumpHookService: "db", DumpHookCommand: "pg_dump -U postgres app",
	}
	got := toBackupApp(app)
	if got.Slug != "blog" || got.Project != "paas-blog" || got.DumpService != "db" || got.DumpCommand != "pg_dump -U postgres app" ||
		len(got.ExcludeVolumes) != 1 {
		t.Errorf("running app = %+v", got)
	}
	// The dump hook needs a running container.
	app.DesiredState = store.DesiredStopped
	if got := toBackupApp(app); got.DumpService != "" || got.DumpCommand != "" {
		t.Errorf("stopped app keeps its dump hook: %+v", got)
	}
	app.DesiredState, app.CurrentDeploymentID = store.DesiredRunning, nil
	if got := toBackupApp(app); got.DumpService != "" {
		t.Errorf("never deployed app keeps its dump hook: %+v", got)
	}
}

func TestBackupRecorder(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	rec := backupRecorder{store: st}
	started := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)

	id, err := rec.StartRun(ctx, backup.TriggerScheduled, started)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := rec.FinishRun(ctx, id, backup.StatusSucceeded, "", started.Add(time.Minute)); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	failed, _ := rec.StartRun(ctx, backup.TriggerManual, started.Add(time.Hour))
	_ = rec.FinishRun(ctx, failed, backup.StatusFailed, "restic: repository unreachable", started.Add(2*time.Hour))

	runs, _ := st.ListBackupRuns(ctx, 10)
	if len(runs) != 2 || runs[0].Status != store.BackupFailed || runs[0].Error == "" || runs[0].Trigger != "manual" ||
		runs[1].Status != store.BackupSucceeded || runs[1].Trigger != "scheduled" {
		t.Errorf("runs = %+v", runs)
	}
	last, _ := st.LastSuccessfulBackup(ctx)
	if last == nil || !last.Equal(started.Add(time.Minute)) {
		t.Errorf("last success = %v", last)
	}
}

func TestMapBackupError(t *testing.T) {
	if mapBackupError(nil) != nil {
		t.Error("nil must stay nil")
	}
	if err := mapBackupError(backup.ErrSnapshotNotFound); !errors.Is(err, api.ErrSnapshotNotFound) {
		t.Errorf("snapshot not found: %v", err)
	}
	if err := mapBackupError(&backup.BusyError{}); !errors.Is(err, api.ErrBackupBusy) {
		t.Errorf("busy: %v", err)
	}
	other := errors.New("restic exploded")
	if err := mapBackupError(other); err != other {
		t.Errorf("other errors pass through: %v", err)
	}
}

func TestBackupsConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Backup.ResticPasswordFile = filepath.Join(t.TempDir(), "restic.key")
	if ok, reason := backupsConfigured(cfg); ok || !strings.Contains(reason, "[storage]") {
		t.Errorf("without storage: %v %q", ok, reason)
	}
	cfg.Storage = config.Storage{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "paas", AccessKeyID: "a", SecretAccessKey: "b"}
	if ok, reason := backupsConfigured(cfg); ok || !strings.Contains(reason, "missing") {
		t.Errorf("without the password file: %v %q", ok, reason)
	}
	if err := os.WriteFile(cfg.Backup.ResticPasswordFile, []byte("example-password\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, reason := backupsConfigured(cfg)
	if ok || !strings.Contains(reason, "0600") || strings.Contains(reason, "example-password") {
		t.Errorf("world-readable password file: %v %q", ok, reason)
	}
	if err := os.Chmod(cfg.Backup.ResticPasswordFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, reason := backupsConfigured(cfg); !ok {
		t.Errorf("configured: %q", reason)
	}
	repo := backupRepository(cfg)
	if repo.Endpoint != "https://s3.example.com" || repo.Bucket != "paas" || repo.Prefix != "restic" ||
		repo.PasswordFile != cfg.Backup.ResticPasswordFile || !strings.HasSuffix(repo.CacheDir, "restic-cache") {
		t.Errorf("repository = %+v", repo)
	}
}

func TestCollectedApps(t *testing.T) {
	st := testStore(t)
	testApp(t, st, "blog")
	testApp(t, st, "shop")

	apps, err := collectedApps(st, "/var/log/caddy/apps")(context.Background())
	if err != nil || len(apps) != 2 {
		t.Fatalf("apps = %+v, %v", apps, err)
	}
	if apps[0] != (logs.App{Slug: "blog", Project: "paas-blog", AccessLogPath: "/var/log/caddy/apps/blog.log"}) {
		t.Errorf("app = %+v", apps[0])
	}
	// Without Caddy there is no access log to follow.
	apps, _ = collectedApps(st, "")(context.Background())
	if apps[0].AccessLogPath != "" {
		t.Errorf("access log path = %q", apps[0].AccessLogPath)
	}
}

func TestAppLogsAdapter(t *testing.T) {
	collector, err := logs.New(logs.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Without object storage the archive calls say so instead of failing oddly.
	local := &appLogs{collector: collector}
	if _, err := local.ListArchives(ctx, "blog", ""); !errors.Is(err, api.ErrNotConfigured) {
		t.Errorf("ListArchives without storage: %v", err)
	}
	if _, err := local.ArchiveURL(ctx, "blog", "logs/blog/web/2026/11/18/04.log.gz"); !errors.Is(err, api.ErrNotConfigured) {
		t.Errorf("ArchiveURL without storage: %v", err)
	}
	if status := local.ArchiveStatus(); status.Configured || status.LastSuccessAt != nil {
		t.Errorf("status without storage = %+v", status)
	}
	if lines, err := local.Tail("blog", "", 10); err != nil || len(lines) != 0 {
		t.Errorf("Tail of an app without logs: %v, %v", lines, err)
	}
	if lines, truncated, err := local.Search(ctx, "blog", api.LogQuery{Limit: 10}); err != nil || truncated || len(lines) != 0 {
		t.Errorf("Search of an app without logs: %v, %v, %v", lines, truncated, err)
	}

	objects := objstore.NewMemory()
	archiver, err := logs.NewArchiver(logs.ArchiverOptions{Collector: collector, Store: objects})
	if err != nil {
		t.Fatal(err)
	}
	remote := &appLogs{collector: collector, archiver: archiver, objects: objects}
	if err := objects.Put(ctx, "logs/blog/web/2026/11/18/04.log.gz", []byte("x"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	archives, err := remote.ListArchives(ctx, "blog", "web")
	if err != nil || len(archives) != 1 || archives[0].Key != "logs/blog/web/2026/11/18/04.log.gz" || archives[0].Service != "web" {
		t.Errorf("archives = %+v, %v", archives, err)
	}
	url, err := remote.ArchiveURL(ctx, "blog", "logs/blog/web/2026/11/18/04.log.gz")
	if err != nil || url == "" {
		t.Errorf("ArchiveURL = %q, %v", url, err)
	}
	// The bucket also holds the backups: only this app's log keys are signed.
	for _, key := range []string{"restic/config", "logs/shop/web/2026/11/18/04.log.gz", "logs/blog/../shop/x.log.gz", ""} {
		if _, err := remote.ArchiveURL(ctx, "blog", key); !errors.Is(err, api.ErrInvalidArchiveKey) {
			t.Errorf("key %q: err = %v", key, err)
		}
	}
	if status := remote.ArchiveStatus(); !status.Configured {
		t.Errorf("status with storage = %+v", status)
	}

	// Subscriptions can be canceled more than once without panicking.
	lines, cancel := remote.Subscribe("blog")
	cancel()
	cancel()
	select {
	case _, open := <-lines:
		if open {
			t.Error("unexpected line")
		}
	case <-time.After(2 * time.Second):
		t.Error("the channel was not closed after cancel")
	}
}

type fakeRestorer struct {
	files map[string]string // relative to the config dir
	db    string
	err   error
	asked string
}

func (f *fakeRestorer) RestoreSystem(_ context.Context, snapshotID, destDir string) (backup.SystemRestore, error) {
	f.asked = snapshotID
	if f.err != nil {
		return backup.SystemRestore{}, f.err
	}
	configDir := filepath.Join(destDir, "config")
	for rel, content := range f.files {
		path := filepath.Join(configDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return backup.SystemRestore{}, err
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return backup.SystemRestore{}, err
		}
	}
	dbPath := filepath.Join(destDir, "paas.db")
	if err := os.WriteFile(dbPath, []byte(f.db), 0o600); err != nil {
		return backup.SystemRestore{}, err
	}
	return backup.SystemRestore{
		SnapshotID: "4bba301e", Time: time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC),
		Dir: destDir, DatabasePath: dbPath, ConfigDir: configDir,
	}, nil
}

func TestRunSystemRestoreOnNewHost(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(root, "var")
	configDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The minimal config the operator wrote to reach the backups.
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("new-host-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	restorer := &fakeRestorer{db: "restored-database", files: map[string]string{
		"config.toml":    "old-host-config",
		"master.key":     "restored-master-key",
		"github-app.pem": "restored-github-key",
	}}
	now := time.Date(2026, 11, 19, 1, 2, 3, 0, time.UTC)

	var out bytes.Buffer
	if err := runSystemRestore(context.Background(), restorer, cfg, configDir, "", false, now, &out); err != nil {
		t.Fatalf("runSystemRestore: %v\n%s", err, out.String())
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	if got := read(cfg.DBPath()); got != "restored-database" {
		t.Errorf("database = %q", got)
	}
	if got := read(filepath.Join(configDir, "master.key")); got != "restored-master-key" {
		t.Errorf("master key = %q", got)
	}
	// The existing config is kept; the operator compares it by hand.
	if got := read(filepath.Join(configDir, "config.toml")); got != "new-host-config" {
		t.Errorf("config.toml was overwritten: %q", got)
	}
	if got := read(filepath.Join(configDir, "config.toml.restored-20261119-010203")); got != "old-host-config" {
		t.Errorf("restored copy of the kept config = %q", got)
	}
	for _, path := range []string{cfg.DBPath(), filepath.Join(configDir, "master.key"), filepath.Join(configDir, "github-app.pem")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, err %v (secrets must be private)", path, info.Mode().Perm(), err)
		}
	}
	text := out.String()
	for _, want := range []string{"Snapshot 4bba301e", "Installed 3 file(s)", "Kept 1 existing file(s)", "config.toml", "Next steps"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "restored-master-key") {
		t.Error("the output contains file content")
	}
	if entries, _ := filepath.Glob(filepath.Join(cfg.DataDir, "restore-system-*")); len(entries) != 0 {
		t.Errorf("the staging directory was left behind: %v", entries)
	}

	// A second run refuses to touch the installed database.
	err := runSystemRestore(context.Background(), restorer, cfg, configDir, "", false, now, &out)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second run err = %v", err)
	}

	// With --force everything is replaced and the old files are kept aside.
	restorer.db = "newer-database"
	if err := runSystemRestore(context.Background(), restorer, cfg, configDir, "9c0ffee1", true, now, &out); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if restorer.asked != "9c0ffee1" {
		t.Errorf("snapshot asked = %q", restorer.asked)
	}
	if got := read(cfg.DBPath()); got != "newer-database" {
		t.Errorf("database after --force = %q", got)
	}
	if got := read(cfg.DBPath() + ".before-restore-20261119-010203"); got != "restored-database" {
		t.Errorf("previous database copy = %q", got)
	}
	if got := read(filepath.Join(configDir, "config.toml")); got != "old-host-config" {
		t.Errorf("config.toml after --force = %q", got)
	}
	if got := read(filepath.Join(configDir, "config.toml.before-restore-20261119-010203")); got != "new-host-config" {
		t.Errorf("previous config copy = %q", got)
	}
}

func TestRunSystemRestoreFailure(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(root, "var")
	restorer := &fakeRestorer{err: errors.New("restic: no system snapshot found")}
	var out bytes.Buffer
	err := runSystemRestore(context.Background(), restorer, cfg, filepath.Join(root, "etc"), "", false, time.Now(), &out)
	if err == nil || !strings.Contains(err.Error(), "no system snapshot") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(cfg.DBPath()); !os.IsNotExist(statErr) {
		t.Error("a failed restore must not create a database")
	}
}

func TestRestoreSystemNeedsBackupConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("base_domain = \"dev.example.com\"\ndata_dir = \""+filepath.Join(dir, "var")+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"restore-system", "--config", configPath}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "[storage]") {
		t.Fatalf("err = %v", err)
	}
}
