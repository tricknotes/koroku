package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	plainExt = ".log"
	gzExt    = ".log.gz"
	// tmpExt and doneExt only exist while a day file is being compressed;
	// see compressDay.
	tmpExt  = ".log.gz.tmp"
	doneExt = ".log.done"
)

// dayFiles describes the files of one service for one day. The content of
// the day is the gzip file followed by the plain file: normally only one of
// them exists, but lines that arrive for a day that was already compressed
// start a new plain file, which the next maintenance pass merges.
type dayFiles struct {
	dir   string
	date  string // YYYY-MM-DD
	gz    bool
	plain bool
}

func (d dayFiles) plainPath() string { return filepath.Join(d.dir, d.date+plainExt) }
func (d dayFiles) gzPath() string    { return filepath.Join(d.dir, d.date+gzExt) }

func validDate(s string) bool {
	_, err := time.Parse(dateLayout, s)
	return err == nil
}

// listDays returns the days that have files in dir, oldest first. A missing
// directory has no days.
func listDays(dir string) ([]dayFiles, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list log files: %w", err)
	}
	byDate := map[string]*dayFiles{}
	for _, entry := range entries {
		name := entry.Name()
		var date string
		var isGz bool
		switch {
		case strings.HasSuffix(name, gzExt):
			date, isGz = strings.TrimSuffix(name, gzExt), true
		case strings.HasSuffix(name, plainExt):
			date = strings.TrimSuffix(name, plainExt)
		default:
			continue
		}
		if !validDate(date) || entry.IsDir() {
			continue
		}
		d := byDate[date]
		if d == nil {
			d = &dayFiles{dir: dir, date: date}
			byDate[date] = d
		}
		if isGz {
			d.gz = true
		} else {
			d.plain = true
		}
	}
	days := make([]dayFiles, 0, len(byDate))
	for _, d := range byDate {
		days = append(days, *d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].date < days[j].date })
	return days, nil
}

// listNames returns the subdirectories of dir whose name passes check,
// sorted. A missing directory has none.
func listNames(dir string, check func(string) error) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list log directories: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && check(entry.Name()) == nil {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// dayStream reads the content of one day: the decompressed gzip file followed
// by the plain file. It holds the files open, so it keeps reading the same
// bytes even when the day is compressed meanwhile.
type dayStream struct {
	io.Reader
	files []*os.File
	// size is the length of the stream in bytes.
	size int64
}

func (s *dayStream) Close() {
	for _, f := range s.files {
		_ = f.Close()
	}
}

// openDay opens the content of a day. Files that vanished since the
// directory was listed are skipped: the day was compressed or expired.
func openDay(d dayFiles) (*dayStream, error) {
	for attempt := 0; ; attempt++ {
		s, err := openDayOnce(d)
		if err != nil {
			return nil, err
		}
		// Finding nothing means the day was compressed between the two
		// opens (or expired). Look once more.
		if len(s.files) > 0 || attempt > 0 {
			return s, nil
		}
	}
}

// openDayOnce opens both files of the day whatever the listing said: the
// listing may be older than a compression or a line that arrived late.
func openDayOnce(d dayFiles) (*dayStream, error) {
	s := &dayStream{}
	var readers []io.Reader
	zr, err := openGzipOnly(d)
	if err != nil {
		return nil, err
	}
	if zr != nil {
		s.files = append(s.files, zr.file)
		size, err := gzipContentSize(zr.file)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.size += size
		readers = append(readers, zr)
	}
	f, err := os.Open(d.plainPath())
	switch {
	case err == nil:
		s.files = append(s.files, f)
		info, err := f.Stat()
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("stat log file: %w", err)
		}
		s.size += info.Size()
		readers = append(readers, f)
	case !errors.Is(err, fs.ErrNotExist):
		s.Close()
		return nil, fmt.Errorf("open log file: %w", err)
	}
	s.Reader = io.MultiReader(readers...)
	return s, nil
}

// gzipFile reads the decompressed content of a day's gzip file.
type gzipFile struct {
	*gzip.Reader
	file *os.File
}

func (g *gzipFile) Close() { _ = g.file.Close() }

// openGzipOnly opens the gzip file of d. It returns nil when there is none.
func openGzipOnly(d dayFiles) (*gzipFile, error) {
	f, err := os.Open(d.gzPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open log file: %w", err)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read %s: %w", d.gzPath(), err)
	}
	return &gzipFile{Reader: zr, file: f}, nil
}

// gzipContentSize returns the uncompressed size recorded in the trailer of a
// gzip file. The files are written by compressDay as a single member; the
// field wraps at 4 GiB, which a day of one service is assumed not to reach.
func gzipContentSize(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat log file: %w", err)
	}
	if info.Size() < 18 {
		return 0, nil
	}
	var trailer [4]byte
	if _, err := f.ReadAt(trailer[:], info.Size()-4); err != nil {
		return 0, fmt.Errorf("read %s: %w", f.Name(), err)
	}
	return int64(binary.LittleEndian.Uint32(trailer[:])), nil
}

// errStop ends a scan early without being an error.
var errStop = errors.New("stop")

// scanEntries calls fn for every complete entry read from r, in order. The
// text is only valid during the call. An unterminated last line is ignored:
// it is still being written. fn may return errStop to end the scan.
func scanEntries(ctx context.Context, r io.Reader, fn func(ts time.Time, text []byte) error) error {
	var ferr error
	sp := splitter{max: readLineCap, emit: func(line []byte, _ bool) {
		if ferr != nil {
			return
		}
		if ts, text, ok := parseEntry(line); ok {
			ferr = fn(ts, text)
		}
	}}
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		_, _ = sp.Write(buf[:n])
		if ferr != nil {
			if errors.Is(ferr, errStop) {
				return nil
			}
			return ferr
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read log file: %w", err)
		}
	}
}

// scanEntriesReverse calls fn for the complete entries of the plain file at
// path, last entry first, until fn returns false. A missing file has no
// entries.
func scanEntriesReverse(path string, fn func(ts time.Time, text []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat log file: %w", err)
	}

	const block = 64 << 10
	emit := func(line []byte) bool {
		ts, text, ok := parseEntry(line)
		return !ok || fn(ts, text)
	}
	// carry is the beginning, found so far, of the line that continues into
	// the blocks already read. skipLast says that the last segment of the
	// next block is not a line: it is the unterminated end of the file, or
	// the rest of a line that was too long to keep.
	var carry []byte
	skipLast := true
	pos := info.Size()
	for pos > 0 {
		n := int64(block)
		if n > pos {
			n = pos
		}
		pos -= n
		data := make([]byte, n, int(n)+len(carry))
		if _, err := f.ReadAt(data, pos); err != nil {
			return fmt.Errorf("read log file: %w", err)
		}
		data = append(data, carry...)
		segments := bytes.Split(data, []byte{'\n'})
		for i := len(segments) - 1; i >= 1; i-- {
			if i == len(segments)-1 && skipLast {
				continue
			}
			if !emit(segments[i]) {
				return nil
			}
		}
		if len(segments) > 1 {
			skipLast = false
		}
		carry = segments[0]
		if len(carry) > readLineCap {
			carry, skipLast = nil, true
		}
	}
	if !skipLast {
		emit(carry)
	}
	return nil
}

// appState is the write side of one app. Its mutex also serializes the
// collector with the steps of maintenance and archiving that must see the
// app's files at rest.
type appState struct {
	mu      sync.Mutex
	writers map[string]*dayWriter // by service
}

// dayWriter appends to the plain file of one service for one day.
type dayWriter struct {
	date  string
	file  *os.File
	buf   *bufio.Writer
	dirty bool
}

func (c *Collector) state(slug string) *appState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.states[slug]
	if st == nil {
		st = &appState{writers: map[string]*dayWriter{}}
		c.states[slug] = st
	}
	return st
}

// write appends lines to the day files of slug, in order, and then hands
// them to the live subscribers. The lines are on disk when it returns.
func (c *Collector) write(slug string, lines []Line) error {
	if len(lines) == 0 {
		return nil
	}
	st := c.state(slug)
	st.mu.Lock()
	err := c.writeLocked(st, slug, lines)
	st.mu.Unlock()
	if err != nil {
		return err
	}
	c.publish(slug, lines)
	return nil
}

func (c *Collector) writeLocked(st *appState, slug string, lines []Line) error {
	var scratch []byte
	var firstErr error
	for _, line := range lines {
		date := c.day(line.Time)
		w := st.writers[line.Service]
		if w == nil || w.date != date {
			if w != nil {
				delete(st.writers, line.Service)
				if err := w.close(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			var err error
			w, err = openDayWriter(c.serviceDir(slug, line.Service), date)
			if err != nil {
				return err
			}
			st.writers[line.Service] = w
		}
		scratch = appendEntry(scratch[:0], line.Time, line.Text)
		if _, err := w.buf.Write(scratch); err != nil {
			return fmt.Errorf("write log file: %w", err)
		}
		w.dirty = true
	}
	for _, w := range st.writers {
		if !w.dirty {
			continue
		}
		w.dirty = false
		if err := w.buf.Flush(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("write log file: %w", err)
		}
	}
	return firstErr
}

func openDayWriter(dir, date string) (*dayWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, date+plainExt), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	w := &dayWriter{date: date, file: f, buf: bufio.NewWriterSize(f, 64<<10)}
	// A process that died in the middle of a write leaves half a line.
	// End it, so the next entry starts on a line of its own.
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err == nil && last[0] != '\n' {
			_ = w.buf.WriteByte('\n')
		}
	}
	return w, nil
}

func (w *dayWriter) close() error {
	flushErr := w.buf.Flush()
	closeErr := w.file.Close()
	if flushErr != nil {
		return fmt.Errorf("write log file: %w", flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close log file: %w", closeErr)
	}
	return nil
}

// closeWriters closes every open file of slug.
func (c *Collector) closeWriters(slug string) {
	st := c.state(slug)
	st.mu.Lock()
	defer st.mu.Unlock()
	for service, w := range st.writers {
		_ = w.close()
		delete(st.writers, service)
	}
}

// closeWriterLocked closes the writer of service when it has date open. The
// caller holds st.mu.
func (st *appState) closeWriterLocked(service, date string) {
	if w := st.writers[service]; w != nil && w.date == date {
		_ = w.close()
		delete(st.writers, service)
	}
}
