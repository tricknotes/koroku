package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// restoreTarget returns the --target of a `restic restore` command.
func restoreTarget(cmd runner.Cmd) string {
	return cmd.Args[slices.Index(cmd.Args, "--target")+1]
}

func sameTree(got, want map[string]string) bool { return fmt.Sprint(got) == fmt.Sprint(want) }

func TestRestoreVolume(t *testing.T) {
	e := newEnv(t)
	current := map[string]string{"a.txt": "current", "sub/b.txt": "current b"}
	mount := e.volume(blog, "uploads", current)
	snap := e.host.addRecord(testNow.Add(-24*time.Hour), mount, "app:blog", "kind:volume", "target:uploads")
	e.host.set("restic restore", func(_ context.Context, cmd runner.Cmd) (string, error) {
		// The app is down and the volume is empty when restic starts.
		if got := e.ctl.log(); !slices.Equal(got, []string{"stop blog"}) {
			t.Errorf("app state at restore time: %v", got)
		}
		if left := readTree(t, mount); len(left) != 0 {
			t.Errorf("the volume was not emptied before the restore: %v", left)
		}
		writeFiles(t, restoreTarget(cmd), map[string]string{"a.txt": "restored"})
		return "", nil
	})

	res, err := e.m.RestoreVolume(context.Background(), blog, snap.ShortID)
	if err != nil {
		t.Fatalf("RestoreVolume: %v", err)
	}
	trash := filepath.Join(e.dir, "restore-trash", "blog-uploads-20261118T180000Z")
	if res != (RestoreResult{SnapshotID: snap.ShortID, Volume: "uploads", TrashDir: trash}) {
		t.Errorf("result = %+v", res)
	}
	if got := readTree(t, mount); !sameTree(got, map[string]string{"a.txt": "restored"}) {
		t.Errorf("volume after the restore = %v", got)
	}
	if got := readTree(t, trash); !sameTree(got, current) {
		t.Errorf("trash = %v, want the previous contents %v", got, current)
	}
	assertLines(t, "app control", e.ctl.log(), []string{"stop blog", "start blog"})
	assertLines(t, "commands", e.host.calls(), []string{
		"restic cat config",
		"restic snapshots --json --tag app:blog --retry-lock 1m0s",
		"docker volume ls --filter label=com.docker.compose.project=paas-blog --format {{.Name}}",
		"docker volume inspect -- paas-blog_uploads",
		"restic restore --retry-lock 1m0s --target " + mount + " " + snap.ID,
	})
	e.assertCleanCommands()
	if e.m.State().Busy {
		t.Error("the Manager stayed busy")
	}
}

func TestRestoreVolumeRollsBackWhenResticFails(t *testing.T) {
	e := newEnv(t)
	current := map[string]string{"a.txt": "current", "sub/b.txt": "current b"}
	mount := e.volume(blog, "uploads", current)
	snap := e.host.addRecord(testNow, mount, "app:blog", "kind:volume", "target:uploads")
	e.host.set("restic restore", func(_ context.Context, cmd runner.Cmd) (string, error) {
		// A restore that dies half way leaves partial files behind.
		writeFiles(t, restoreTarget(cmd), map[string]string{"a.txt": "half", "partial/c.txt": "half"})
		fmt.Fprintf(cmd.Stderr, "Fatal: storage rejected %s\n", testSecretKey)
		return "", exit("restic", 1)
	})

	_, err := e.m.RestoreVolume(context.Background(), blog, snap.ShortID)
	var rerr *RestoreError
	if !errors.As(err, &rerr) {
		t.Fatalf("err = %v, want *RestoreError", err)
	}
	if !rerr.RolledBack || !rerr.AppStarted || rerr.TrashDir != "" || rerr.RollbackErr != nil || rerr.StartErr != nil {
		t.Errorf("restore error = %+v", rerr)
	}
	want := "restore failed: restic restore: exit status 1: Fatal: storage rejected [MASKED]; the previous contents of the volume were put back; the app was started again"
	if err.Error() != want {
		t.Errorf("error = %q\n want %q", err.Error(), want)
	}
	if got := readTree(t, mount); !sameTree(got, current) {
		t.Errorf("volume after the rollback = %v, want %v", got, current)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "restore-trash")); len(entries) != 0 {
		t.Errorf("the trash keeps a directory after a rollback: %v", entries)
	}
	assertLines(t, "app control", e.ctl.log(), []string{"stop blog", "start blog"})
	e.assertCleanCommands()
}

func TestRestoreVolumeRollsBackAfterCancellation(t *testing.T) {
	e := newEnv(t)
	current := map[string]string{"a.txt": "current"}
	mount := e.volume(blog, "uploads", current)
	snap := e.host.addRecord(testNow, mount, "app:blog", "kind:volume", "target:uploads")
	ctx, cancel := context.WithCancel(context.Background())
	e.host.set("restic restore", func(ctx context.Context, _ runner.Cmd) (string, error) {
		cancel() // the HTTP client went away
		<-ctx.Done()
		return "", ctx.Err()
	})
	_, err := e.m.RestoreVolume(ctx, blog, snap.ShortID)
	var rerr *RestoreError
	if !errors.As(err, &rerr) || !rerr.RolledBack || !rerr.AppStarted {
		t.Fatalf("err = %v", err)
	}
	if got := readTree(t, mount); !sameTree(got, current) {
		t.Errorf("volume after the rollback = %v, want %v", got, current)
	}
	assertLines(t, "app control", e.ctl.log(), []string{"stop blog", "start blog"})
}

func TestRestoreVolumeReportsWhereTheDataIsWhenRollbackFails(t *testing.T) {
	e := newEnv(t)
	mount := e.volume(blog, "uploads", map[string]string{"a.txt": "current"})
	snap := e.host.addRecord(testNow, mount, "app:blog", "kind:volume", "target:uploads")
	trash := filepath.Join(e.dir, "restore-trash", "blog-uploads-20261118T180000Z")
	e.host.set("restic restore", func(context.Context, runner.Cmd) (string, error) {
		// Make putting the data back impossible: the volume disappears.
		if err := os.RemoveAll(mount); err != nil {
			t.Error(err)
		}
		return "", exit("restic", 1)
	})
	_, err := e.m.RestoreVolume(context.Background(), blog, snap.ShortID)
	var rerr *RestoreError
	if !errors.As(err, &rerr) {
		t.Fatalf("err = %v, want *RestoreError", err)
	}
	if rerr.RolledBack || rerr.AppStarted || rerr.TrashDir != trash || rerr.RollbackErr == nil {
		t.Errorf("restore error = %+v", rerr)
	}
	if !strings.Contains(err.Error(), "are kept in "+trash) || !strings.Contains(err.Error(), "the app was left stopped") {
		t.Errorf("error = %q", err.Error())
	}
	// The previous contents are still there to be recovered by hand, and
	// the app was not started on a broken volume.
	if got := readTree(t, trash); !sameTree(got, map[string]string{"a.txt": "current"}) {
		t.Errorf("trash = %v", got)
	}
	assertLines(t, "app control", e.ctl.log(), []string{"stop blog"})
}

func TestRestoreVolumeReportsAFailedStart(t *testing.T) {
	e := newEnv(t)
	mount := e.volume(blog, "uploads", map[string]string{"a.txt": "current"})
	snap := e.host.addRecord(testNow, mount, "app:blog", "kind:volume", "target:uploads")
	e.ctl.startErr = errors.New("containers did not become healthy")

	// After a successful restore the data stays; the error says the app is down.
	res, err := e.m.RestoreVolume(context.Background(), blog, snap.ShortID)
	if err == nil || !strings.Contains(err.Error(), "the volume was restored but the app could not be started: containers did not become healthy") {
		t.Fatalf("err = %v", err)
	}
	if res.TrashDir == "" {
		t.Errorf("result = %+v", res)
	}

	// After a failed restore both failures are reported.
	e.clock.Advance(time.Minute)
	e.host.set("restic restore", func(context.Context, runner.Cmd) (string, error) { return "", exit("restic", 1) })
	_, err = e.m.RestoreVolume(context.Background(), blog, snap.ShortID)
	var rerr *RestoreError
	if !errors.As(err, &rerr) || !rerr.RolledBack || rerr.AppStarted || rerr.StartErr == nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "the app could not be started again: containers did not become healthy") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRestoreVolumeRefusesForeignAndWrongSnapshots(t *testing.T) {
	e := newEnv(t)
	current := map[string]string{"a.txt": "current"}
	mount := e.volume(blog, "uploads", current)
	shop := e.host.addRecord(testNow, "/data", "app:shop", "kind:volume", "target:uploads")
	dump := e.host.addRecord(testNow, "/blog-db.dump", "app:blog", "kind:dump", "target:db")
	system := e.host.addRecord(testNow, "/stage", "system", "kind:system")
	gone := e.host.addRecord(testNow, "/data", "app:blog", "kind:volume", "target:removed")

	for name, id := range map[string]string{
		"another app's volume": shop.ShortID,
		"a dump":               dump.ShortID,
		"the system":           system.ShortID,
		"an unknown ID":        "ffffffff",
		"a keyword":            "latest",
		"an option":            "--delete",
	} {
		if _, err := e.m.RestoreVolume(context.Background(), blog, id); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("%s: err = %v, want ErrSnapshotNotFound", name, err)
		}
	}
	// A volume the app no longer has is a different, plain error.
	if _, err := e.m.RestoreVolume(context.Background(), blog, gone.ShortID); err == nil || errors.Is(err, ErrSnapshotNotFound) || !strings.Contains(err.Error(), `no volume "removed"`) {
		t.Errorf("restore into a missing volume: %v", err)
	}

	if len(e.ctl.log()) != 0 {
		t.Errorf("a refused restore touched the app: %v", e.ctl.log())
	}
	if calls := e.host.calls("restic restore"); len(calls) != 0 {
		t.Errorf("a refused restore ran restic: %v", calls)
	}
	if got := readTree(t, mount); !sameTree(got, current) {
		t.Errorf("a refused restore changed the volume: %v", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "restore-trash")); len(entries) != 0 {
		t.Errorf("a refused restore left trash: %v", entries)
	}
}

func TestRestoreVolumePassesOnTheStopError(t *testing.T) {
	e := newEnv(t)
	current := map[string]string{"a.txt": "current"}
	mount := e.volume(blog, "uploads", current)
	snap := e.host.addRecord(testNow, mount, "app:blog", "kind:volume", "target:uploads")
	errDeploying := errors.New("busy: a deployment is in progress")
	e.ctl.stopErr = fmt.Errorf("stop blog: %w", errDeploying)

	_, err := e.m.RestoreVolume(context.Background(), blog, snap.ShortID)
	var rerr *RestoreError
	if !errors.Is(err, errDeploying) || errors.As(err, &rerr) {
		t.Fatalf("err = %v, want the controller's error, not a RestoreError", err)
	}
	assertLines(t, "app control", e.ctl.log(), []string{"stop blog"})
	if got := readTree(t, mount); !sameTree(got, current) {
		t.Errorf("the volume changed although the app could not be stopped: %v", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "restore-trash")); len(entries) != 0 {
		t.Errorf("trash left behind: %v", entries)
	}
	if calls := e.host.calls("restic restore"); len(calls) != 0 {
		t.Errorf("restic ran: %v", calls)
	}
}

func TestCleanTrashRemovesOnlyExpiredRestores(t *testing.T) {
	e := newEnv(t)
	trash := filepath.Join(e.dir, "restore-trash")
	name := func(age time.Duration) string {
		return "blog-my-uploads-" + testNow.Add(-age).Format(timestampLayout)
	}
	expired, edge, fresh := name(8*24*time.Hour), name(7*24*time.Hour), name(time.Hour)
	for _, dir := range []string{expired, edge, fresh, "notes", "blog-uploads-notatime"} {
		writeFiles(t, filepath.Join(trash, dir), map[string]string{"sub/file": "x"})
	}
	writeFiles(t, trash, map[string]string{"blog-uploads-20200101T000000Z": "a file, not a restore"})

	removed, err := e.m.CleanTrash(context.Background())
	if err != nil {
		t.Fatalf("CleanTrash: %v", err)
	}
	assertLines(t, "removed", removed, []string{filepath.Join(trash, expired)})
	entries, _ := os.ReadDir(trash)
	var left []string
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	want := []string{edge, fresh, "notes", "blog-uploads-notatime", "blog-uploads-20200101T000000Z"}
	slices.Sort(want)
	assertLines(t, "kept", left, want)

	// No trash directory at all is fine.
	e2 := newEnv(t)
	if removed, err := e2.m.CleanTrash(context.Background()); err != nil || len(removed) != 0 {
		t.Errorf("CleanTrash without a directory: %v, %v", removed, err)
	}
}

func TestRestoreSystem(t *testing.T) {
	e := newEnv(t)
	old := e.host.addRecord(testNow.Add(-48*time.Hour), "/var/lib/paas/backup-work/system", "system", "kind:system")
	latest := e.host.addRecord(testNow.Add(-time.Hour), "/var/lib/paas/backup-work/system", "system", "kind:system")
	// Tagged kind:system by someone else, but not ours.
	e.host.addRecord(testNow, "/x", "kind:system", "app:blog")
	e.host.set("restic restore", func(_ context.Context, cmd runner.Cmd) (string, error) {
		writeFiles(t, restoreTarget(cmd), map[string]string{"paas.db": "sqlite", "config/config.toml": "x", "config/restic.key": "k"})
		return "", nil
	})

	dest := filepath.Join(e.dir, "restore", "system")
	got, err := e.m.RestoreSystem(context.Background(), "", dest)
	if err != nil {
		t.Fatalf("RestoreSystem: %v", err)
	}
	want := SystemRestore{
		SnapshotID:   latest.ShortID,
		Time:         latest.Time,
		Dir:          dest,
		DatabasePath: filepath.Join(dest, "paas.db"),
		ConfigDir:    filepath.Join(dest, "config"),
	}
	if got != want {
		t.Errorf("result = %+v\n  want %+v", got, want)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the staging directory must be private: %v %v", info, err)
	}
	assertLines(t, "commands", e.host.calls(), []string{
		"restic cat config",
		"restic snapshots --json --tag kind:system --retry-lock 1m0s",
		"restic restore --retry-lock 1m0s --target " + dest + " " + latest.ID,
	})
	e.assertCleanCommands()

	// A given snapshot, into a fresh directory.
	dest2 := filepath.Join(e.dir, "restore", "older")
	if got, err := e.m.RestoreSystem(context.Background(), old.ShortID, dest2); err != nil || got.SnapshotID != old.ShortID {
		t.Errorf("restore of a given snapshot: %+v, %v", got, err)
	}

	// Never into a directory that already holds something.
	if _, err := e.m.RestoreSystem(context.Background(), "", dest); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Errorf("restore into a used directory: %v", err)
	}
	if _, err := e.m.RestoreSystem(context.Background(), "", "relative/dir"); err == nil {
		t.Error("a relative destination was accepted")
	}
	if _, err := e.m.RestoreSystem(context.Background(), "ffffffff", filepath.Join(e.dir, "restore", "x")); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("unknown snapshot: %v", err)
	}
}

func TestRestoreSystemNeverCreatesARepository(t *testing.T) {
	e := newEnv(t)
	e.host.set("restic cat", func(_ context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stderr, "Fatal: repository does not exist\n")
		return "", exit("restic", exitNoRepository)
	})
	_, err := e.m.RestoreSystem(context.Background(), "", filepath.Join(e.dir, "restore"))
	if !errors.Is(err, ErrNoRepository) {
		t.Fatalf("err = %v, want ErrNoRepository", err)
	}
	assertLines(t, "commands", e.host.calls(), []string{"restic cat config"})

	// An existing repository without a system snapshot is "not found".
	e2 := newEnv(t)
	if _, err := e2.m.RestoreSystem(context.Background(), "", filepath.Join(e2.dir, "restore")); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("err = %v, want ErrSnapshotNotFound", err)
	}
}
