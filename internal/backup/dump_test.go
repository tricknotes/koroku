package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

var blogWithHook = App{Slug: "blog", Project: "paas-blog", DumpService: "db", DumpCommand: "pg_dump -U postgres app"}

func dumpTarget(t *testing.T, res RunResult) TargetResult {
	t.Helper()
	for _, tr := range res.Targets {
		if tr.Kind == KindDump {
			return tr
		}
	}
	t.Fatalf("no dump target in %+v", res.Targets)
	return TargetResult{}
}

func TestDumpStreamsLargeOutput(t *testing.T) {
	e := newEnv(t)
	// Larger than a pipe buffer: the hook can only finish while restic reads.
	big := strings.Repeat("INSERT INTO posts VALUES (1);\n", 20000)
	e.host.set("docker compose", func(context.Context, runner.Cmd) (string, error) { return big, nil })
	var got string
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		data, err := readStdin(ctx, cmd.Stdin)
		got = string(data)
		return summaryLine(e.host.newID(), len(data)), err
	})
	res, err := e.m.BackupApp(context.Background(), blogWithHook)
	if err != nil {
		t.Fatalf("BackupApp: %v", err)
	}
	if got != big {
		t.Errorf("restic read %d bytes, the hook wrote %d", len(got), len(big))
	}
	if tr := dumpTarget(t, res); tr.SnapshotID == "" || tr.Target != "db" {
		t.Errorf("target = %+v", tr)
	}
	if calls := e.host.calls("restic forget"); len(calls) != 0 {
		t.Errorf("a good dump was discarded: %v", calls)
	}
	e.assertCleanCommands()
}

// A failing hook must not leave a snapshot: restic is stopped while it is
// still waiting for input and never sees the end of the stream.
func TestFailingDumpHookStopsResticBeforeEndOfStream(t *testing.T) {
	e := newEnv(t)
	reading := make(chan struct{})
	e.host.set("docker compose", func(_ context.Context, cmd runner.Cmd) (string, error) {
		<-reading
		io.WriteString(cmd.Stderr, "pg_dump: error: connection to server failed: FATAL: database \"app\" does not exist\n")
		return "-- partial dump\n", exit("docker", 1)
	})
	var sawEOF, killed atomic.Bool
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		close(reading)
		data, err := readStdin(ctx, cmd.Stdin)
		if err != nil {
			killed.Store(true)
			return "", err
		}
		sawEOF.Store(true)
		return summaryLine(e.host.newID(), len(data)), nil
	})

	res, err := e.m.BackupApp(context.Background(), blogWithHook)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v, want ErrRunFailed", err)
	}
	if sawEOF.Load() || !killed.Load() {
		t.Errorf("restic saw the end of the truncated stream (eof=%v, killed=%v)", sawEOF.Load(), killed.Load())
	}
	tr := dumpTarget(t, res)
	want := `dump command: exit status 1: pg_dump: error: connection to server failed: FATAL: database "app" does not exist`
	if tr.Err == nil || tr.Err.Error() != want || tr.SnapshotID != "" {
		t.Errorf("target error = %v, snapshot %q; want %q", tr.Err, tr.SnapshotID, want)
	}
	if res.Status != StatusFailed || res.Error != "blog dump db: "+want {
		t.Errorf("status %s, error %q", res.Status, res.Error)
	}
	// The hook's command line may hold a password; it is not in the error.
	if strings.Contains(res.Error, "pg_dump -U") {
		t.Errorf("the error repeats the hook's command line: %s", res.Error)
	}
}

// Should restic nevertheless have reported a snapshot for a failed hook, the
// snapshot is removed. Real restic cannot get that far (see backupDump);
// this is the second line of defense.
func TestSnapshotOfAFailedDumpIsDiscarded(t *testing.T) {
	e := newEnv(t)
	id := e.host.newID()
	reported := make(chan struct{})
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stdout, summaryLine(id, 7))
		close(reported)
		<-ctx.Done()
		return "", ctx.Err()
	})
	e.host.set("docker compose", func(context.Context, runner.Cmd) (string, error) {
		<-reported
		return "partial", exit("docker", 137)
	})

	res, err := e.m.BackupApp(context.Background(), blogWithHook)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v, want ErrRunFailed", err)
	}
	if tr := dumpTarget(t, res); tr.Err == nil || tr.SnapshotID != "" {
		t.Errorf("target = %+v", tr)
	}
	assertLines(t, "forget", e.host.calls("restic forget"), []string{"restic forget --retry-lock 1m0s " + id})
	e.assertCleanCommands()
}

// restic stores an empty snapshot and exits with status 3 when the stream is
// empty. That is a failed dump, and the snapshot goes away again.
func TestEmptyDumpFailsAndIsDiscarded(t *testing.T) {
	e := newEnv(t)
	e.host.set("docker compose", func(context.Context, runner.Cmd) (string, error) { return "", nil })
	id := e.host.newID()
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		if _, err := readStdin(ctx, cmd.Stdin); err != nil {
			return "", err
		}
		io.WriteString(cmd.Stderr, `{"message_type":"exit_error","code":3,"message":"Warning: at least one source file could not be read"}`+"\n")
		return `{"message_type":"error","error":{"message":"failed to save /blog-db.dump: read blog-db.dump: no data read"}}` + "\n" + summaryLine(id, 0), exit("restic", exitIncomplete)
	})

	res, err := e.m.BackupApp(context.Background(), blogWithHook)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v, want ErrRunFailed", err)
	}
	if want := "blog dump db: the dump command printed nothing"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
	assertLines(t, "forget", e.host.calls("restic forget"), []string{"restic forget --retry-lock 1m0s " + id})

	// When even the removal fails, the error says which snapshot is bad.
	e.host.set("restic forget", func(context.Context, runner.Cmd) (string, error) { return "", exit("restic", 1) })
	res, _ = e.m.BackupApp(context.Background(), blogWithHook)
	if !strings.Contains(res.Error, "the incomplete snapshot "+id[:8]+" could not be removed") {
		t.Errorf("error = %q", res.Error)
	}
}

func TestResticFailureStopsTheDumpHook(t *testing.T) {
	e := newEnv(t)
	hookStarted, hookStopped := make(chan struct{}), make(chan struct{})
	e.host.set("docker compose", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		close(hookStarted)
		defer close(hookStopped)
		// Keep writing until told to stop, like a dump of a large database.
		for {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				if _, err := io.WriteString(cmd.Stdout, strings.Repeat("x", 4096)); err != nil {
					return "", err
				}
			}
		}
	})
	e.host.set("restic backup", func(_ context.Context, cmd runner.Cmd) (string, error) {
		<-hookStarted
		io.WriteString(cmd.Stderr, "Fatal: unable to save snapshot: connection reset\n")
		return "", exit("restic", 1)
	})

	res, err := e.m.BackupApp(context.Background(), blogWithHook)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v, want ErrRunFailed", err)
	}
	// BackupApp only returns once the hook is gone.
	select {
	case <-hookStopped:
	default:
		t.Fatal("the dump hook kept running after restic failed")
	}
	if want := "blog dump db: restic backup: exit status 1: Fatal: unable to save snapshot: connection reset"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
}

func TestLookupAndWriteDump(t *testing.T) {
	e := newEnv(t)
	at := time.Date(2026, 11, 18, 18, 0, 12, 0, time.FixedZone("JST", 9*3600))
	dump := e.host.addRecord(at, "/blog-db.dump", "app:blog", "kind:dump", "target:db")
	vol := e.host.addRecord(at, "/var/lib/docker/volumes/paas-blog_uploads/_data", "app:blog", "kind:volume", "target:uploads")
	other := e.host.addRecord(at, "/shop-db.dump", "app:shop", "kind:dump", "target:db")
	e.host.set("restic dump", func(context.Context, runner.Cmd) (string, error) { return "CREATE TABLE posts;\n", nil })

	f, err := e.m.LookupDump(context.Background(), blog, dump.ShortID)
	if err != nil {
		t.Fatalf("LookupDump: %v", err)
	}
	if f.Name != "blog-db-20261118T090012Z.dump" || !f.Time.Equal(at) {
		t.Errorf("dump file = %+v", f)
	}
	// Looking up must not read the dump.
	if calls := e.host.calls("restic dump"); len(calls) != 0 {
		t.Errorf("LookupDump read the dump: %v", calls)
	}
	var body bytes.Buffer
	if err := e.m.WriteDump(context.Background(), f, &body); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	if body.String() != "CREATE TABLE posts;\n" {
		t.Errorf("body = %q", body.String())
	}
	assertLines(t, "commands", e.host.calls(), []string{
		"restic snapshots --json --tag app:blog --retry-lock 1m0s",
		"restic dump --retry-lock 1m0s " + dump.ID + " /blog-db.dump",
	})
	e.assertCleanCommands()

	body.Reset()
	if f2, err := e.m.Dump(context.Background(), blog, dump.ID, &body); err != nil || f2.Name != f.Name || body.Len() == 0 {
		t.Errorf("Dump by full ID: %+v, %v", f2, err)
	}

	e.host.run.Reset()
	for name, id := range map[string]string{
		"another app's dump": other.ShortID,
		"a volume snapshot":  vol.ShortID,
		"an unknown ID":      "ffffffff",
		"a malformed ID":     "latest",
		"an option":          "--target=/",
		"too short":          "0000",
	} {
		if _, err := e.m.LookupDump(context.Background(), blog, id); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("%s: err = %v, want ErrSnapshotNotFound", name, err)
		}
	}
	if calls := e.host.calls("restic dump"); len(calls) != 0 {
		t.Errorf("a refused dump was read: %v", calls)
	}
	if err := e.m.WriteDump(context.Background(), DumpFile{Name: "x"}, io.Discard); err == nil {
		t.Error("WriteDump accepted a DumpFile that did not come from LookupDump")
	}
}

func TestSnapshotsListsNewestFirst(t *testing.T) {
	e := newEnv(t)
	day := func(d int) time.Time { return time.Date(2026, 11, d, 18, 0, 0, 0, time.UTC) }
	older := e.host.addRecord(day(16), "/data", "app:blog", "kind:volume", "target:uploads")
	newest := e.host.addRecord(day(18), "/blog-db.dump", "app:blog", "kind:dump", "target:db")
	middle := e.host.addRecord(day(17), "/data", "app:blog", "kind:volume", "target:uploads")
	e.host.addRecord(day(18), "/data", "app:blog", "kind:other")               // no target, unknown kind
	e.host.addRecord(day(18), "/data", "app:blog2", "kind:volume", "target:x") // another app with a similar slug
	e.host.addRecord(day(18), "/stage", "system", "kind:system")

	got, err := e.m.Snapshots(context.Background(), blog)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	want := []Snapshot{
		{ID: newest.ShortID, Time: day(18), Kind: KindDump, Target: "db"},
		{ID: middle.ShortID, Time: day(17), Kind: KindVolume, Target: "uploads"},
		{ID: older.ShortID, Time: day(16), Kind: KindVolume, Target: "uploads"},
	}
	if len(got) != len(want) {
		t.Fatalf("snapshots = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("snapshot %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	assertLines(t, "commands", e.host.calls(), []string{"restic snapshots --json --tag app:blog --retry-lock 1m0s"})
	e.assertCleanCommands()

	sys, err := e.m.SystemSnapshots(context.Background())
	if err != nil || len(sys) != 1 || sys[0].Kind != KindSystem {
		t.Errorf("system snapshots = %+v, %v", sys, err)
	}
}

func TestSnapshotsOfAMissingRepositoryIsEmpty(t *testing.T) {
	e := newEnv(t)
	e.host.set("restic snapshots", func(context.Context, runner.Cmd) (string, error) { return "", exit("restic", exitNoRepository) })
	got, err := e.m.Snapshots(context.Background(), blog)
	if err != nil || len(got) != 0 {
		t.Errorf("Snapshots = %+v, %v", got, err)
	}
	// Listing never creates a repository.
	if calls := e.host.calls("restic init"); len(calls) != 0 {
		t.Errorf("listing initialized a repository: %v", calls)
	}
}

func TestSnapshotsClearsAStaleLockOnce(t *testing.T) {
	e := newEnv(t)
	e.host.addRecord(testNow, "/data", "app:blog", "kind:volume", "target:uploads")
	var attempts atomic.Int32
	e.host.set("restic snapshots", func(_ context.Context, cmd runner.Cmd) (string, error) {
		if attempts.Add(1) == 1 {
			io.WriteString(cmd.Stderr, "repository is already locked exclusively by PID 4242\n")
			return "", exit("restic", exitLocked)
		}
		return e.host.snapshots(cmd), nil
	})
	got, err := e.m.Snapshots(context.Background(), blog)
	if err != nil || len(got) != 1 {
		t.Fatalf("Snapshots = %+v, %v", got, err)
	}
	assertLines(t, "commands", e.host.calls(), []string{
		"restic snapshots --json --tag app:blog --retry-lock 1m0s",
		"restic unlock",
		"restic snapshots --json --tag app:blog --retry-lock 1m0s",
	})
}
