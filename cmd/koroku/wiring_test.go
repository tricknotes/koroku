package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/logs"
	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
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
	if got.Slug != "blog" || got.Project != "koroku-blog" || got.DumpService != "db" || got.DumpCommand != "pg_dump -U postgres app" ||
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

// A backup that takes the dump hands the backup package the values of the
// app's variables to mask in the dump command's output: the saved ones and
// the ones the running containers still hold.
func TestToDumpingAppMasksTheValuesOfTheApp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "blog")
	box, err := secrets.NewBox([]byte(strings.Repeat("k", secrets.KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("s3cret-db-passw0rd"), secrets.EnvVarContext(app.ID, "DATABASE_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutEnvVar(ctx, app.ID, store.EnvVar{Key: "DATABASE_PASSWORD", ValueEncrypted: sealed}); err != nil {
		t.Fatal(err)
	}
	engine := deploy.New(deploy.Options{Store: st, Runner: &runner.Fake{}, AppsDir: t.TempDir(), Box: box})
	// The live env file holds a value that was deleted since the deployment.
	if err := os.MkdirAll(filepath.Dir(engine.EnvFile("blog")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.EnvFile("blog"), []byte("OLD_TOKEN=deleted-t0ken-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	current := int64(1)
	app.DesiredState, app.CurrentDeploymentID = store.DesiredRunning, &current
	app.DumpHookService, app.DumpHookCommand = "db", "pg_dump -U postgres app"

	got := toDumpingApp(app, &appController{store: st, engine: engine})
	if got.OutputMasker == nil || got.WithDumpService == nil {
		t.Fatalf("dumping app = %+v", got)
	}
	masker, err := got.OutputMasker(ctx)
	if err != nil {
		t.Fatalf("OutputMasker: %v", err)
	}
	line := masker.Mask("connection to postgres://app:s3cret-db-passw0rd@db failed (deleted-t0ken-value)")
	if want := "connection to postgres://app:[MASKED]@db failed ([MASKED])"; line != want {
		t.Errorf("masked = %q, want %q", line, want)
	}

	// Without the engine the values are unknown: an error, so that the
	// backup keeps nothing of the output.
	got = toDumpingApp(app, &appController{store: st})
	if _, err := got.OutputMasker(ctx); err == nil {
		t.Error("OutputMasker without an engine: no error")
	}
	// An app whose dump hook does not run has nothing to mask.
	app.DesiredState = store.DesiredStopped
	if got := toDumpingApp(app, &appController{store: st, engine: engine}); got.OutputMasker != nil {
		t.Errorf("stopped app = %+v", got)
	}
}

// The API lists an app's volumes through the backup package, by the Compose
// project of the app, and without a backup manager.
func TestAppVolumesAdapter(t *testing.T) {
	fake := &runner.Fake{Handler: func(_ context.Context, cmd runner.Cmd) (string, error) {
		switch strings.Join(cmd.Args[:2], " ") {
		case "volume ls":
			return "koroku-blog_uploads\nkoroku-blog_pgdata\n", nil
		case "volume inspect":
			return `[
				{"Name": "koroku-blog_pgdata", "Mountpoint": "/var/lib/docker/volumes/koroku-blog_pgdata/_data",
				 "Labels": {"com.docker.compose.project": "koroku-blog", "com.docker.compose.volume": "pgdata"}},
				{"Name": "koroku-blog_uploads", "Mountpoint": "/var/lib/docker/volumes/koroku-blog_uploads/_data",
				 "Labels": {"com.docker.compose.project": "koroku-blog", "com.docker.compose.volume": "uploads"}}
			]`, nil
		}
		return "", errors.New("unexpected command: " + cmd.String())
	}}
	var lister api.AppVolumes = appVolumes{runner: fake}

	names, err := lister.VolumeNames(context.Background(), store.App{Slug: "blog"})
	if err != nil || strings.Join(names, ",") != "pgdata,uploads" {
		t.Errorf("VolumeNames = %v, %v", names, err)
	}
	var commands []string
	for _, cmd := range fake.Calls() {
		commands = append(commands, cmd.String())
	}
	want := []string{
		"docker volume ls --filter label=com.docker.compose.project=koroku-blog --format {{.Name}}",
		"docker volume inspect -- koroku-blog_pgdata koroku-blog_uploads",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands = %q, want %q", commands, want)
	}

	// An app that was never deployed has no volumes.
	fake.Handler = func(context.Context, runner.Cmd) (string, error) { return "", nil }
	if names, err := lister.VolumeNames(context.Background(), store.App{Slug: "notes"}); err != nil || len(names) != 0 {
		t.Errorf("never deployed app: %v, %v", names, err)
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
	if err := rec.FinishRun(ctx, id, backup.StatusSucceeded, "", started.Add(time.Minute), nil); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	failed, _ := rec.StartRun(ctx, backup.TriggerManual, started.Add(time.Hour))
	_ = rec.FinishRun(ctx, failed, backup.StatusFailed, "restic: repository unreachable", started.Add(2*time.Hour), nil)

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
	cfg.Storage = config.Storage{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "koroku", AccessKeyID: "a", SecretAccessKey: "b"}
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
	if repo.Endpoint != "https://s3.example.com" || repo.Bucket != "koroku" || repo.Prefix != "restic" ||
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
	if apps[0] != (logs.App{Slug: "blog", Project: "koroku-blog", AccessLogPath: "/var/log/caddy/apps/blog.log"}) {
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
	dbPath := filepath.Join(destDir, "koroku.db")
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

// fakeBackupManager scripts the backup manager for the adapter tests.
type fakeBackupManager struct {
	state      backup.State
	startErr   error
	result     backup.RunResult
	backupErr  error
	snapshots  []backup.Snapshot
	restoreErr error
	dumpErr    error
	dumped     string
	apps       []backup.App
}

func (f *fakeBackupManager) State() backup.State { return f.state }

func (f *fakeBackupManager) Start(_ context.Context, trigger backup.Trigger) (*backup.Run, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &backup.Run{ID: 7, Trigger: trigger, StartedAt: time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeBackupManager) BackupApp(_ context.Context, app backup.App) (backup.RunResult, error) {
	f.apps = append(f.apps, app)
	return f.result, f.backupErr
}

func (f *fakeBackupManager) BackupDump(_ context.Context, app backup.App) (string, error) {
	f.apps = append(f.apps, app)
	return "4bba301e5d2c7a90", f.backupErr
}

func (f *fakeBackupManager) Snapshots(context.Context, backup.App) ([]backup.Snapshot, error) {
	return f.snapshots, nil
}

func (f *fakeBackupManager) RestoreVolume(context.Context, backup.App, string) (backup.RestoreResult, error) {
	return backup.RestoreResult{}, f.restoreErr
}

func (f *fakeBackupManager) LookupDump(_ context.Context, _ backup.App, id string) (backup.DumpFile, error) {
	if f.dumpErr != nil {
		return backup.DumpFile{}, f.dumpErr
	}
	return backup.DumpFile{Name: "blog-db-" + id + ".dump"}, nil
}

func (f *fakeBackupManager) WriteDump(_ context.Context, _ backup.DumpFile, w io.Writer) error {
	_, err := io.WriteString(w, f.dumped)
	return err
}

func (f *fakeBackupManager) BackupStream(context.Context, backup.Stream) (string, error) {
	return "", errors.New("not used")
}

func TestBackupServiceAdapter(t *testing.T) {
	ctx := context.Background()
	manager := &fakeBackupManager{}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	svc := &backupService{
		manager:   manager,
		scheduler: &backup.Scheduler{Hour: 3, Location: time.FixedZone("JST", 9*3600)},
		clock:     fixedClock{now},
		serverCtx: ctx,
	}
	current := int64(1)
	app := store.App{Slug: "blog", DesiredState: store.DesiredRunning, CurrentDeploymentID: &current,
		DumpHookService: "db", DumpHookCommand: "pg_dump app"}

	// 03:00 JST is 18:00 UTC.
	if next := svc.NextRun(); !next.Equal(time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("NextRun = %v", next)
	}
	if svc.Running() {
		t.Error("Running must be false while idle")
	}
	manager.state = backup.State{Busy: true, Operation: backup.OpRun}
	if !svc.Running() {
		t.Error("Running must follow the manager")
	}

	run, err := svc.StartManual(ctx)
	if err != nil || run.ID != 7 || run.Trigger != store.BackupTriggerManual || run.Status != store.BackupRunning {
		t.Errorf("StartManual = %+v, %v", run, err)
	}
	manager.startErr = &backup.BusyError{}
	if _, err := svc.StartManual(ctx); !errors.Is(err, api.ErrBackupBusy) {
		t.Errorf("StartManual while busy: %v", err)
	}

	manager.snapshots = []backup.Snapshot{
		{ID: "4bba301e", Kind: backup.KindVolume, Target: "pgdata", Size: 2048},
		{ID: "9c0ffee1", Kind: backup.KindDump, Target: "db"},
	}
	snapshots, err := svc.Snapshots(ctx, app)
	if err != nil || len(snapshots) != 2 || snapshots[0].SizeBytes == nil || *snapshots[0].SizeBytes != 2048 ||
		snapshots[1].SizeBytes != nil || snapshots[1].Kind != "dump" {
		t.Errorf("Snapshots = %+v, %v", snapshots, err)
	}

	manager.restoreErr = backup.ErrSnapshotNotFound
	if err := svc.RestoreVolume(ctx, app, "4bba301e"); !errors.Is(err, api.ErrSnapshotNotFound) {
		t.Errorf("RestoreVolume: %v", err)
	}
	manager.restoreErr = nil
	if err := svc.RestoreVolume(ctx, app, "4bba301e"); err != nil {
		t.Errorf("RestoreVolume: %v", err)
	}

	manager.dumped = "-- dump --"
	name, write, err := svc.OpenDump(ctx, app, "9c0ffee1")
	var out bytes.Buffer
	if err != nil || name != "blog-db-9c0ffee1.dump" || write(&out) != nil || out.String() != "-- dump --" {
		t.Errorf("OpenDump = %q, %v, %q", name, err, out.String())
	}
	manager.dumpErr = backup.ErrSnapshotNotFound
	if _, _, err := svc.OpenDump(ctx, app, "9c0ffee1"); !errors.Is(err, api.ErrSnapshotNotFound) {
		t.Errorf("OpenDump of an unknown snapshot: %v", err)
	}
}

// TestBackupBeforeDeployOutcome pins what the deploy engine sees: only a
// fully successful backup lets the deploy continue.
func TestBackupBeforeDeployOutcome(t *testing.T) {
	ctx := context.Background()
	manager := &fakeBackupManager{}
	svc := &backupService{manager: manager}
	current := int64(1)
	app := store.App{Slug: "blog", DesiredState: store.DesiredRunning, CurrentDeploymentID: &current,
		BackupExcludeVolumes: []string{"cache"}, DumpHookService: "db", DumpHookCommand: "pg_dump app"}

	manager.result = backup.RunResult{Status: backup.StatusSucceeded}
	if err := svc.BackupApp(ctx, app); err != nil {
		t.Errorf("successful backup: %v", err)
	}
	if len(manager.apps) != 1 || manager.apps[0].Project != "koroku-blog" || manager.apps[0].DumpService != "db" ||
		len(manager.apps[0].ExcludeVolumes) != 1 {
		t.Errorf("app passed to the manager = %+v", manager.apps)
	}

	manager.result = backup.RunResult{Status: backup.StatusFailed, Error: "volume pgdata: restic exited with 1"}
	if err := svc.BackupApp(ctx, app); err == nil || !strings.Contains(err.Error(), "pgdata") {
		t.Errorf("a failed target must fail the deploy: %v", err)
	}
	manager.result, manager.backupErr = backup.RunResult{}, &backup.BusyError{}
	if err := svc.BackupApp(ctx, app); err == nil || !strings.Contains(err.Error(), "another backup is running") {
		t.Errorf("busy: %v", err)
	}
	manager.backupErr = errors.New("restic: repository unreachable")
	if err := svc.BackupApp(ctx, app); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("other errors: %v", err)
	}
}

// TestDatabaseCopyBackups pins what a database copy sees: the newest dump
// snapshot of the source, and the backup of the target.
func TestDatabaseCopyBackups(t *testing.T) {
	ctx := context.Background()
	manager := &fakeBackupManager{}
	svc := &backupService{manager: manager}
	current := int64(1)
	app := store.App{Slug: "blog", DesiredState: store.DesiredRunning, CurrentDeploymentID: &current,
		DumpHookService: "db", DumpHookCommand: "pg_dump app"}

	if _, err := svc.LatestDump(ctx, app); !errors.Is(err, deploy.ErrNoDumpSnapshot) {
		t.Errorf("without snapshots: %v", err)
	}
	when := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	manager.snapshots = []backup.Snapshot{
		{ID: "4bba301e", Kind: backup.KindVolume, Target: "pgdata"},
		{ID: "9c0ffee1", Time: when, Kind: backup.KindDump, Target: "db"},
		{ID: "11111111", Kind: backup.KindDump, Target: "db"},
	}
	manager.dumped = "-- dump --"
	latest, err := svc.LatestDump(ctx, app)
	var out bytes.Buffer
	if err != nil || latest.ID != "9c0ffee1" || !latest.Time.Equal(when) || latest.Write(ctx, &out) != nil || out.String() != "-- dump --" {
		t.Errorf("LatestDump = %+v, %v, %q", latest, err, out.String())
	}

	if id, err := svc.BackupDump(ctx, app); err != nil || id == "" || manager.apps[0].DumpService != "db" {
		t.Errorf("BackupDump = %q, %v (%+v)", id, err, manager.apps)
	}
	manager.backupErr = &backup.BusyError{}
	if _, err := svc.BackupDump(ctx, app); err == nil || !strings.Contains(err.Error(), "another backup is running") {
		t.Errorf("busy: %v", err)
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// TestAppControllerKeepsStoppedAppsStopped: a volume restore stops the app
// and starts it again, but an app that was stopped before stays stopped.
func TestAppControllerKeepsStoppedAppsStopped(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "blog")
	d, _, err := st.EnqueueDeployment(ctx, app.ID, "main", strings.Repeat("a", 40), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.StartDeployment(ctx, d.ID, time.Now())
	if err := st.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	fake := &runner.Fake{}
	engine := deploy.New(deploy.Options{Store: st, Runner: fake, AppsDir: t.TempDir()})
	controller := &appController{store: st, engine: engine}
	target := backup.App{Slug: "blog", Project: "koroku-blog"}
	composeCalls := func() []string {
		var subs []string
		for _, c := range fake.Calls() {
			subs = append(subs, c.Args[len(c.Args)-1])
		}
		return subs
	}

	// Running app: stopped for the restore, started afterwards.
	if err := controller.Stop(ctx, target); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got, _ := st.GetApp(ctx, "blog"); got.DesiredState != store.DesiredStopped {
		t.Errorf("after Stop: %q", got.DesiredState)
	}
	if err := controller.Start(ctx, target); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, _ := st.GetApp(ctx, "blog"); got.DesiredState != store.DesiredRunning {
		t.Errorf("after Start: %q", got.DesiredState)
	}
	if calls := composeCalls(); len(calls) != 2 || calls[0] != "stop" || calls[1] != "start" {
		t.Errorf("docker calls = %v", calls)
	}

	// Already stopped: nothing runs and it stays stopped.
	if err := st.SetAppDesiredState(ctx, app.ID, store.DesiredStopped, time.Now()); err != nil {
		t.Fatal(err)
	}
	fake.Reset()
	if err := controller.Stop(ctx, target); err != nil {
		t.Fatalf("Stop of a stopped app: %v", err)
	}
	if err := controller.Start(ctx, target); err != nil {
		t.Fatalf("Start of a previously stopped app: %v", err)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("a stopped app must not be touched: %v", calls)
	}
	if got, _ := st.GetApp(ctx, "blog"); got.DesiredState != store.DesiredStopped {
		t.Errorf("a stopped app was started: %q", got.DesiredState)
	}
	// The memory of "was stopped" is used once: a later restore of the app,
	// now running, starts it again.
	_ = st.SetAppDesiredState(ctx, app.ID, store.DesiredRunning, time.Now())
	_ = controller.Stop(ctx, target)
	_ = controller.Start(ctx, target)
	if got, _ := st.GetApp(ctx, "blog"); got.DesiredState != store.DesiredRunning {
		t.Errorf("second restore left the app %q", got.DesiredState)
	}

	if err := controller.Stop(ctx, backup.App{Slug: "missing"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown app: %v", err)
	}
}
