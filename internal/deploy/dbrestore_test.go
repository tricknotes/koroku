package deploy

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

const (
	restoreSnapshotID = "9fceb02d0ae598e95dc970b74767f19372d61af8"
	// The dump BackupDump of the fake stores.
	preRestoreSnapshotID = "4bba301e5d2c7a90"
)

// newRestoreHarness is the copy harness whose staging app has a dump hook
// too, with a dump of it to restore and the one the fake takes before.
func newRestoreHarness(t *testing.T) *copyHarness {
	t.Helper()
	c := newCopyHarness(t, nil)
	app := c.reload("blog-staging")
	app.DumpHookService, app.DumpHookCommand = "db", `pg_dump -Fc -Z0 "$POSTGRES_DB"`
	if err := c.store.UpdateApp(context.Background(), app, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	c.target = c.reload("blog-staging")
	writer := func(data string) func(context.Context, io.Writer) error {
		return func(_ context.Context, w io.Writer) error {
			_, err := io.WriteString(w, data)
			return err
		}
	}
	c.backups.dumps = map[string]DumpSnapshot{
		restoreSnapshotID: {
			ID: restoreSnapshotID, Time: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), Target: "db",
			Write: writer("the dump of 09:00\n"),
		},
		preRestoreSnapshotID: {
			ID: preRestoreSnapshotID, Time: c.clock.Now(), Target: "db",
			Write: writer("the data before the restore\n"),
		},
	}
	return c
}

func (c *copyHarness) startRestore(id string) (store.DBCopy, error) {
	return c.engine.StartRestore(context.Background(), c.reload("blog-staging"), id)
}

func TestRestoreLoadsTheChosenDump(t *testing.T) {
	c := newRestoreHarness(t)
	started, err := c.startRestore(restoreSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if started.Kind != store.CopyKindRestore || started.SourceSnapshotID != restoreSnapshotID || started.SourceSlug != "blog-staging" {
		t.Errorf("record = %+v", started)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopySucceeded || got.BackupSnapshotID != preRestoreSnapshotID {
		t.Fatalf("restore = %+v\n%s", got, c.copyLog(started.ID))
	}
	if c.loaded != "the dump of 09:00\n" || !c.loadSawEOF {
		t.Errorf("loaded %q (end of input %v)", c.loaded, c.loadSawEOF)
	}
	if !slices.Equal(c.backups.backedUp, []string{"blog-staging"}) {
		t.Errorf("backed up %v", c.backups.backedUp)
	}
	calls := c.composeCalls()
	wantStop, wantStart := "koroku-blog-staging stop web worker", "koroku-blog-staging start web worker"
	if i, j := slices.Index(calls, wantStop), slices.Index(calls, wantStart); i < 0 || j < i {
		t.Errorf("calls = %v", calls)
	}
	log := c.copyLog(started.ID)
	for _, step := range []string{"check", "backup", "stop", "load", "start", "done"} {
		if !strings.Contains(log, " "+step+" ") {
			t.Errorf("log has no %s step:\n%s", step, log)
		}
	}
	if strings.Contains(log, " reload ") {
		t.Errorf("a good load was followed by a reload:\n%s", log)
	}
}

// Without the dump of the current data nothing is changed.
func TestRestoreStopsWhenTheCurrentDataCannotBeBackedUp(t *testing.T) {
	c := newRestoreHarness(t)
	c.backups.err = errors.New("another backup is running; copy again when it has finished")
	started, err := c.startRestore(restoreSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "backup before the restore") {
		t.Fatalf("restore = %+v", got)
	}
	for _, call := range c.composeCalls() {
		if strings.Contains(call, " stop ") || strings.Contains(call, " exec ") {
			t.Errorf("the restore went on: %v", c.composeCalls())
		}
	}
}

// A load that fails is followed by loading the dump taken before, and the
// app is started on the data it had.
func TestRestoreLoadsTheDataBackWhenTheLoadFails(t *testing.T) {
	c := newRestoreHarness(t)
	c.loadErrs = []error{errors.New("pg_restore: error: input file is too short")}
	started, err := c.startRestore(restoreSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "the load command failed") {
		t.Fatalf("restore = %+v", got)
	}
	if want := []string{"the dump of 09:00\n", "the data before the restore\n"}; !slices.Equal(c.loads, want) {
		t.Errorf("loads = %q, want %q", c.loads, want)
	}
	if !slices.Contains(c.composeCalls(), "koroku-blog-staging start web worker") {
		t.Errorf("the services were not started again: %v", c.composeCalls())
	}
	log := c.copyLog(started.ID)
	if !strings.Contains(log, " reload ") || !strings.Contains(log, "Loaded the data before the restore back.") {
		t.Errorf("log:\n%s", log)
	}
}

// When loading the data back fails too, the database may hold part of a
// dump: the app is not served.
func TestRestoreKeepsTheAppStoppedWhenTheReloadFails(t *testing.T) {
	c := newRestoreHarness(t)
	c.loadErrs = []error{errors.New("first"), errors.New("second")}
	started, err := c.startRestore(restoreSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.waitCopy(started.ID); got.Status != store.CopyFailed {
		t.Fatalf("restore = %+v", got)
	}
	for _, call := range c.composeCalls() {
		if strings.Contains(call, " start ") {
			t.Errorf("services were started: %v", c.composeCalls())
		}
	}
	log := c.copyLog(started.ID)
	if !strings.Contains(log, "stay stopped") || !strings.Contains(log, preRestoreSnapshotID[:8]) {
		t.Errorf("log does not say what is left and where the data is:\n%s", log)
	}
}

func TestRestoreRefusals(t *testing.T) {
	c := newRestoreHarness(t)
	if _, err := c.startRestore("0123456789abcdef"); !errors.Is(err, ErrDumpNotFound) {
		t.Errorf("unknown snapshot: err = %v", err)
	}
	c.backups.dumps["aaaabbbbccccdddd"] = DumpSnapshot{ID: "aaaabbbbccccdddd", Target: "mysql"}
	if _, err := c.startRestore("aaaabbbbccccdddd"); !errors.Is(err, ErrDumpOfOtherService) {
		t.Errorf("dump of another service: err = %v", err)
	}
	ctx := context.Background()
	app := c.reload("blog-staging")
	app.DumpHookService, app.DumpHookCommand = "", ""
	if err := c.store.UpdateApp(ctx, app, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.startRestore(restoreSnapshotID); !errors.Is(err, ErrRestoreNeedsDumpHook) {
		t.Errorf("no dump hook: err = %v", err)
	}
	app.LoadHookService, app.LoadHookCommand = "", ""
	if err := c.store.UpdateApp(ctx, app, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.startRestore(restoreSnapshotID); !errors.Is(err, ErrNoLoadHook) {
		t.Errorf("no load hook: err = %v", err)
	}
	if copies, _ := c.store.ListDBCopies(ctx, app.ID, 10); len(copies) != 0 {
		t.Errorf("refused restores were recorded: %+v", copies)
	}
}
