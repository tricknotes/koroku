package backup

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tricknotes/koroku/internal/runner"
)

// A sleeping app's dump runs inside WithDumpService, which starts the
// service; when it cannot, the dump fails without running the hook.
func TestDumpRunsInsideWithDumpService(t *testing.T) {
	e := newEnv(t)
	var inside atomic.Bool
	e.host.set("docker compose", func(context.Context, runner.Cmd) (string, error) {
		if !inside.Load() {
			t.Error("the hook ran outside WithDumpService")
		}
		return "-- dump\n", nil
	})
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		data, err := readStdin(ctx, cmd.Stdin)
		return summaryLine(e.host.newID(), len(data)), err
	})
	app := blogWithHook
	app.WithDumpService = func(ctx context.Context, run func(context.Context) error) error {
		inside.Store(true)
		defer inside.Store(false)
		return run(ctx)
	}
	res, err := e.m.BackupApp(context.Background(), app)
	if err != nil {
		t.Fatalf("BackupApp: %v", err)
	}
	if tr := dumpTarget(t, res); tr.SnapshotID == "" || tr.Err != nil {
		t.Errorf("target = %+v", tr)
	}

	app.WithDumpService = func(context.Context, func(context.Context) error) error {
		return errors.New("start the db service of the sleeping app: boom")
	}
	before := len(e.host.calls("docker compose"))
	res, _ = e.m.BackupApp(context.Background(), app)
	if tr := dumpTarget(t, res); tr.Err == nil || !strings.Contains(tr.Err.Error(), "boom") {
		t.Errorf("target = %+v", tr)
	}
	if after := len(e.host.calls("docker compose")); after != before {
		t.Error("the hook ran although its service could not be started")
	}
}
