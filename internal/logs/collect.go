package logs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

const (
	// resumeOverlap is how far before the newest written entry a follow
	// command starts again. Streams of different containers do not arrive
	// in step, so the newest entry of one says little about the others;
	// the overlap is read again and compared with what the files hold.
	resumeOverlap = 30 * time.Second

	// composePrefixRoom is the space allowed for the container name and
	// timestamp in front of the text of a Compose log line.
	composePrefixRoom = 512

	// commitEvery bounds how many lines are held before they are written.
	commitEvery = 1024

	// resumeMarkerName is a file in an app's directory that exists while
	// a follow command may still be delivering its backlog. It holds the
	// time that command started from.
	resumeMarkerName = ".resume"

	// A follow command has delivered its backlog once its output paused
	// for settleIdle, or settleMax after it started.
	defaultSettleIdle = 2 * time.Second
	defaultSettleMax  = 5 * time.Minute
)

// Run collects the logs of the apps returned by Options.Apps until ctx is
// done. It reconciles the set of apps periodically: new apps get a follower,
// removed apps lose theirs. It only returns early, with an error, when the
// Collector was built without Runner or Apps.
func (c *Collector) Run(ctx context.Context) error {
	if c.run == nil || c.apps == nil {
		return errors.New("logs: Run needs Options.Runner and Options.Apps")
	}
	running := map[string]*follower{}
	defer func() {
		for _, f := range running {
			f.cancel()
		}
		for _, f := range running {
			<-f.done
		}
	}()

	failing := false
	for {
		apps, err := c.apps(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			if !failing {
				c.log.Warn("cannot list apps for log collection; retrying", "error", err)
			}
			failing = true
		default:
			failing = false
			c.reconcile(ctx, running, apps)
		}
		if !sleep(ctx, c.reconcileEvery) {
			return nil
		}
	}
}

// follower is the collection of one app: the Compose log stream and,
// optionally, the access log.
type follower struct {
	app    App
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *Collector) reconcile(ctx context.Context, running map[string]*follower, apps []App) {
	wanted := map[string]App{}
	for _, app := range apps {
		if checkSlug(app.Slug) != nil || app.Project == "" || strings.HasPrefix(app.Project, "-") {
			c.log.Debug("skipping app that cannot be collected", "app", app.Slug)
			continue
		}
		wanted[app.Slug] = app
	}
	for slug, f := range running {
		if app, ok := wanted[slug]; ok && app == f.app {
			continue
		}
		// Wait for the old follower: two of them must never write the
		// same app.
		f.cancel()
		<-f.done
		delete(running, slug)
	}
	for slug, app := range wanted {
		if _, ok := running[slug]; !ok {
			running[slug] = c.startFollower(ctx, app)
		}
	}
}

func (c *Collector) startFollower(ctx context.Context, app App) *follower {
	ctx, cancel := context.WithCancel(ctx)
	f := &follower{app: app, cancel: cancel, done: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		(&composeFollow{c: c, app: app}).run(ctx)
	}()
	if app.AccessLogPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			(&accessTail{c: c, app: app}).run(ctx)
		}()
	}
	go func() {
		wg.Wait()
		c.closeWriters(app.Slug)
		close(f.done)
	}()
	return f
}

// resume remembers what was written recently, so that a stream can be picked
// up again without a gap and without writing a line twice.
//
// A stream is started again a little before the newest written entry, and
// every line it delivers is checked against the entries written from that
// point on. Entries are compared by service, timestamp and text and counted,
// so distinct lines with the same timestamp are all kept, and so are
// identical lines of two replicas of a service. (A line one stream prints
// twice for the same container is a different matter: see replayFilter.)
type resume struct {
	loaded bool
	// high is the newest timestamp written.
	high time.Time
	// open says that a stream which started at floor (zero: at the
	// beginning) is not known to have delivered its whole backlog. While
	// it is, the newest entry proves nothing about slower containers, and
	// the next stream must start at floor again.
	open  bool
	floor time.Time
	// recent holds the entries written since the point start returns.
	recent  []recentEntry
	pruneAt int
}

type recentEntry struct {
	ts  int64 // nanoseconds since the epoch
	key uint64
}

// start returns the time the next stream must start from. all means from the
// beginning.
func (r *resume) start() (since time.Time, all bool) {
	if r.high.IsZero() || (r.open && r.floor.IsZero()) {
		return time.Time{}, true
	}
	since = r.high.Add(-resumeOverlap)
	if r.open && r.floor.Before(since) {
		since = r.floor
	}
	return since, false
}

// load rebuilds the state from the files of the services of slug that
// include accepts. It reads the newest day of every service completely: the
// files are the truth, and lines are not strictly in time order in them.
func (r *resume) load(ctx context.Context, c *Collector, slug string, include func(service string) bool, open bool, floor time.Time) error {
	*r = resume{open: open, floor: floor}
	services, err := listNames(c.appDir(slug), checkService)
	if err != nil {
		return err
	}
	days := map[string][]dayFiles{}
	for _, service := range services {
		if !include(service) {
			continue
		}
		list, err := listDays(c.serviceDir(slug, service))
		if err != nil {
			return err
		}
		days[service] = list
		// The newest day that has entries holds the newest entry.
		for i := len(list) - 1; i >= 0; i-- {
			var newest time.Time
			err := scanDay(ctx, list[i], func(ts time.Time, _ []byte) {
				if ts.After(newest) {
					newest = ts
				}
			})
			if err != nil {
				return err
			}
			if newest.After(r.high) {
				r.high = newest
			}
			if !newest.IsZero() {
				break
			}
		}
	}
	since, all := r.start()
	if !r.high.IsZero() {
		sinceDay := c.day(since)
		for service, list := range days {
			for _, d := range list {
				if !all && d.date < sinceDay {
					continue
				}
				err := scanDay(ctx, d, func(ts time.Time, text []byte) {
					if all || !ts.Before(since) {
						r.recent = append(r.recent, recentEntry{ts: ts.UnixNano(), key: entryKey(service, ts, string(text))})
					}
				})
				if err != nil {
					return err
				}
			}
		}
	}
	r.pruneAt = 2*len(r.recent) + commitEvery
	r.loaded = true
	return nil
}

// scanDay calls fn for every entry of a day.
func scanDay(ctx context.Context, d dayFiles, fn func(ts time.Time, text []byte)) error {
	stream, err := openDay(d)
	if err != nil {
		return err
	}
	defer stream.Close()
	return scanEntries(ctx, stream, func(ts time.Time, text []byte) error {
		fn(ts, text)
		return nil
	})
}

// begin returns what a stream that starts now needs: the time to start from
// (all: from the beginning) and the entries already written from there on,
// counted by key.
func (r *resume) begin() (since time.Time, all bool, seen map[uint64]int) {
	since, all = r.start()
	seen = make(map[uint64]int, len(r.recent))
	from := since.UnixNano()
	for _, e := range r.recent {
		if all || e.ts >= from {
			seen[e.key]++
		}
	}
	return since, all, seen
}

// note records a written entry.
func (r *resume) note(ts time.Time, key uint64) {
	if ts.After(r.high) {
		r.high = ts
	}
	r.recent = append(r.recent, recentEntry{ts: ts.UnixNano(), key: key})
	if len(r.recent) < r.pruneAt {
		return
	}
	if since, all := r.start(); !all {
		from := since.UnixNano()
		kept := r.recent[:0]
		for _, e := range r.recent {
			if e.ts >= from {
				kept = append(kept, e)
			}
		}
		r.recent = kept
	}
	r.pruneAt = 2*len(r.recent) + commitEvery
}

// composeFollow keeps `docker compose logs --follow` of one app running and
// writes what it prints.
type composeFollow struct {
	c   *Collector
	app App
	res resume
}

func (f *composeFollow) run(ctx context.Context) {
	delay := f.c.restartDelay
	failing := false
	for {
		started := time.Now()
		wrote, err := f.once(ctx)
		if ctx.Err() != nil {
			return
		}
		// The command ends whenever the app has no containers, which is
		// normal for a stopped app: only a change of state is worth a
		// message.
		switch {
		case err != nil && !failing:
			failing = true
			f.c.log.Warn("log follow failed; retrying", "app", f.app.Slug, "error", err)
		case err == nil && failing:
			failing = false
			f.c.log.Info("log follow recovered", "app", f.app.Slug)
		}
		if wrote || time.Since(started) >= time.Minute {
			delay = f.c.restartDelay
		}
		if !sleep(ctx, delay) {
			return
		}
		delay = min(2*delay, f.c.maxRestartDelay)
	}
}

// once runs the follow command until it ends and reports whether it wrote
// any line.
func (f *composeFollow) once(ctx context.Context) (bool, error) {
	if !f.res.loaded {
		open, floor, err := f.readMarker()
		if err != nil {
			return false, err
		}
		isContainerService := func(service string) bool { return !strings.HasPrefix(service, "_") }
		if err := f.res.load(ctx, f.c, f.app.Slug, isContainerService, open, floor); err != nil {
			return false, err
		}
	}
	w := &followWriter{f: f, started: time.Now(), replay: replayFilter{}}
	w.since, w.all, w.seen = f.res.begin()
	w.sp = splitter{max: f.c.maxLine + composePrefixRoom, emit: w.line}

	args := []string{"compose", "-p", f.app.Project, "logs", "--follow", "--timestamps", "--no-color"}
	if !w.all {
		// Docker's --since is inclusive; what the files already hold is
		// dropped in followWriter.
		args = append(args, "--since", w.since.Format(time.RFC3339Nano))
	}
	// Standard error is discarded: it is not known to be free of app output.
	err := f.c.run.Run(ctx, runner.Cmd{Name: "docker", Args: args, Stdout: w})
	// A line without its newline is incomplete; it comes again in full.
	if w.err == nil {
		w.err = w.commit()
	}
	if w.err != nil {
		// What reached the disk is unknown: read it back next time.
		f.res.loaded = false
		return w.wrote, w.err
	}
	if err == nil {
		// The command ended by itself, so it delivered all there was.
		f.settle()
	}
	return w.wrote, err
}

func (f *composeFollow) markerPath() string {
	return filepath.Join(f.c.appDir(f.app.Slug), resumeMarkerName)
}

// readMarker reports whether a previous process left a stream open, and
// where that stream had started.
func (f *composeFollow) readMarker() (open bool, floor time.Time, err error) {
	data, err := os.ReadFile(f.markerPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, fmt.Errorf("read resume marker: %w", err)
	}
	floor, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		// Unreadable: start from the beginning, which loses nothing.
		return true, time.Time{}, nil
	}
	return true, floor, nil
}

// openFill records, before the first line of a stream that started at since
// is written, that its backlog is outstanding. A stream that is open already
// keeps its older start. A stream that writes nothing needs no record: it
// does not move the newest entry.
func (f *composeFollow) openFill(since time.Time) error {
	if f.res.open {
		return nil
	}
	st := f.c.state(f.app.Slug)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.MkdirAll(f.c.appDir(f.app.Slug), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if err := writeFileAtomic(f.markerPath(), []byte(since.UTC().Format(time.RFC3339Nano)+"\n")); err != nil {
		return fmt.Errorf("write resume marker: %w", err)
	}
	f.res.open, f.res.floor = true, since
	return nil
}

// settle records that the stream caught up with the present.
func (f *composeFollow) settle() {
	if !f.res.open {
		return
	}
	if err := os.Remove(f.markerPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return // stays open; tried again at the next opportunity
	}
	f.res.open = false
}

// writeFileAtomic replaces path with data by way of a temporary file.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// replayWindow is how many of a container's latest lines one run of the
// follow command remembers.
const replayWindow = 4096

// replayFilter drops lines that one run of the follow command prints twice.
//
// `docker compose logs --follow` attaches to the containers it finds and to
// every container that starts later. Started while a container is coming up
// (which is what a follower waiting for a stopped app does at every deploy),
// it can attach to that container twice, and from then on prints each of its
// lines twice, timestamp included (seen with Compose 5.8; ARCHITECTURE 12).
// Docker stamps every line of a container with its own nanosecond, so a line
// that repeats container, timestamp and text is such an echo and not output
// the app printed twice. The two copies arrive close together: remembering
// the latest lines of each container is enough.
type replayFilter map[string]*recentKeys

// recentKeys is a ring of the latest keys with a count per key.
type recentKeys struct {
	ring  []uint64
	next  int
	count map[uint64]int
}

// printedBefore reports whether this run already printed the line with this
// key for the container, and remembers the key otherwise.
func (r replayFilter) printedBefore(container string, key uint64) bool {
	recent := r[container]
	if recent == nil {
		recent = &recentKeys{count: map[uint64]int{}}
		r[container] = recent
	}
	if recent.count[key] > 0 {
		return true
	}
	if len(recent.ring) < replayWindow {
		recent.ring = append(recent.ring, key)
	} else {
		old := recent.ring[recent.next]
		if recent.count[old]--; recent.count[old] <= 0 {
			delete(recent.count, old)
		}
		recent.ring[recent.next] = key
		recent.next = (recent.next + 1) % replayWindow
	}
	recent.count[key]++
	return false
}

// followWriter receives the standard output of one run of the follow command.
type followWriter struct {
	f  *composeFollow
	sp splitter

	since time.Time
	all   bool
	seen  map[uint64]int
	// replay recognises lines this run of the command printed twice.
	replay replayFilter

	started   time.Time
	lastWrite time.Time
	// live is set once the backlog is over; marked once the resume marker
	// was written for this run.
	live   bool
	marked bool

	batch []Line
	keys  []uint64
	wrote bool
	err   error
}

// Write is called with whatever the command printed. Failing it makes the
// runner stop the command; the follower then starts over from the files.
func (w *followWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	// The backlog comes in one burst. Output after a pause, or long after
	// the start, is live.
	now := time.Now()
	if !w.live && !w.lastWrite.IsZero() && (now.Sub(w.lastWrite) >= w.f.c.settleIdle || now.Sub(w.started) >= w.f.c.settleMax) {
		w.live = true
		w.f.settle()
	}
	w.lastWrite = now

	_, _ = w.sp.Write(p)
	if w.err == nil {
		w.err = w.commit()
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

func (w *followWriter) line(raw []byte, truncated bool) {
	if w.err != nil {
		return
	}
	name, ts, rawText, ok := parseComposeLine(raw)
	if !ok {
		return // a status message of Compose, or damage
	}
	service, ok := serviceOfContainer(name)
	if !ok {
		return
	}
	if !w.all && ts.Before(w.since) {
		return
	}
	text := cleanText(rawText, w.f.c.maxLine, truncated)
	if w.replay.printedBefore(name, entryKey(name, ts, text)) {
		return
	}
	key := entryKey(service, ts, text)
	if w.seen[key] > 0 {
		w.seen[key]--
		return // already in the files
	}
	w.batch = append(w.batch, Line{Service: service, Time: ts, Text: text})
	w.keys = append(w.keys, key)
	if len(w.batch) >= commitEvery {
		w.err = w.commit()
	}
}

func (w *followWriter) commit() error {
	if len(w.batch) == 0 {
		return nil
	}
	if !w.live && !w.marked {
		if err := w.f.openFill(w.since); err != nil {
			return err
		}
		w.marked = true
	}
	if err := w.f.c.write(w.f.app.Slug, w.batch); err != nil {
		return err
	}
	for i, line := range w.batch {
		w.f.res.note(line.Time, w.keys[i])
	}
	w.wrote = true
	// Subscribers may still hold the lines; start a fresh batch.
	w.batch, w.keys = nil, w.keys[:0]
	return nil
}
