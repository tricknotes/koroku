//go:build integration

package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/runner"
)

// realRunner runs restic for real and stands in for Docker: volume queries
// are answered from a table (Docker's volume directories are only readable
// by root), and `docker compose exec ... sh -c <command>` runs the command
// with a local shell, so that the dump hook is a real process writing into
// the real pipe.
type realRunner struct {
	mu      sync.Mutex
	volumes map[string]map[string]string // project -> short name -> mountpoint
}

func (r *realRunner) Run(ctx context.Context, cmd runner.Cmd) error {
	if cmd.Name == "restic" {
		return runner.Exec{}.Run(ctx, cmd)
	}
	if cmd.Name != "docker" {
		return fmt.Errorf("unexpected command %s", cmd)
	}
	switch commandKey(cmd) {
	case "docker volume ls":
		project := strings.TrimPrefix(cmd.Args[3], "label=com.docker.compose.project=")
		r.mu.Lock()
		defer r.mu.Unlock()
		for short := range r.volumes[project] {
			fmt.Fprintln(cmd.Stdout, project+"_"+short)
		}
		return nil
	case "docker volume inspect":
		r.mu.Lock()
		defer r.mu.Unlock()
		var out []map[string]any
		for _, name := range cmd.Args[3:] {
			project, short, _ := strings.Cut(name, "_")
			out = append(out, map[string]any{
				"Name":       name,
				"Mountpoint": r.volumes[project][short],
				"Labels":     map[string]string{"com.docker.compose.project": project, "com.docker.compose.volume": short},
			})
		}
		return json.NewEncoder(cmd.Stdout).Encode(out)
	case "docker compose":
		// compose -p <project> exec -T <service> sh -c <command>
		return runner.Exec{}.Run(ctx, runner.Cmd{Name: "sh", Args: cmd.Args[len(cmd.Args)-2:], Stdout: cmd.Stdout, Stderr: cmd.Stderr})
	}
	return fmt.Errorf("unexpected command %s", cmd)
}

// site is a Manager on a real restic repository in a temporary directory.
type site struct {
	t     *testing.T
	m     *Manager
	run   *realRunner
	rec   *recorder
	ctl   *controller
	clock *clock.Fake
	dir   string
	apps  []App

	dbContent string
	configDir string
}

func newSite(t *testing.T) *site {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not on PATH")
	}
	s := &site{
		t:         t,
		run:       &realRunner{volumes: map[string]map[string]string{}},
		rec:       &recorder{},
		ctl:       &controller{},
		clock:     clock.NewFake(time.Now().UTC().Truncate(time.Second)),
		dir:       t.TempDir(),
		dbContent: "SQLite format 3 (copy 1)",
	}
	password := filepath.Join(s.dir, "restic.key")
	if err := os.WriteFile(password, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.configDir = filepath.Join(s.dir, "etc-paas")
	writeFiles(t, s.configDir, map[string]string{"config.toml": "base_domain = \"example.com\"\n", "keys/app.pem": "private key"})
	if err := os.Chmod(filepath.Join(s.configDir, "keys", "app.pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		Repository: Repository{
			Path:         filepath.Join(s.dir, "repo"),
			PasswordFile: password,
			CacheDir:     filepath.Join(s.dir, "cache"),
		},
		Runner:     s.run,
		Clock:      s.clock,
		Recorder:   s.rec,
		Controller: s.ctl,
		Apps:       func(context.Context) ([]App, error) { return s.apps, nil },
		System: &System{
			ConfigDir: s.configDir,
			SnapshotDB: func(_ context.Context, dest string) error {
				return os.WriteFile(dest, []byte(s.dbContent), 0o600)
			},
		},
		WorkDir:  filepath.Join(s.dir, "work"),
		TrashDir: filepath.Join(s.dir, "restore-trash"),
		Timeouts: Timeouts{Command: time.Minute, Backup: 2 * time.Minute, Restore: 2 * time.Minute, Maintenance: 2 * time.Minute, LockWait: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.m = m
	return s
}

func (s *site) volume(app App, short string, files map[string]string) string {
	s.t.Helper()
	dir := filepath.Join(s.dir, "volumes", app.Project+"_"+short, "_data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	writeFiles(s.t, dir, files)
	s.run.mu.Lock()
	if s.run.volumes[app.Project] == nil {
		s.run.volumes[app.Project] = map[string]string{}
	}
	s.run.volumes[app.Project][short] = dir
	s.run.mu.Unlock()
	return dir
}

func (s *site) snapshots(app App) []Snapshot {
	s.t.Helper()
	snaps, err := s.m.Snapshots(testContext(s.t), app)
	if err != nil {
		s.t.Fatalf("Snapshots: %v", err)
	}
	return snaps
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func count(snaps []Snapshot, kind Kind, target string) int {
	n := 0
	for _, s := range snaps {
		if s.Kind == kind && s.Target == target {
			n++
		}
	}
	return n
}

func TestIntegrationVolumeBackupAndRestore(t *testing.T) {
	s := newSite(t)
	ctx := testContext(t)
	original := map[string]string{"index.html": "<h1>blog</h1>", "2026/11/post.md": "first post", "empty": ""}
	mount := s.volume(blog, "uploads", original)
	if err := os.Symlink("index.html", filepath.Join(mount, "home")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(mount, "2026", "11", "post.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.volume(blog, "cache", map[string]string{"tmp": "not worth keeping"})
	app := blog
	app.ExcludeVolumes = []string{"cache"}
	s.apps = []App{app, {Slug: "shop", Project: "paas-shop"}}

	// First use: the repository does not exist yet and is created.
	res, err := s.m.RunAll(ctx, TriggerManual)
	if err != nil {
		t.Fatalf("RunAll: %v (%+v)", err, res)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "repo", "config")); err != nil {
		t.Fatalf("the repository was not initialized: %v", err)
	}
	snaps := s.snapshots(app)
	if len(snaps) != 1 || snaps[0].Kind != KindVolume || snaps[0].Target != "uploads" || len(snaps[0].ID) != 8 {
		t.Fatalf("snapshots after the first run = %+v", snaps)
	}
	// The size is what the files of the volume add up to.
	if want := int64(len("<h1>blog</h1>") + len("first post")); snaps[0].Size != want {
		t.Errorf("snapshot size = %d, want %d", snaps[0].Size, want)
	}
	if other := s.snapshots(App{Slug: "shop", Project: "paas-shop"}); len(other) != 0 {
		t.Errorf("an app without volumes has snapshots: %+v", other)
	}

	// The volume changes and loses data.
	changed := map[string]string{"index.html": "<h1>defaced</h1>", "new.txt": "written after the backup"}
	if err := os.RemoveAll(filepath.Join(mount, "2026")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, mount, changed)
	changed["empty"] = ""

	// A dump or another app's ID cannot be restored into the volume.
	if _, err := s.m.RestoreVolume(ctx, App{Slug: "shop", Project: "paas-shop"}, snaps[0].ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("restore of another app's snapshot: %v", err)
	}

	result, err := s.m.RestoreVolume(ctx, app, snaps[0].ID)
	if err != nil {
		t.Fatalf("RestoreVolume: %v", err)
	}
	if got := readTree(t, mount); !sameTree(got, original) {
		t.Errorf("volume after the restore = %v, want %v", got, original)
	}
	if link, err := os.Readlink(filepath.Join(mount, "home")); err != nil || link != "index.html" {
		t.Errorf("symlink after the restore: %q, %v", link, err)
	}
	if info, err := os.Stat(filepath.Join(mount, "2026", "11", "post.md")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file mode after the restore: %v, %v", info, err)
	}
	wantTrash := filepath.Join(s.dir, "restore-trash", "blog-uploads-"+s.clock.Now().Format(timestampLayout))
	if result.TrashDir != wantTrash || result.Volume != "uploads" || result.SnapshotID != snaps[0].ID {
		t.Errorf("result = %+v, want trash %s", result, wantTrash)
	}
	if got := readTree(t, result.TrashDir); !sameTree(got, changed) {
		t.Errorf("trash = %v, want what the volume held before the restore: %v", got, changed)
	}
	assertLines(t, "app control", s.ctl.log(), []string{"stop blog", "start blog"})

	// The trash is kept for a week.
	s.clock.Advance(6 * 24 * time.Hour)
	if removed, err := s.m.CleanTrash(ctx); err != nil || len(removed) != 0 {
		t.Errorf("CleanTrash after 6 days: %v, %v", removed, err)
	}
	s.clock.Advance(2 * 24 * time.Hour)
	if removed, err := s.m.CleanTrash(ctx); err != nil || !slices.Equal(removed, []string{wantTrash}) {
		t.Errorf("CleanTrash after 8 days: %v, %v", removed, err)
	}
}

func TestIntegrationRestoreOfAMissingSnapshotDataRollsBack(t *testing.T) {
	s := newSite(t)
	ctx := testContext(t)
	current := map[string]string{"a.txt": "current"}
	mount := s.volume(blog, "uploads", current)
	s.apps = []App{blog}
	if _, err := s.m.RunAll(ctx, TriggerManual); err != nil {
		t.Fatal(err)
	}
	snaps := s.snapshots(blog)
	// Break the repository: without its data packs restic cannot restore.
	if err := os.RemoveAll(filepath.Join(s.dir, "repo", "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.dir, "cache")); err != nil {
		t.Fatal(err)
	}

	_, err := s.m.RestoreVolume(ctx, blog, snaps[0].ID)
	var rerr *RestoreError
	if !errors.As(err, &rerr) || !rerr.RolledBack || !rerr.AppStarted {
		t.Fatalf("err = %v, want a rolled back RestoreError", err)
	}
	if got := readTree(t, mount); !sameTree(got, current) {
		t.Errorf("volume after the failed restore = %v, want %v", got, current)
	}
	assertLines(t, "app control", s.ctl.log(), []string{"stop blog", "start blog"})
}

func TestIntegrationDumpRoundTrip(t *testing.T) {
	s := newSite(t)
	ctx := testContext(t)
	// 3 MiB of incompressible data: several pipe buffers and restic chunks.
	dump := make([]byte, 3<<20)
	if _, err := rand.Read(dump); err != nil {
		t.Fatal(err)
	}
	dumpFile := filepath.Join(s.dir, "database.dump")
	if err := os.WriteFile(dumpFile, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	app := App{Slug: "blog", Project: "paas-blog", DumpService: "db", DumpCommand: "cat '" + dumpFile + "'"}

	res, err := s.m.BackupApp(ctx, app)
	if err != nil {
		t.Fatalf("BackupApp: %v (%+v)", err, res)
	}
	snaps := s.snapshots(app)
	if len(snaps) != 1 || snaps[0].Kind != KindDump || snaps[0].Target != "db" || snaps[0].Size != int64(len(dump)) {
		t.Fatalf("snapshots = %+v, want one dump of %d bytes", snaps, len(dump))
	}
	if !strings.HasPrefix(res.Targets[0].SnapshotID, snaps[0].ID) {
		t.Errorf("reported snapshot %s, listed %s", res.Targets[0].SnapshotID, snaps[0].ID)
	}

	f, err := s.m.LookupDump(ctx, app, snaps[0].ID)
	if err != nil {
		t.Fatalf("LookupDump: %v", err)
	}
	if want := "blog-db-" + snaps[0].Time.Format(timestampLayout) + ".dump"; f.Name != want {
		t.Errorf("file name = %q, want %q", f.Name, want)
	}
	var body bytes.Buffer
	if err := s.m.WriteDump(ctx, f, &body); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	if !bytes.Equal(body.Bytes(), dump) {
		t.Errorf("downloaded %d bytes that differ from the %d bytes dumped", body.Len(), len(dump))
	}
	if _, err := s.m.LookupDump(ctx, App{Slug: "shop", Project: "paas-shop"}, snaps[0].ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("another app got the dump: %v", err)
	}

	// A hook that fails after writing most of its output leaves no snapshot.
	failing := app
	failing.DumpCommand = "head -c 2000000 '" + dumpFile + "'; echo 'pg_dump: error: connection lost' >&2; exit 3"
	res, err = s.m.BackupApp(ctx, failing)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("a failing hook: err = %v", err)
	}
	if want := "blog dump db: dump command: exit status 3: pg_dump: error: connection lost"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
	if after := s.snapshots(app); len(after) != 1 || after[0].ID != snaps[0].ID {
		t.Errorf("snapshots after the failed hook = %+v", after)
	}

	// A hook that prints nothing is a failure, and its empty snapshot is removed.
	empty := app
	empty.DumpCommand = "true"
	res, err = s.m.BackupApp(ctx, empty)
	if !errors.Is(err, ErrRunFailed) || res.Error != "blog dump db: the dump command printed nothing" {
		t.Fatalf("an empty dump: err = %v, error = %q", err, res.Error)
	}
	if after := s.snapshots(app); len(after) != 1 || after[0].ID != snaps[0].ID {
		t.Errorf("snapshots after the empty dump = %+v", after)
	}

	// restic was killed above and left its lock behind. Maintenance, which
	// needs the repository exclusively, still works.
	if err := s.m.Forget(ctx); err != nil {
		t.Errorf("Forget after a killed restic: %v", err)
	}
	if err := s.m.Check(ctx); err != nil {
		t.Errorf("Check: %v", err)
	}
	assertLines(t, "recorded runs", s.rec.finishedRuns(), []string{
		"1 succeeded",
		"2 failed blog dump db: dump command: exit status 3: pg_dump: error: connection lost",
		"3 failed blog dump db: the dump command printed nothing",
	})
}

func TestIntegrationRetentionKeepsGenerationsPerTarget(t *testing.T) {
	s := newSite(t)
	ctx := testContext(t)
	uploads := s.volume(blog, "uploads", map[string]string{"a.txt": "1"})
	s.volume(blog, "assets", map[string]string{"b.txt": "1"})
	app := App{Slug: "blog", Project: "paas-blog", DumpService: "db", DumpCommand: "echo dump"}
	shop := App{Slug: "shop", Project: "paas-shop"}
	s.volume(shop, "uploads", map[string]string{"c.txt": "1"})
	s.apps = []App{app, shop}

	// Three runs on the same day. Per group, restic keeps the newest one as
	// the daily generation and, while the group is young, its oldest.
	for i := range 3 {
		writeFiles(t, uploads, map[string]string{"a.txt": fmt.Sprint(i)})
		s.dbContent = fmt.Sprintf("SQLite format 3 (copy %d)", i)
		res, err := s.m.RunAll(ctx, TriggerScheduled)
		if err != nil {
			t.Fatalf("scheduled run %d: %v (%+v)", i, err, res)
		}
		if len(res.Targets) != 5 {
			t.Fatalf("targets of run %d = %+v", i, res.Targets)
		}
		// Snapshot times have a resolution that makes runs within the same
		// second indistinguishable to the policy.
		time.Sleep(1100 * time.Millisecond)
	}

	blogSnaps := s.snapshots(app)
	for _, want := range []struct {
		kind   Kind
		target string
	}{{KindVolume, "uploads"}, {KindVolume, "assets"}, {KindDump, "db"}} {
		if n := count(blogSnaps, want.kind, want.target); n != 2 {
			t.Errorf("blog %s %s: %d snapshots kept, want 2 (its own generations)", want.kind, want.target, n)
		}
	}
	// Same volume name, different app: its own group as well.
	if n := count(s.snapshots(shop), KindVolume, "uploads"); n != 2 {
		t.Errorf("shop volume uploads: %d snapshots kept, want 2", n)
	}
	system, err := s.m.SystemSnapshots(ctx)
	if err != nil || len(system) != 2 {
		t.Errorf("system snapshots = %+v, %v; want 2", system, err)
	}
	if err := s.m.Check(ctx); err != nil {
		t.Errorf("Check after forget --prune: %v", err)
	}
}

func TestIntegrationSystemBackupAndRestore(t *testing.T) {
	s := newSite(t)
	ctx := testContext(t)

	// Before the first backup there is nothing to restore, and looking
	// does not create a repository.
	if _, err := s.m.RestoreSystem(ctx, "", filepath.Join(s.dir, "early")); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("restore without a repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "repo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a restore created the repository: %v", err)
	}
	if snaps := s.snapshots(blog); len(snaps) != 0 {
		t.Fatalf("snapshots without a repository = %+v", snaps)
	}

	if _, err := s.m.RunAll(ctx, TriggerManual); err != nil {
		t.Fatalf("first run: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	s.dbContent = "SQLite format 3 (copy 2)"
	writeFiles(t, s.configDir, map[string]string{"config.toml": "base_domain = \"example.org\"\n"})
	if _, err := s.m.RunAll(ctx, TriggerManual); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "work", "system")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging directory with secrets was left behind: %v", err)
	}
	system, err := s.m.SystemSnapshots(ctx)
	if err != nil || len(system) != 2 {
		t.Fatalf("system snapshots = %+v, %v", system, err)
	}

	// A new host: a fresh Manager on the same repository restores the latest.
	fresh := newSite(t)
	fresh.m.opts.Repository = s.m.opts.Repository
	dest := filepath.Join(fresh.dir, "restore")
	got, err := fresh.m.RestoreSystem(ctx, "", dest)
	if err != nil {
		t.Fatalf("RestoreSystem: %v", err)
	}
	if got.SnapshotID != system[0].ID || got.Dir != dest || got.DatabasePath != filepath.Join(dest, "paas.db") || got.ConfigDir != filepath.Join(dest, "config") {
		t.Errorf("result = %+v", got)
	}
	want := map[string]string{
		"paas.db":             "SQLite format 3 (copy 2)",
		"config/config.toml":  "base_domain = \"example.org\"\n",
		"config/keys/app.pem": "private key",
	}
	if tree := readTree(t, dest); !sameTree(tree, want) {
		t.Errorf("restored files = %v, want %v", tree, want)
	}
	if info, err := os.Stat(filepath.Join(got.ConfigDir, "keys", "app.pem")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode of a restored key file: %v, %v", info, err)
	}

	// The older snapshot, by ID.
	older, err := fresh.m.RestoreSystem(ctx, system[1].ID, filepath.Join(fresh.dir, "older"))
	if err != nil {
		t.Fatalf("RestoreSystem of the older snapshot: %v", err)
	}
	if data, _ := os.ReadFile(older.DatabasePath); string(data) != "SQLite format 3 (copy 1)" {
		t.Errorf("older database = %q", data)
	}
	if data, _ := io.ReadAll(mustOpen(t, filepath.Join(older.ConfigDir, "config.toml"))); string(data) != "base_domain = \"example.com\"\n" {
		t.Errorf("older config = %q", data)
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
