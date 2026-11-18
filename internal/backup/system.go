package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Layout of a system snapshot, and so of the directory RestoreSystem fills.
const (
	// SystemDatabaseFile is the database copy at the root of the snapshot.
	SystemDatabaseFile = "koroku.db"
	// SystemConfigDir holds the contents of the configuration directory.
	SystemConfigDir = "config"
)

// backupSystem stages the database copy and the configuration directory and
// stores them as one snapshot. The staging directory holds secrets and is
// removed again whatever happens.
func (m *Manager) backupSystem(ctx context.Context) TargetResult {
	res := TargetResult{Kind: KindSystem}
	stage := filepath.Join(m.opts.WorkDir, "system")
	// The path is the same for every run so that restic finds the previous
	// snapshot of it and reads unchanged files quickly.
	if err := os.RemoveAll(stage); err != nil {
		res.Err = fmt.Errorf("clear the staging directory: %w", err)
		return res
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		res.Err = fmt.Errorf("create the staging directory: %w", err)
		return res
	}
	defer os.RemoveAll(stage)

	dbCtx, cancel := context.WithTimeout(ctx, m.opts.Timeouts.Command)
	err := m.opts.System.SnapshotDB(dbCtx, filepath.Join(stage, SystemDatabaseFile))
	cancel()
	if err != nil {
		res.Err = fmt.Errorf("copy the database: %w", err)
		return res
	}
	if info, err := os.Stat(filepath.Join(stage, SystemDatabaseFile)); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		res.Err = errors.New("copy the database: no database file was written")
		return res
	}
	if dir := m.opts.System.ConfigDir; dir != "" {
		if err := copyTree(dir, filepath.Join(stage, SystemConfigDir)); err != nil {
			res.Err = fmt.Errorf("copy the configuration directory: %w", err)
			return res
		}
	}

	report, err := m.backupDir(ctx, stage, systemTags())
	if err != nil {
		// Every staged file is ours; an unreadable one is a failure.
		res.Err = err
		if text := report.problemText(); text != "" {
			res.Err = fmt.Errorf("%w: %s", err, text)
		}
		if id := report.snapshotID(); id != "" {
			if derr := m.discardSnapshot(ctx, id); derr != nil {
				res.Err = fmt.Errorf("%w; the incomplete snapshot %s could not be removed: %v", res.Err, shortID(id), derr)
			}
		}
		return res
	}
	res.SnapshotID = report.snapshotID()
	return res
}

// copyTree copies the regular files, directories and symbolic links below
// src to dst, keeping permissions and modification times. Ownership is not
// kept: the copy belongs to the user koroku runs as.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info)
		default:
			return nil // sockets and the like are not configuration
		}
	})
}

func copyFile(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// SystemRestore tells where RestoreSystem put the pieces of a system
// snapshot. Nothing outside Dir was touched; moving the pieces into place is
// the caller's decision.
type SystemRestore struct {
	// SnapshotID and Time identify the snapshot that was restored.
	SnapshotID string
	Time       time.Time
	// Dir is the staging directory the snapshot was restored into.
	Dir string
	// DatabasePath is the database copy (Dir/koroku.db).
	DatabasePath string
	// ConfigDir holds the contents of the configuration directory
	// (Dir/config). It is empty when the snapshot has none.
	ConfigDir string
}

// RestoreSystem restores a system snapshot into destDir, which must be empty
// or not exist yet; it is created with mode 0700 because it receives
// secrets. An empty snapshotID picks the newest system snapshot.
//
// It never creates a repository: on a new host the error matches
// ErrNoRepository when the configured location holds none, and
// ErrSnapshotNotFound when it holds no (such) system snapshot.
func (m *Manager) RestoreSystem(ctx context.Context, snapshotID, destDir string) (SystemRestore, error) {
	if !filepath.IsAbs(destDir) {
		return SystemRestore{}, errors.New("backup: the destination of a system restore must be an absolute path")
	}
	if err := m.ensureRepository(ctx, false); err != nil {
		return SystemRestore{}, err
	}
	recs, err := m.systemRecords(ctx)
	if err != nil {
		return SystemRestore{}, err
	}
	var rec record
	switch {
	case snapshotID != "":
		if rec, err = matchRecord(recs, snapshotID); err != nil {
			return SystemRestore{}, err
		}
	case len(recs) == 0:
		return SystemRestore{}, fmt.Errorf("%w: the repository holds no system snapshot", ErrSnapshotNotFound)
	default:
		rec = recs[0]
	}

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return SystemRestore{}, fmt.Errorf("create %s: %w", destDir, err)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return SystemRestore{}, fmt.Errorf("read %s: %w", destDir, err)
	}
	if len(entries) > 0 {
		return SystemRestore{}, fmt.Errorf("backup: %s is not empty", destDir)
	}

	err = m.restic(ctx, resticCall{
		args:    []string{"restore", "--retry-lock", m.lockWait(), "--target", destDir, rec.ID},
		timeout: m.opts.Timeouts.Restore,
	})
	if err != nil {
		return SystemRestore{}, fmt.Errorf("restore system snapshot %s: %w", shortID(rec.ID), err)
	}

	out := SystemRestore{
		SnapshotID:   shortID(rec.ID),
		Time:         rec.Time.UTC(),
		Dir:          destDir,
		DatabasePath: filepath.Join(destDir, SystemDatabaseFile),
	}
	if info, err := os.Stat(out.DatabasePath); err != nil || !info.Mode().IsRegular() {
		return SystemRestore{}, fmt.Errorf("system snapshot %s holds no %s", out.SnapshotID, SystemDatabaseFile)
	}
	if info, err := os.Stat(filepath.Join(destDir, SystemConfigDir)); err == nil && info.IsDir() {
		out.ConfigDir = filepath.Join(destDir, SystemConfigDir)
	}
	return out, nil
}
