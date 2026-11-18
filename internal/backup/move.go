package backup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// rename is os.Rename; tests replace it to simulate a cross-device move.
var rename = os.Rename

// moveEntry moves the file or directory tree src to dst, which must not
// exist. It renames when it can. The trash directory and the volume are on
// one filesystem (Restore checks that), but they may still be reached
// through two different mounts of it, as in the local development setup,
// where koroku runs in a container with bind mounts of its data directory
// and of Docker's volume directory. Linux refuses to rename across mounts
// (EXDEV) even then, so the entry is copied with its owner, mode and times
// and the original is removed afterwards. Hard links within the tree become
// separate files and extended attributes are not copied: the trash only
// keeps the previous contents for a manual rescue.
//
// When the copy fails, the partial copy is removed and src is left as it
// was.
func moveEntry(src, dst string) error {
	err := rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyEntry(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("copy %s across mounts: %w", src, err)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("remove %s after copying it across mounts: %w", src, err)
	}
	return nil
}

// copyEntry copies src to dst like `cp -a` for directories, regular files
// and symbolic links. Other file types (sockets, devices, named pipes) are
// refused.
func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		return chownLike(dst, info, true)
	case mode.IsDir():
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyEntry(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
	case mode.IsRegular():
		if err := copyRegular(src, dst); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s is a %s, which cannot be copied", src, mode.Type())
	}
	// Owner first: changing it clears the setuid and setgid bits.
	if err := chownLike(dst, info, false); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	// Last, so that creating the children did not change a directory's time.
	return os.Chtimes(dst, accessTime(info), info.ModTime())
}

func copyRegular(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// chownLike gives dst the owner of info. It needs root (or CAP_CHOWN) unless
// the owner is already the caller.
func chownLike(dst string, info fs.FileInfo, link bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if link {
		return os.Lchown(dst, int(st.Uid), int(st.Gid))
	}
	return os.Chown(dst, int(st.Uid), int(st.Gid))
}

func accessTime(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return info.ModTime()
}
