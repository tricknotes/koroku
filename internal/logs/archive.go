package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/objstore"
)

const (
	archiveStateName = ".archive-state.json"
	archiveMIME      = "application/gzip"

	// gridMinute is the minute past local midnight at which the archive
	// grid starts.
	gridMinute = 10

	// maxDeployLogBytes bounds one deploy log read into memory.
	maxDeployLogBytes = 64 << 20
)

// ArchiverOptions configure an Archiver.
type ArchiverOptions struct {
	// Collector owns the local files to archive. Its Dir, Clock, Location
	// and Logger are used. Required.
	Collector *Collector
	// Store is the bucket. Required.
	Store objstore.Store
	// IntervalHours is the length of an archive window. Default 4, at
	// least 1, at most 24.
	IntervalHours int
	// AppsDir is the directory that holds <slug>/deploys/<id>.log, as
	// written by the deploy engine. Empty disables deploy log archiving.
	AppsDir string
	// DeployQuiet is how long a deploy log must be unmodified before it
	// counts as finished. Default 30m.
	DeployQuiet time.Duration
	// MaxObjectBytes bounds the uncompressed size of one object, and with
	// it the memory of one upload. What does not fit waits for the next
	// window. Default 256 MiB.
	MaxObjectBytes int64
	// UploadTimeout bounds one request to the store. Default 5m.
	UploadTimeout time.Duration
	// PollInterval is how often Run looks at the clock. Default 30s.
	PollInterval time.Duration
	// RetryInterval is how long Run waits before repeating a run that
	// failed. Default 10m.
	RetryInterval time.Duration
}

// Archiver uploads the collected logs to object storage, one object per
// service and window, and remembers how far it got.
type Archiver struct {
	c             *Collector
	store         objstore.Store
	interval      time.Duration
	appsDir       string
	deployQuiet   time.Duration
	maxObject     int64
	uploadTimeout time.Duration
	pollEvery     time.Duration
	retryEvery    time.Duration

	mu    sync.Mutex
	state *archiveState

	// statusMu guards status. It is separate from mu so that Status does
	// not wait for an upload.
	statusMu sync.Mutex
	status   ArchiveStatus
}

// maxStatusError bounds ArchiveStatus.LastError.
const maxStatusError = 1000

// ArchiveStatus says how the runs of Archiver.Run are going.
type ArchiveStatus struct {
	// LastAttempt is when the last run ended. It is zero until the first
	// run of this process.
	LastAttempt time.Time
	// LastSuccess is when a run last ended without an error. It is kept
	// across restarts, and zero when no run ever succeeded.
	LastSuccess time.Time
	// LastError describes what failed in the last run, and is empty when
	// that run succeeded. It names keys and files, never log content.
	LastError string
}

// NewArchiver returns an Archiver for the files of opts.Collector. It reads
// the state a previous process left behind.
func NewArchiver(opts ArchiverOptions) (*Archiver, error) {
	if opts.Collector == nil || opts.Store == nil {
		return nil, errors.New("logs: the archiver needs a Collector and a Store")
	}
	hours := orDefault(opts.IntervalHours, 4)
	if hours < 1 || hours > 24 {
		return nil, fmt.Errorf("logs: archive interval of %d hours is out of range (1 to 24)", hours)
	}
	a := &Archiver{
		c:             opts.Collector,
		store:         opts.Store,
		interval:      time.Duration(hours) * time.Hour,
		appsDir:       opts.AppsDir,
		deployQuiet:   orDefault(opts.DeployQuiet, 30*time.Minute),
		maxObject:     orDefault(opts.MaxObjectBytes, 256<<20),
		uploadTimeout: orDefault(opts.UploadTimeout, 5*time.Minute),
		pollEvery:     orDefault(opts.PollInterval, 30*time.Second),
		retryEvery:    orDefault(opts.RetryInterval, 10*time.Minute),
	}
	if err := a.loadState(); err != nil {
		return nil, err
	}
	a.status.LastSuccess = a.state.LastSuccess
	return a, nil
}

// Status reports the outcome of the runs made by Run. It does not block
// while a run is in progress.
func (a *Archiver) Status() ArchiveStatus {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	return a.status
}

// record notes the outcome of a run for Status and keeps the time of a
// success in the state file.
func (a *Archiver) record(runErr error) {
	now := a.c.clk.Now().UTC()
	a.statusMu.Lock()
	a.status.LastAttempt = now
	if runErr == nil {
		a.status.LastSuccess, a.status.LastError = now, ""
	} else {
		a.status.LastError = runErr.Error()
		if len(a.status.LastError) > maxStatusError {
			a.status.LastError = strings.ToValidUTF8(a.status.LastError[:maxStatusError], "") + "..."
		}
	}
	a.statusMu.Unlock()
	if runErr != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.LastSuccess = now
	if err := a.saveState(); err != nil {
		a.c.log.Warn("cannot save archive state", "error", err)
	}
}

// List returns the archived logs of slug, newest first, with hours in the
// Collector's location. See ListArchives.
func (a *Archiver) List(ctx context.Context, slug, service string) ([]Archive, error) {
	return ListArchives(ctx, a.store, a.c.loc, slug, service)
}

// LastWindowEnd returns the latest point of the archive grid that is not
// after t. The grid restarts every day at 00:10 in the Collector's location
// and has a point every interval from there.
func (a *Archiver) LastWindowEnd(t time.Time) time.Time {
	local := t.In(a.c.loc)
	base := time.Date(local.Year(), local.Month(), local.Day(), 0, gridMinute, 0, 0, a.c.loc)
	if base.After(t) {
		base = time.Date(local.Year(), local.Month(), local.Day()-1, 0, gridMinute, 0, 0, a.c.loc)
	}
	steps := t.Sub(base) / a.interval
	// The last window of a day is shorter when the interval does not
	// divide the day.
	if last := (24*time.Hour - 1) / a.interval; steps > last {
		steps = last
	}
	return base.Add(steps * a.interval)
}

// NextWindowEnd returns the first point of the archive grid after t.
func (a *Archiver) NextWindowEnd(t time.Time) time.Time {
	last := a.LastWindowEnd(t)
	local := last.In(a.c.loc)
	nextDay := time.Date(local.Year(), local.Month(), local.Day()+1, 0, gridMinute, 0, 0, a.c.loc)
	if next := last.Add(a.interval); next.Before(nextDay) {
		return next
	}
	return nextDay
}

// NextRun returns when the next scheduled run of Run starts: the first point
// of the archive grid after the current time.
func (a *Archiver) NextRun() time.Time {
	return a.NextWindowEnd(a.c.clk.Now())
}

// Run archives at every point of the grid until ctx is done. It starts with
// the window that ended last, which catches up after a restart; that is safe
// because a window is never uploaded twice. A run that failed is repeated
// after RetryInterval.
func (a *Archiver) Run(ctx context.Context) {
	due := a.LastWindowEnd(a.c.clk.Now())
	for {
		// The retry is timed from the start of the run. Reading the clock
		// after the run would race with anyone who reacts to the recorded
		// failure (the status is visible before this loop continues).
		started := a.c.clk.Now()
		err := a.runOnce(ctx, due)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.c.log.Warn("log archive run failed; retrying later", "window_end", due, "error", err)
		}
		next := a.NextWindowEnd(due)
		retryAt := started.Add(a.retryEvery)
		for {
			if !sleep(ctx, a.pollEvery) {
				return
			}
			now := a.c.clk.Now()
			if !now.Before(next) {
				due = a.LastWindowEnd(now)
				break
			}
			if err != nil && !now.Before(retryAt) {
				break
			}
		}
	}
}

func (a *Archiver) runOnce(ctx context.Context, windowEnd time.Time) error {
	err := a.ArchiveOnce(ctx, windowEnd)
	if a.appsDir != "" {
		err = errors.Join(err, a.ArchiveDeployLogs(ctx))
	}
	// Being shut down in the middle of a run is not an outcome.
	if ctx.Err() == nil {
		a.record(err)
	}
	return err
}

// archiveState is the content of the state file.
type archiveState struct {
	Version int `json:"version"`
	// LastSuccess is when a run of Run last ended without an error.
	LastSuccess time.Time `json:"last_success,omitzero"`
	// Services is keyed by "<slug>/<service>".
	Services map[string]*serviceState `json:"services"`
}

// serviceState records how much of each day of one service is archived. A
// day's content (see dayFiles) only ever grows at the end, so an offset into
// it stays valid when the day is compressed.
type serviceState struct {
	// Offsets maps a day to the number of bytes of it that are archived.
	Offsets map[string]int64 `json:"offsets,omitempty"`
	// LastWindow is the start of the newest window that has an object.
	LastWindow time.Time `json:"last_window,omitzero"`
	// Pending is an upload that was decided but not confirmed. It is
	// repeated, to the same key and with the same bytes, before anything
	// else happens.
	Pending *pendingUpload `json:"pending,omitempty"`
}

type pendingUpload struct {
	Key    string    `json:"key"`
	Window time.Time `json:"window"`
	// Upto maps a day to the offset the upload reaches.
	Upto map[string]int64 `json:"upto"`
}

func (a *Archiver) statePath() string { return filepath.Join(a.c.dir, archiveStateName) }

func (a *Archiver) loadState() error {
	if a.state != nil {
		return nil
	}
	state := &archiveState{Version: 1, Services: map[string]*serviceState{}}
	data, err := os.ReadFile(a.statePath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read archive state: %w", err)
	default:
		if err := json.Unmarshal(data, state); err != nil || state.Services == nil {
			// Starting over uploads the local files again under new
			// keys. That duplicates lines in the bucket but loses none.
			a.c.log.Warn("archive state is unreadable; starting over", "path", a.statePath())
			state = &archiveState{Version: 1, Services: map[string]*serviceState{}}
		}
	}
	a.state = state
	return nil
}

func (a *Archiver) saveState() error {
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode archive state: %w", err)
	}
	tmp := a.statePath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write archive state: %w", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, a.statePath())
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write archive state: %w", err)
	}
	return nil
}

// ArchiveOnce uploads, for every app and service with local files, the lines
// that were not uploaded before and are older than windowEnd, as one object
// named after the window that ends there. Services without such lines upload
// nothing.
//
// Lines are taken in the order they were written, up to the first one that
// is not older than windowEnd; a line that arrives late for a window is in
// the object of the next. Calling it again for the same window is harmless.
// It keeps going after a failure and returns all of them; what failed is
// done first by the next call.
func (a *Archiver) ArchiveOnce(ctx context.Context, windowEnd time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.loadState(); err != nil {
		return err
	}
	windowStart := a.LastWindowEnd(windowEnd.Add(-time.Nanosecond))

	targets := map[string]bool{}
	slugs, err := listNames(a.c.dir, checkSlug)
	if err != nil {
		return err
	}
	var errs []error
	for _, slug := range slugs {
		services, err := listNames(a.c.appDir(slug), checkService)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, service := range services {
			targets[slug+"/"+service] = true
		}
	}
	for name, st := range a.state.Services {
		switch {
		case st.Pending != nil:
			targets[name] = true
		case !targets[name] && windowEnd.Sub(st.LastWindow) > 48*time.Hour:
			// The files are gone and so is any risk of reusing a key.
			delete(a.state.Services, name)
		}
	}
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		slug, service, _ := strings.Cut(name, "/")
		if err := a.archiveService(ctx, slug, service, windowStart, windowEnd); err != nil {
			errs = append(errs, fmt.Errorf("archive logs of %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (a *Archiver) archiveService(ctx context.Context, slug, service string, windowStart, windowEnd time.Time) error {
	name := slug + "/" + service
	st := a.state.Services[name]
	if st == nil {
		st = &serviceState{}
	}
	if st.Offsets == nil {
		st.Offsets = map[string]int64{}
	}
	if st.Pending != nil {
		if err := a.upload(ctx, slug, service, name, st); err != nil {
			return err
		}
	}
	if !windowStart.After(st.LastWindow) {
		// This window has its object already. Writing the key again
		// would replace it; what came since goes into the next window.
		return nil
	}
	upto, err := a.plan(slug, service, st, windowEnd)
	if err != nil || len(upto) == 0 {
		return err
	}
	st.Pending = &pendingUpload{
		Key:    windowKey(slug, service, windowStart, a.c.loc),
		Window: windowStart,
		Upto:   upto,
	}
	a.state.Services[name] = st
	// The decision is on disk before the upload starts: after a crash the
	// same bytes go to the same key again.
	if err := a.saveState(); err != nil {
		return err
	}
	return a.upload(ctx, slug, service, name, st)
}

// openedDay is a day's content held open for reading.
type openedDay struct {
	date   string
	stream *dayStream
}

func closeDays(days []openedDay) {
	for _, d := range days {
		d.stream.Close()
	}
}

// openDays opens the days of a service that want accepts, oldest first. It
// holds the app's lock while opening, so every stream is a consistent view of
// its day; reading happens afterwards without the lock.
func (a *Archiver) openDays(slug, service string, want func(date string, size int64) bool) ([]openedDay, map[string]bool, error) {
	st := a.c.state(slug)
	st.mu.Lock()
	defer st.mu.Unlock()
	days, err := listDays(a.c.serviceDir(slug, service))
	if err != nil {
		return nil, nil, err
	}
	existing := map[string]bool{}
	var opened []openedDay
	for _, d := range days {
		existing[d.date] = true
		stream, err := openDay(d)
		if err != nil {
			closeDays(opened)
			return nil, nil, err
		}
		if !want(d.date, stream.size) {
			stream.Close()
			continue
		}
		opened = append(opened, openedDay{date: d.date, stream: stream})
	}
	return opened, existing, nil
}

// plan decides which bytes of each day the next object holds: everything
// after the archived offset up to the first line that is not older than
// windowEnd, and no more than the object size limit in total.
func (a *Archiver) plan(slug, service string, st *serviceState, windowEnd time.Time) (map[string]int64, error) {
	days, existing, err := a.openDays(slug, service, func(date string, size int64) bool {
		return size > st.Offsets[date]
	})
	if err != nil {
		return nil, err
	}
	defer closeDays(days)
	for date := range st.Offsets {
		if !existing[date] {
			delete(st.Offsets, date) // expired
		}
	}

	upto := map[string]int64{}
	var total int64
	for _, d := range days {
		from := st.Offsets[d.date]
		if _, err := io.CopyN(io.Discard, d.stream, from); err != nil {
			return nil, fmt.Errorf("read log file: %w", err)
		}
		taken, stopped, err := measureLines(d.stream, func(head []byte, size int64) bool {
			if total+size > a.maxObject && total > 0 {
				return false
			}
			if ts, ok := entryTime(head); ok && !ts.Before(windowEnd) {
				return false
			}
			total += size
			return true
		})
		if err != nil {
			return nil, err
		}
		if taken > 0 {
			upto[d.date] = from + taken
		}
		if stopped {
			break
		}
	}
	return upto, nil
}

// upload sends the pending object of a service and records it as archived.
func (a *Archiver) upload(ctx context.Context, slug, service, name string, st *serviceState) error {
	pending := st.Pending
	days, _, err := a.openDays(slug, service, func(date string, _ int64) bool {
		return pending.Upto[date] > st.Offsets[date]
	})
	if err != nil {
		return err
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	var total int64
	for _, d := range days {
		from := st.Offsets[d.date]
		_, err := io.CopyN(io.Discard, d.stream, from)
		if err == nil {
			var n int64
			n, err = io.CopyN(zw, d.stream, pending.Upto[d.date]-from)
			total += n
		}
		if err != nil && err != io.EOF {
			closeDays(days)
			return fmt.Errorf("read log file: %w", err)
		}
	}
	closeDays(days)
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compress logs: %w", err)
	}

	// Nothing to send when the files expired while the upload was pending.
	if total > 0 {
		putCtx, cancel := context.WithTimeout(ctx, a.uploadTimeout)
		err := a.store.Put(putCtx, pending.Key, body.Bytes(), archiveMIME)
		cancel()
		if err != nil {
			return fmt.Errorf("upload %s: %w", pending.Key, err)
		}
		if pending.Window.After(st.LastWindow) {
			st.LastWindow = pending.Window
		}
	}
	for date, offset := range pending.Upto {
		if offset > st.Offsets[date] {
			st.Offsets[date] = offset
		}
	}
	st.Pending = nil
	a.state.Services[name] = st
	return a.saveState()
}

// entryTime parses the timestamp at the start of a file line.
func entryTime(head []byte) (time.Time, bool) {
	if i := bytes.IndexByte(head, ' '); i >= 0 {
		head = head[:i]
	}
	ts, err := time.Parse(time.RFC3339Nano, string(bytes.TrimRight(head, "\n")))
	return ts, err == nil
}

// measureLines reads lines from r while accept approves them and returns
// their total size, newlines included. accept gets the beginning of the line
// (all of it unless it is very long) and its size. An unterminated last line
// is not counted. stopped reports that accept refused a line.
func measureLines(r io.Reader, accept func(head []byte, size int64) bool) (total int64, stopped bool, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		head, err := br.ReadSlice('\n')
		size := int64(len(head))
		if err == bufio.ErrBufferFull {
			head = bytes.Clone(head)
			for err == bufio.ErrBufferFull {
				var more []byte
				more, err = br.ReadSlice('\n')
				size += int64(len(more))
			}
		}
		if err == io.EOF {
			return total, false, nil
		}
		if err != nil {
			return total, false, fmt.Errorf("read log file: %w", err)
		}
		if !accept(head, size) {
			return total, true, nil
		}
		total += size
	}
}

// ArchiveDeployLogs uploads the deploy logs below AppsDir that are finished
// and not in the bucket yet, as logs/<slug>/_deploy/<id>.log.gz. A log counts
// as finished when it was not modified for DeployQuiet. Logs older than the
// local retention period are left alone, so that a log the bucket's
// lifecycle rule deleted is not uploaded again.
func (a *Archiver) ArchiveDeployLogs(ctx context.Context) error {
	if a.appsDir == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	slugs, err := listNames(a.appsDir, checkSlug)
	if err != nil {
		return err
	}
	now := a.c.clk.Now()
	maxAge := time.Duration(a.c.retentionDays) * 24 * time.Hour
	var errs []error
	for _, slug := range slugs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		dir := filepath.Join(a.appsDir, slug, "deploys")
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("list deploy logs of %s: %w", slug, err))
			}
			continue
		}
		type candidate struct {
			path string
			key  string
		}
		var candidates []candidate
		for _, entry := range entries {
			idText, ok := strings.CutSuffix(entry.Name(), plainExt)
			if !ok || entry.IsDir() {
				continue
			}
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if age := now.Sub(info.ModTime()); age < a.deployQuiet || age > maxAge {
				continue
			}
			candidates = append(candidates, candidate{path: filepath.Join(dir, entry.Name()), key: deployKey(slug, id)})
		}
		if len(candidates) == 0 {
			continue
		}
		listCtx, cancel := context.WithTimeout(ctx, a.uploadTimeout)
		objects, err := a.store.List(listCtx, appPrefix(slug)+DeployService+"/")
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("list archived deploy logs of %s: %w", slug, err))
			continue
		}
		archived := map[string]bool{}
		for _, object := range objects {
			archived[object.Key] = true
		}
		for _, cand := range candidates {
			if archived[cand.key] {
				continue
			}
			if err := a.uploadDeployLog(ctx, cand.path, cand.key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (a *Archiver) uploadDeployLog(ctx context.Context, path, key string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open deploy log: %w", err)
	}
	defer f.Close()
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if _, err := io.Copy(zw, io.LimitReader(f, maxDeployLogBytes)); err != nil {
		return fmt.Errorf("read deploy log: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compress deploy log: %w", err)
	}
	putCtx, cancel := context.WithTimeout(ctx, a.uploadTimeout)
	defer cancel()
	if err := a.store.Put(putCtx, key, body.Bytes(), archiveMIME); err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}
