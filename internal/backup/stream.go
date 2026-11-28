package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Stream is a command whose standard output is stored as one snapshot: a
// base backup or a batch of WAL of point-in-time recovery (ARCHITECTURE
// 10.9).
type Stream struct {
	// App gives the slug the snapshot is tagged with and the masker of the
	// command's error output (App.OutputMasker).
	App App
	// Kind is KindPITRBase or KindWAL; Target the PostgreSQL service.
	Kind   Kind
	Target string
	// FileName is the name of the file in the snapshot.
	FileName string
	// Args are the arguments of the docker command that prints the data.
	Args []string
}

// BackupStream stores the output of the stream's command and returns the
// snapshot ID. It holds the repository like a backup run; the error
// matches ErrBusy when another operation holds it.
func (m *Manager) BackupStream(ctx context.Context, s Stream) (snapshotID string, err error) {
	if err := s.App.validate(); err != nil {
		return "", err
	}
	if (s.Kind != KindPITRBase && s.Kind != KindWAL) || !nameRE.MatchString(s.Target) || s.FileName == "" || strings.ContainsAny(s.FileName, "/\x00") {
		return "", fmt.Errorf("backup: invalid stream %s %q %q", s.Kind, s.Target, s.FileName)
	}
	release, err := m.begin(OpRun, TriggerPITR)
	if err != nil {
		return "", err
	}
	defer release()
	if err := m.prepare(ctx, true); err != nil {
		return "", err
	}
	what := "base backup"
	if s.Kind == KindWAL {
		what = "WAL archive"
	}
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeouts.Backup)
	defer cancel()
	res := m.storeStream(ctx, stream{app: s.App, kind: s.Kind, target: s.Target, fileName: s.FileName, args: s.Args, what: what})
	if res.Err != nil {
		return "", errors.New(m.masker.Mask(res.Err.Error()))
	}
	return res.SnapshotID, nil
}

// WriteStream writes the file of a snapshot BackupStream stored to w. The
// error matches ErrSnapshotNotFound when the ID is not a snapshot of that
// kind of the app.
func (m *Manager) WriteStream(ctx context.Context, app App, kind Kind, snapshotID string, w io.Writer) error {
	if err := app.validate(); err != nil {
		return err
	}
	rec, err := m.appRecord(ctx, app, snapshotID, kind)
	if err != nil {
		return err
	}
	if len(rec.Paths) != 1 || !strings.HasPrefix(rec.Paths[0], "/") {
		return fmt.Errorf("%w: %s does not hold a single file", ErrSnapshotNotFound, snapshotID)
	}
	err = m.restic(ctx, resticCall{
		args:    []string{"dump", "--retry-lock", m.lockWait(), rec.ID, rec.Paths[0]},
		stdout:  w,
		timeout: m.opts.Timeouts.Restore,
	})
	if err != nil {
		return fmt.Errorf("read snapshot %s: %w", shortID(rec.ID), err)
	}
	return nil
}
