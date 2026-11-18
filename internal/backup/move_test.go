package backup

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// crossDevice makes every rename fail as it does between two bind mounts.
func crossDevice(t *testing.T) {
	t.Helper()
	orig := rename
	rename = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { rename = orig })
}

func writeFile(t *testing.T, path, content string, mode os.FileMode, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestMoveContentsAcrossMounts: when rename fails with EXDEV, the contents
// are copied with their modes, times and links, and the originals removed.
func TestMoveContentsAcrossMounts(t *testing.T) {
	crossDevice(t)
	src, dst := t.TempDir(), t.TempDir()
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	writeFile(t, filepath.Join(src, "PG_VERSION"), "17\n", 0o600, old)
	writeFile(t, filepath.Join(src, "base", "1", "1259"), "table data", 0o640, old)
	writeFile(t, filepath.Join(src, "bin", "run"), "#!/bin/sh\n", 0o755, old)
	if err := os.Symlink("base/1", filepath.Join(src, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "base"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(src, "base"), old, old); err != nil {
		t.Fatal(err)
	}

	if err := moveContents(src, dst); err != nil {
		t.Fatalf("moveContents: %v", err)
	}

	if left, _ := os.ReadDir(src); len(left) != 0 {
		t.Errorf("src still holds %v", left)
	}
	for path, want := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		"PG_VERSION":  {"17\n", 0o600},
		"base/1/1259": {"table data", 0o640},
		"bin/run":     {"#!/bin/sh\n", 0o755},
	} {
		full := filepath.Join(dst, path)
		got, err := os.ReadFile(full)
		if err != nil || string(got) != want.content {
			t.Errorf("%s = %q, %v", path, got, err)
			continue
		}
		info, _ := os.Stat(full)
		if info.Mode().Perm() != want.mode {
			t.Errorf("%s mode = %v, want %v", path, info.Mode().Perm(), want.mode)
		}
		if !info.ModTime().Equal(old) {
			t.Errorf("%s mtime = %v, want %v", path, info.ModTime(), old)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "base")); err != nil || info.Mode().Perm() != 0o700 || !info.ModTime().Equal(old) {
		t.Errorf("base: %v, %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "current")); err != nil || target != "base/1" {
		t.Errorf("symlink = %q, %v", target, err)
	}
}

// TestMoveContentsAcrossMountsKeepsSourceOnFailure: an entry that cannot be
// copied leaves the original in place and no partial copy behind, so the
// restore can put everything back.
func TestMoveContentsAcrossMountsKeepsSourceOnFailure(t *testing.T) {
	crossDevice(t)
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "data", "a"), "a", 0o600, time.Now())
	if err := syscall.Mkfifo(filepath.Join(src, "data", "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	if err := moveContents(src, dst); err == nil {
		t.Fatal("moveContents succeeded with a named pipe in the tree")
	}
	if _, err := os.Stat(filepath.Join(src, "data", "a")); err != nil {
		t.Errorf("the original was touched: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a partial copy was left behind: %v", err)
	}
}

// TestMoveEntryReportsOtherRenameErrors: only EXDEV switches to copying.
func TestMoveEntryReportsOtherRenameErrors(t *testing.T) {
	src := t.TempDir()
	err := moveEntry(filepath.Join(src, "missing"), filepath.Join(src, "dst"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want not exist", err)
	}
	if _, err := os.Lstat(filepath.Join(src, "dst")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dst was created: %v", err)
	}
}
