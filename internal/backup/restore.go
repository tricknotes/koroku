package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// timestampLayout is used in trash directory names and dump file names.
const timestampLayout = "20060102T150405Z"

// RestoreResult describes a finished volume restore.
type RestoreResult struct {
	// SnapshotID is the short ID of the restored snapshot.
	SnapshotID string
	// Volume is the volume name as written in the Compose file.
	Volume string
	// TrashDir holds what the volume contained before the restore. It is
	// kept for Options.TrashRetention.
	TrashDir string
}

// RestoreError is returned when a volume restore failed after the app was
// stopped. It says in which state the volume and the app were left.
type RestoreError struct {
	// Err is why the restore failed.
	Err error
	// RolledBack is true when the previous contents are back in the volume.
	RolledBack bool
	// TrashDir holds the previous contents when they could not be put back
	// (RolledBack is false); the volume may be empty or half restored.
	TrashDir string
	// RollbackErr is why the previous contents could not be put back.
	RollbackErr error
	// AppStarted is true when the app runs again. After a failed rollback
	// the app is deliberately left stopped: starting it on an empty volume
	// could make it initialize fresh data over the half-restored state.
	AppStarted bool
	// StartErr is why the app could not be started again.
	StartErr error
}

func (e *RestoreError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "restore failed: %v", e.Err)
	if e.RolledBack {
		b.WriteString("; the previous contents of the volume were put back")
	} else {
		fmt.Fprintf(&b, "; the previous contents of the volume could not be put back (%v) and are kept in %s", e.RollbackErr, e.TrashDir)
	}
	switch {
	case e.AppStarted:
		b.WriteString("; the app was started again")
	case e.StartErr != nil:
		fmt.Fprintf(&b, "; the app could not be started again: %v", e.StartErr)
	default:
		b.WriteString("; the app was left stopped")
	}
	return b.String()
}

func (e *RestoreError) Unwrap() error { return e.Err }

// RestoreVolume replaces the contents of one volume with a snapshot:
// stop the app, move the current contents to the trash directory, restore,
// start the app (ARCHITECTURE 10.2).
//
// Before the app is stopped the error is plain: ErrBusy when another backup
// operation runs, ErrSnapshotNotFound when the snapshot is not a volume
// snapshot of this app, or the wrapped error of AppController.Stop. From
// then on a failure is a *RestoreError: the previous contents are moved
// back and the app is started again, and when even that fails the error
// names the trash directory holding the data.
//
// The work survives a canceled ctx only as far as putting things back is
// concerned; pass a context that outlives the HTTP request if the restore
// should not stop when the client goes away.
func (m *Manager) RestoreVolume(ctx context.Context, app App, snapshotID string) (RestoreResult, error) {
	if err := app.validate(); err != nil {
		return RestoreResult{}, err
	}
	if m.opts.Controller == nil {
		return RestoreResult{}, errors.New("backup: Options.Controller is required to restore a volume")
	}
	if m.opts.TrashDir == "" {
		return RestoreResult{}, errors.New("backup: Options.TrashDir is required to restore a volume")
	}
	release, err := m.begin(OpRestore, "")
	if err != nil {
		return RestoreResult{}, err
	}
	defer release()
	log := m.opts.Logger.With("app", app.Slug, "snapshot", snapshotID)

	if err := m.ensureRepository(ctx, false); err != nil {
		return RestoreResult{}, err
	}
	rec, err := m.appRecord(ctx, app, snapshotID, KindVolume)
	if err != nil {
		return RestoreResult{}, err
	}
	vols, err := m.volumes(ctx, app.Project)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("list volumes: %w", err)
	}
	var vol volume
	for _, v := range vols {
		if v.Short == rec.target() {
			vol = v
		}
	}
	if vol.Name == "" {
		return RestoreResult{}, fmt.Errorf("the app has no volume %q to restore into; deploy the app first", rec.target())
	}
	if info, err := os.Stat(vol.Mountpoint); err != nil || !info.IsDir() {
		return RestoreResult{}, fmt.Errorf("the mountpoint of volume %s is not a readable directory", vol.Short)
	}

	// Everything that can fail without touching the app comes first.
	trash := filepath.Join(m.opts.TrashDir, fmt.Sprintf("%s-%s-%s", app.Slug, vol.Short, m.opts.Clock.Now().UTC().Format(timestampLayout)))
	if err := os.MkdirAll(m.opts.TrashDir, 0o700); err != nil {
		return RestoreResult{}, fmt.Errorf("create the trash directory: %w", err)
	}
	if err := os.Mkdir(trash, 0o700); err != nil {
		return RestoreResult{}, fmt.Errorf("create the trash directory: %w", err)
	}
	if same, err := sameFilesystem(trash, vol.Mountpoint); err != nil || !same {
		_ = os.Remove(trash)
		if err != nil {
			return RestoreResult{}, fmt.Errorf("compare filesystems: %w", err)
		}
		return RestoreResult{}, fmt.Errorf("the trash directory %s is not on the same filesystem as the volume", m.opts.TrashDir)
	}

	log.Info("volume restore: stopping the app", "volume", vol.Short)
	if err := m.control(ctx, m.opts.Controller.Stop, app); err != nil {
		_ = os.Remove(trash)
		return RestoreResult{}, fmt.Errorf("stop the app: %w", err)
	}

	fail := func(cause error, clear bool) (RestoreResult, error) {
		rerr := &RestoreError{Err: cause, TrashDir: trash}
		// The request may be gone by now; the volume still has to be fixed.
		cctx, cancel := cleanupContext(ctx, m.opts.Timeouts.Control)
		defer cancel()
		if err := putBack(trash, vol.Mountpoint, clear); err != nil {
			rerr.RollbackErr = err
		} else {
			rerr.RolledBack = true
			rerr.TrashDir = ""
			if err := m.opts.Controller.Start(cctx, app); err != nil {
				rerr.StartErr = err
			} else {
				rerr.AppStarted = true
			}
		}
		log.Error("volume restore failed", "volume", vol.Short, "error", m.masker.Mask(rerr.Error()))
		return RestoreResult{}, rerr
	}

	if err := moveContents(vol.Mountpoint, trash); err != nil {
		return fail(fmt.Errorf("move the current contents aside: %w", err), false)
	}
	err = m.restic(ctx, resticCall{
		args:    []string{"restore", "--retry-lock", m.lockWait(), "--target", vol.Mountpoint, rec.ID},
		timeout: m.opts.Timeouts.Restore,
	})
	if err != nil {
		return fail(err, true)
	}

	result := RestoreResult{SnapshotID: shortID(rec.ID), Volume: vol.Short, TrashDir: trash}
	// Starting gets its own time even when ctx is about to end: the data is
	// in place and the app should not stay down.
	cctx, cancel := cleanupContext(ctx, m.opts.Timeouts.Control)
	defer cancel()
	if err := m.opts.Controller.Start(cctx, app); err != nil {
		return result, fmt.Errorf("the volume was restored but the app could not be started: %w", err)
	}
	log.Info("volume restored", "volume", vol.Short, "trash", trash)
	return result, nil
}

func (m *Manager) control(ctx context.Context, fn func(context.Context, App) error, app App) error {
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeouts.Control)
	defer cancel()
	return fn(ctx, app)
}

func sameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

// moveContents moves every entry of src into dst. The directory src itself
// stays: it is the volume's mountpoint, and its owner and mode are what the
// container expects. See moveEntry for how one entry moves.
func moveContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := moveEntry(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// putBack undoes a restore attempt and removes the then empty trash
// directory. With clear, whatever is in the volume now is removed first: it
// can only be the output of a partial restore, because the restore starts
// after every entry was moved away. Without clear the move itself stopped
// half way, and what is still in the volume is original data that stays.
func putBack(trash, mountpoint string, clear bool) error {
	if clear {
		entries, err := os.ReadDir(mountpoint)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(mountpoint, e.Name())); err != nil {
				return err
			}
		}
	}
	if err := moveContents(trash, mountpoint); err != nil {
		return err
	}
	return os.Remove(trash)
}

// CleanTrash removes the trash directories of restores older than
// Options.TrashRetention and returns their paths. Their age is read from
// the timestamp in the name; anything else in the directory is left alone.
// A scheduled run of the Scheduler calls it.
func (m *Manager) CleanTrash(ctx context.Context) ([]string, error) {
	if m.opts.TrashDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(m.opts.TrashDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the trash directory: %w", err)
	}
	cutoff := m.opts.Clock.Now().Add(-m.opts.TrashRetention)
	var removed []string
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		created, ok := trashTime(e.Name())
		if !ok || !e.IsDir() || !created.Before(cutoff) {
			continue
		}
		path := filepath.Join(m.opts.TrashDir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, path)
	}
	return removed, errors.Join(errs...)
}

// trashTime reads the timestamp at the end of <slug>-<volume>-<timestamp>.
func trashTime(name string) (time.Time, bool) {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(timestampLayout, name[i+1:])
	return t, err == nil
}
