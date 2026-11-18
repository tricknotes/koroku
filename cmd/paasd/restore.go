package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/runner"
)

// systemRestorer is the part of the backup manager restore-system uses.
type systemRestorer interface {
	RestoreSystem(ctx context.Context, snapshotID, destDir string) (backup.SystemRestore, error)
}

// restoreSystem implements `paasd restore-system`: on a freshly provisioned
// host it fetches the newest system snapshot (the database and the
// configuration directory) and puts the files in place (ARCHITECTURE 10.2).
//
// The host needs a config.toml with [storage] and the restic password file
// before this can run; everything else comes out of the backup.
func restoreSystem(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("restore-system", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.DefaultPath, "path to config.toml (needs [storage] and backup.restic_password_file)")
	snapshot := flags.String("snapshot", "", "system snapshot ID to restore (default: the newest)")
	force := flags.Bool("force", false, "replace existing files; each one is first renamed with a .before-restore suffix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if ok, reason := backupsConfigured(cfg); !ok {
		return fmt.Errorf("cannot reach the backups: %s", reason)
	}
	manager, err := backup.New(backup.Options{Repository: backupRepository(cfg), Runner: runner.Exec{}})
	if err != nil {
		return err
	}
	return runSystemRestore(ctx, manager, cfg, filepath.Dir(*configPath), *snapshot, *force, time.Now(), stdout)
}

func runSystemRestore(ctx context.Context, restorer systemRestorer, cfg config.Config, configDir, snapshot string,
	force bool, now time.Time, stdout io.Writer) error {
	stamp := now.UTC().Format("20060102-150405")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// Refuse early: nothing is downloaded when the database would be kept anyway.
	dbPath := cfg.DBPath()
	if _, err := os.Stat(dbPath); err == nil && !force {
		return fmt.Errorf("%s already exists; this command is for a new host (use --force to replace it, the old file is kept with a .before-restore suffix)", dbPath)
	}

	staging := filepath.Join(cfg.DataDir, "restore-system-"+stamp)
	fmt.Fprintf(stdout, "Restoring the system snapshot into %s ...\n", staging)
	restored, err := restorer.RestoreSystem(ctx, snapshot, staging)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Snapshot %s taken at %s.\n", restored.SnapshotID, restored.Time.UTC().Format(time.RFC3339))

	placed, skipped, err := placeFile(restored.DatabasePath, dbPath, 0o600, force, stamp)
	if err != nil {
		return fmt.Errorf("install the database: %w", err)
	}
	report := newRestoreReport()
	report.add(dbPath, placed, skipped)

	// Configuration files: the file this command was started with stays as
	// it is unless --force is given, like every other existing file.
	entries, err := listFiles(restored.ConfigDir)
	if err != nil {
		return fmt.Errorf("read the restored configuration: %w", err)
	}
	for _, rel := range entries {
		dest := filepath.Join(configDir, rel)
		placed, skipped, err := placeFile(filepath.Join(restored.ConfigDir, rel), dest, 0o600, force, stamp)
		if err != nil {
			return fmt.Errorf("install %s: %w", dest, err)
		}
		report.add(dest, placed, skipped)
	}
	report.print(stdout)

	if err := os.RemoveAll(staging); err != nil {
		fmt.Fprintf(stdout, "Note: the staging directory %s could not be removed: %v\n", staging, err)
	}
	fmt.Fprint(stdout, `
Next steps:
  1. Compare any kept file with its .restored-<time> copy (config.toml usually needs the
     [github] section and other settings from the old host), then start paasd: systemctl start paasd
  2. Deploy each app again from the console, then restore its volumes from the app's backup tab.
  3. Point the DNS records at this host's address.
`)
	return nil
}

type restoreReport struct {
	placed, skipped []string
}

func newRestoreReport() *restoreReport { return &restoreReport{} }

func (r *restoreReport) add(path string, placed, skipped bool) {
	switch {
	case placed:
		r.placed = append(r.placed, path)
	case skipped:
		r.skipped = append(r.skipped, path)
	}
}

func (r *restoreReport) print(w io.Writer) {
	sort.Strings(r.placed)
	sort.Strings(r.skipped)
	fmt.Fprintf(w, "Installed %d file(s):\n", len(r.placed))
	for _, p := range r.placed {
		fmt.Fprintf(w, "  %s\n", p)
	}
	if len(r.skipped) > 0 {
		fmt.Fprintf(w, "Kept %d existing file(s); the restored version of each is next to it as <name>.restored-<time>:\n", len(r.skipped))
		for _, p := range r.skipped {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}
}

// listFiles returns the regular files below dir, relative to it.
func listFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	sort.Strings(files)
	return files, err
}

// placeFile copies src to dest with the given mode. Without force an
// existing dest is kept and the restored version is written next to it; with
// force the existing file is renamed first. Either way nothing is lost.
func placeFile(src, dest string, mode os.FileMode, force bool, stamp string) (placed, skipped bool, err error) {
	if _, statErr := os.Lstat(dest); statErr == nil {
		if !force {
			// Keep the existing file, and put the restored version next to
			// it so the two can be compared after the staging directory is gone.
			if _, _, err := placeFile(src, dest+".restored-"+stamp, mode, true, stamp); err != nil {
				return false, false, err
			}
			return false, true, nil
		}
		if err := os.Rename(dest, dest+".before-restore-"+stamp); err != nil {
			return false, false, err
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return false, false, statErr
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return false, false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = in.Close() }()
	// Secrets live here (keys, config with credentials): private by default.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return false, false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return false, false, err
	}
	if err := out.Close(); err != nil {
		return false, false, err
	}
	return true, false, nil
}
