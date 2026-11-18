package logs

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RunMaintenance calls Maintain now and then every Options.MaintainInterval
// until ctx is done.
func (c *Collector) RunMaintenance(ctx context.Context) {
	for {
		if err := c.Maintain(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("log maintenance failed", "error", err)
		}
		if !sleep(ctx, c.maintainEvery) {
			return
		}
	}
}

// Maintain compresses the files of past days and deletes files older than
// the retention period. The day that is current in Options.Location is never
// touched. It keeps going after a failure and returns all of them.
func (c *Collector) Maintain(ctx context.Context) error {
	c.maintainMu.Lock()
	defer c.maintainMu.Unlock()

	now := c.clk.Now().In(c.loc)
	today := now.Format(dateLayout)
	oldest := now.AddDate(0, 0, -c.retentionDays).Format(dateLayout)

	slugs, err := listNames(c.dir, checkSlug)
	if err != nil {
		return err
	}
	var errs []error
	for _, slug := range slugs {
		services, err := listNames(c.appDir(slug), checkService)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, service := range services {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			if err := c.maintainService(ctx, slug, service, today, oldest); err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: %w", slug, service, err))
			}
		}
		c.removeAppDirIfUnused(slug)
	}
	return errors.Join(errs...)
}

// removeAppDirIfUnused removes the directory of an app once all its log
// files expired, together with a resume marker left behind in it.
func (c *Collector) removeAppDirIfUnused(slug string) {
	st := c.state(slug)
	st.mu.Lock()
	defer st.mu.Unlock()
	dir := c.appDir(slug)
	services, err := listNames(dir, func(string) error { return nil })
	if err != nil || len(services) > 0 {
		return
	}
	// A marker without files says nothing: with no files, collection
	// starts from the beginning anyway.
	_ = os.Remove(filepath.Join(dir, resumeMarkerName))
	_ = os.Remove(filepath.Join(dir, resumeMarkerName+".tmp"))
	_ = os.Remove(dir)
}

func (c *Collector) maintainService(ctx context.Context, slug, service, today, oldest string) error {
	dir := c.serviceDir(slug, service)
	days, err := listDays(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range days {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		switch {
		case d.date < oldest:
			if err := c.expireDay(slug, service, d); err != nil {
				errs = append(errs, err)
			}
		case d.date < today && d.plain:
			if err := c.compressDay(slug, service, d); err != nil {
				errs = append(errs, err)
			}
		}
	}
	c.removeIfEmpty(slug, dir)
	return errors.Join(errs...)
}

func (c *Collector) expireDay(slug, service string, d dayFiles) error {
	st := c.state(slug)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.closeWriterLocked(service, d.date)
	var errs []error
	for _, path := range []string{d.plainPath(), d.gzPath()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("delete expired log file: %w", err))
		}
	}
	return errors.Join(errs...)
}

// removeIfEmpty removes dir when nothing is left in it. It holds the app's
// lock so that a writer is not about to create a file there.
func (c *Collector) removeIfEmpty(slug, dir string) {
	st := c.state(slug)
	st.mu.Lock()
	defer st.mu.Unlock()
	_ = os.Remove(dir) // fails when not empty, which is the common case
}

// compressDay turns the plain file of a past day into the gzip file. When a
// gzip file exists already (lines arrived late for a compressed day), the
// plain file is appended to its content.
//
// The switch is made of renames, so a reader sees the old or the new state
// and a crash at any point can be completed by recoverCompressions:
//
//  1. write <day>.log.gz.tmp (old gzip content followed by the plain file)
//  2. rename <day>.log to <day>.log.done    <- from here the tmp file wins
//  3. rename <day>.log.gz.tmp to <day>.log.gz
//  4. remove <day>.log.done
//
// Step 1 runs without the app's lock, so collection is not held up by the
// compression. Steps 2 to 4 hold it, after checking that the plain file did
// not grow meanwhile.
func (c *Collector) compressDay(slug, service string, d dayFiles) error {
	st := c.state(slug)
	// Flush and release the writer if it still has this day open.
	st.mu.Lock()
	st.closeWriterLocked(service, d.date)
	st.mu.Unlock()

	tmp := filepath.Join(d.dir, d.date+tmpExt)
	done := filepath.Join(d.dir, d.date+doneExt)
	plainSize, err := writeCompressed(tmp, d)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.closeWriterLocked(service, d.date)
	info, err := os.Stat(d.plainPath())
	if err != nil || info.Size() != plainSize {
		// Lines arrived while compressing. The next pass tries again.
		_ = os.Remove(tmp)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat log file: %w", err)
		}
		return nil
	}
	if err := os.Rename(d.plainPath(), done); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compress log file: %w", err)
	}
	if err := os.Rename(tmp, d.gzPath()); err != nil {
		return fmt.Errorf("compress log file: %w", err)
	}
	if err := os.Remove(done); err != nil {
		return fmt.Errorf("compress log file: %w", err)
	}
	return nil
}

// writeCompressed writes the content of day d, gzip-compressed, to tmp and
// returns how many bytes of the plain file went into it.
func writeCompressed(tmp string, d dayFiles) (plainSize int64, err error) {
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("compress log file: %w", err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("compress log file: %w", cerr)
		}
	}()
	zw := gzip.NewWriter(out)

	old, err := openGzipOnly(d)
	if err != nil {
		return 0, err
	}
	if old != nil {
		_, err := io.Copy(zw, old)
		old.Close()
		if err != nil {
			return 0, fmt.Errorf("compress log file: %w", err)
		}
	}
	plain, err := os.Open(d.plainPath())
	if err != nil {
		return 0, fmt.Errorf("compress log file: %w", err)
	}
	plainSize, err = io.Copy(zw, plain)
	_ = plain.Close()
	if err != nil {
		return 0, fmt.Errorf("compress log file: %w", err)
	}
	if err := zw.Close(); err != nil {
		return 0, fmt.Errorf("compress log file: %w", err)
	}
	if err := out.Sync(); err != nil {
		return 0, fmt.Errorf("compress log file: %w", err)
	}
	return plainSize, nil
}

// recoverCompressions completes or undoes the compressions a previous
// process was interrupted in. See compressDay for the steps.
func (c *Collector) recoverCompressions() error {
	slugs, err := listNames(c.dir, checkSlug)
	if err != nil {
		return err
	}
	for _, slug := range slugs {
		services, err := listNames(c.appDir(slug), checkService)
		if err != nil {
			return err
		}
		for _, service := range services {
			if err := recoverDir(c.serviceDir(slug, service)); err != nil {
				return err
			}
		}
	}
	return nil
}

func recoverDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list log files: %w", err)
	}
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	for name := range names {
		if !strings.HasSuffix(name, doneExt) {
			continue
		}
		// Interrupted after step 2: the tmp file is complete and holds
		// everything the renamed plain file held.
		date := strings.TrimSuffix(name, doneExt)
		if names[date+tmpExt] {
			if err := os.Rename(filepath.Join(dir, date+tmpExt), filepath.Join(dir, date+gzExt)); err != nil {
				return fmt.Errorf("recover log file: %w", err)
			}
			delete(names, date+tmpExt)
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("recover log file: %w", err)
		}
	}
	for name := range names {
		// Interrupted during step 1: the plain file is still the truth.
		if strings.HasSuffix(name, tmpExt) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("recover log file: %w", err)
			}
		}
	}
	return nil
}
