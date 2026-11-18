package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
)

// dumpFileName is the name of the dump inside its snapshot.
func dumpFileName(app App) string { return app.Slug + "-" + app.DumpService + ".dump" }

// backupDump runs the dump hook and stores its standard output.
//
// The hook and restic are connected by an operating system pipe whose two
// ends are handed to the two processes directly. restic only sees the end of
// the stream once every copy of the write end is closed, and this process
// keeps one open until it knows how the hook ended:
//
//   - the hook exits with status 0: the write end is closed, restic reads
//     the end of the stream and stores the snapshot;
//   - the hook fails: restic is killed while it still waits for more input,
//     so it cannot have written a snapshot of the truncated dump;
//   - restic ends first: the hook is stopped and the backup has failed.
//
// A hook that succeeds without printing anything makes restic store an empty
// snapshot and exit with status 3; that snapshot is removed again and the
// backup fails, because an empty dump protects nothing.
func (m *Manager) backupDump(ctx context.Context, app App) TargetResult {
	res := TargetResult{App: app.Slug, Kind: KindDump, Target: app.DumpService}
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeouts.Backup)
	defer cancel()

	pr, pw, err := os.Pipe()
	if err != nil {
		res.Err = fmt.Errorf("create pipe: %w", err)
		return res
	}
	defer pr.Close()
	defer pw.Close()

	hookCtx, stopHook := context.WithCancel(ctx)
	defer stopHook()
	resticCtx, stopRestic := context.WithCancel(ctx)
	defer stopRestic()

	hookTail := &tail{}
	hookDone := make(chan error, 1)
	go func() {
		stderr := secrets.NewLineWriter(hookTail, m.masker)
		err := m.opts.Runner.Run(hookCtx, runner.Cmd{Name: "docker", Args: dumpArgs(app), Stdout: pw, Stderr: stderr})
		_ = stderr.Flush()
		hookDone <- err
	}()

	report := &backupReport{}
	resticDone := make(chan error, 1)
	go func() {
		stdout := secrets.NewLineWriter(report, m.masker)
		err := m.restic(resticCtx, resticCall{
			args:    m.backupArgs(appTags(app.Slug, KindDump, app.DumpService), "--stdin", "--stdin-filename", dumpFileName(app)),
			stdin:   pr,
			stdout:  stdout,
			timeout: m.opts.Timeouts.Backup,
		})
		_ = stdout.Flush()
		resticDone <- err
	}()

	var hookErr, resticErr error
	resticFirst := false
	select {
	case hookErr = <-hookDone:
		if hookErr != nil {
			// Kill restic before it can see the end of the stream.
			stopRestic()
			resticErr = <-resticDone
		} else {
			// End of the stream: restic finishes the snapshot.
			_ = pw.Close()
			resticErr = <-resticDone
		}
	case resticErr = <-resticDone:
		// restic cannot have read the end of the stream yet, so it did
		// not finish normally. Closing the read end stops the hook.
		resticFirst = true
		stopHook()
		_ = pr.Close()
		hookErr = <-hookDone
	}

	id := report.snapshotID()
	switch {
	case resticFirst:
		if resticErr == nil {
			resticErr = errors.New("restic ended before the dump command did")
		}
		res.Err = resticErr
	case hookErr != nil:
		res.Err = &commandError{Command: "dump command", Err: hookErr, Detail: hookTail.String()}
	case exitCode(resticErr) == exitIncomplete, resticErr == nil && id != "" && report.bytes() == 0:
		res.Err = errors.New("the dump command printed nothing")
	case resticErr != nil:
		res.Err = resticErr
	}
	if res.Err == nil {
		res.SnapshotID = id
		return res
	}
	// Whatever restic may have stored for a failed dump must not be offered
	// as a dump.
	if id != "" {
		if err := m.discardSnapshot(ctx, id); err != nil {
			res.Err = fmt.Errorf("%w; the incomplete snapshot %s could not be removed: %v", res.Err, shortID(id), err)
		}
	}
	return res
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// discardSnapshot removes one snapshot. Its data goes with the next prune.
func (m *Manager) discardSnapshot(ctx context.Context, id string) error {
	ctx, cancel := cleanupContext(ctx, m.opts.Timeouts.Command)
	defer cancel()
	return m.restic(ctx, resticCall{
		args:    []string{"forget", "--retry-lock", m.lockWait(), id},
		timeout: m.opts.Timeouts.Command,
	})
}

// DumpFile is a stored dump that can be downloaded.
type DumpFile struct {
	// Name is a file name for Content-Disposition, for example
	// blog-db-20261118T180012Z.dump. It only holds letters, digits, dots,
	// dashes and underscores.
	Name string
	Time time.Time

	snapshotID string
	path       string
}

// LookupDump finds a dump snapshot of the app without reading its contents,
// so that response headers can be set before WriteDump streams the file. The
// error matches ErrSnapshotNotFound when the ID is unknown, belongs to
// another app or is not a dump.
func (m *Manager) LookupDump(ctx context.Context, app App, snapshotID string) (DumpFile, error) {
	if err := app.validate(); err != nil {
		return DumpFile{}, err
	}
	rec, err := m.appRecord(ctx, app, snapshotID, KindDump)
	if err != nil {
		return DumpFile{}, err
	}
	if len(rec.Paths) != 1 || !strings.HasPrefix(rec.Paths[0], "/") {
		return DumpFile{}, fmt.Errorf("%w: %s does not hold a single dump file", ErrSnapshotNotFound, snapshotID)
	}
	return DumpFile{
		Name:       fmt.Sprintf("%s-%s-%s.dump", app.Slug, rec.target(), rec.Time.UTC().Format(timestampLayout)),
		Time:       rec.Time.UTC(),
		snapshotID: rec.ID,
		path:       rec.Paths[0],
	}, nil
}

// WriteDump streams the dump to w. A failure in the middle leaves w with a
// truncated file; the returned error is the only sign of it.
func (m *Manager) WriteDump(ctx context.Context, f DumpFile, w io.Writer) error {
	if f.snapshotID == "" {
		return errors.New("backup: WriteDump needs a DumpFile from LookupDump")
	}
	err := m.restic(ctx, resticCall{
		args:    []string{"dump", "--retry-lock", m.lockWait(), f.snapshotID, f.path},
		stdout:  w,
		timeout: m.opts.Timeouts.Restore,
	})
	if err != nil {
		return fmt.Errorf("read dump %s: %w", f.Name, err)
	}
	return nil
}

// Dump is LookupDump followed by WriteDump, for callers that do not need the
// file name first.
func (m *Manager) Dump(ctx context.Context, app App, snapshotID string, w io.Writer) (DumpFile, error) {
	f, err := m.LookupDump(ctx, app, snapshotID)
	if err != nil {
		return DumpFile{}, err
	}
	return f, m.WriteDump(ctx, f, w)
}
