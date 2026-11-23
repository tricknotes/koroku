// Package logs collects the output of deployed apps into local files, serves
// it to readers (live subscribers, tail, search) and archives it to object
// storage.
//
// Local layout, below Options.Dir:
//
//	<slug>/<service>/YYYY-MM-DD.log      lines of one day, appended as they arrive
//	<slug>/<service>/YYYY-MM-DD.log.gz   the same, once the day is over
//	<slug>/.resume                       present while a restart must not trust the newest entry
//	.archive-state.json                  what the Archiver already uploaded
//
// Every entry is one line: a UTC timestamp with nanoseconds, a space and the
// text. The day of a file is the day of the entry's timestamp in
// Options.Location.
//
// Log lines are app output and may contain secrets. Nothing in this package
// puts line content into errors or into koroku's own log.
package logs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/runner"
)

const (
	// AccessService is the pseudo-service that holds the reverse proxy's
	// access log of an app.
	AccessService = "_access"
	// DeployService is the pseudo-service under which deploy logs are
	// archived. It only exists in object storage.
	DeployService = "_deploy"
)

// MaxLineBytesLimit is the largest accepted Options.MaxLineBytes.
const MaxLineBytesLimit = 256 << 10

// ErrInvalidName is returned when a slug or service name cannot be used as a
// directory name.
var ErrInvalidName = errors.New("logs: invalid name")

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	// Service names follow Compose. A leading underscore is reserved for
	// pseudo-services.
	servicePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Line is one log entry.
type Line struct {
	Service string
	// Time is when the line was produced, in UTC.
	Time time.Time
	Text string
}

// App is one app whose logs are collected.
type App struct {
	Slug string
	// Project is the Compose project name (koroku-<slug>).
	Project string
	// AccessLogPath is the access log the reverse proxy writes for this app.
	// Empty means there is none.
	AccessLogPath string
	// PublicService, when set, is the public service of a blue/green app:
	// the lines of its coloured copies (<service>-blue, <service>-green)
	// are recorded under it (ARCHITECTURE 7.2).
	PublicService string
}

// recordedService maps the service of a container to the one its lines are
// recorded under.
func (a App) recordedService(service string) string {
	if a.PublicService != "" && (service == a.PublicService+"-blue" || service == a.PublicService+"-green") {
		return a.PublicService
	}
	return service
}

// Options configure a Collector. Only Dir is required for reading; Run also
// needs Runner and Apps.
type Options struct {
	// Dir is the root of the local log files.
	Dir string
	// Runner executes docker. Required by Run.
	Runner runner.Runner
	// Clock defaults to the system clock.
	Clock clock.Clock
	// Location decides which day file an entry belongs to and where the
	// archive grid starts. The default is UTC.
	Location *time.Location
	// Apps lists the apps to collect. Run calls it periodically.
	Apps func(ctx context.Context) ([]App, error)
	// ComposePlugin returns the Compose plugin binary to run the follow
	// commands with, or "" to run them as `docker compose`. Run calls it
	// before each follow command. FindComposePlugin is the real lookup; the
	// default is `docker compose`.
	ComposePlugin func() string
	// Logger receives operational messages (never line content). The
	// default discards them.
	Logger *slog.Logger

	// RetentionDays is how many past days stay on local disk. Default 14.
	RetentionDays int
	// MaxLineBytes bounds the text of one entry; longer lines are cut.
	// Default 16 KiB, at most MaxLineBytesLimit.
	MaxLineBytes int
	// SubscriberBuffer is the channel capacity of a live subscriber.
	// Default 1024: a burst larger than that outruns the subscriber.
	SubscriberBuffer int

	// ReconcileInterval is how often Apps is consulted. Default 15s.
	ReconcileInterval time.Duration
	// RestartDelay is the pause before the follow command is started again
	// after it ended. It doubles up to MaxRestartDelay while the command
	// keeps ending without output. Defaults 2s and 15s.
	RestartDelay    time.Duration
	MaxRestartDelay time.Duration
	// AccessPollInterval is how often access logs are checked for new
	// entries. Default 1s.
	AccessPollInterval time.Duration
	// MaintainInterval is the period of RunMaintenance. Default 1h.
	MaintainInterval time.Duration

	// OnAccess, when set, is called with every new access log entry of an
	// app as it is collected (not with the entries read again after a
	// restart). It runs on the collecting goroutine, must return quickly
	// and must not keep entry. The idle tracker counts requests with it (ARCHITECTURE 8.4).
	OnAccess func(slug string, entry []byte)
}

// Collector owns the local log files: it writes them from the running apps,
// keeps them compressed and bounded, and reads them back.
type Collector struct {
	dir  string
	run  runner.Runner
	clk  clock.Clock
	loc  *time.Location
	apps func(ctx context.Context) ([]App, error)
	log  *slog.Logger

	composePlugin func() string
	onAccess      func(slug string, entry []byte)

	retentionDays    int
	maxLine          int
	subscriberBuffer int
	reconcileEvery   time.Duration
	restartDelay     time.Duration
	maxRestartDelay  time.Duration
	accessPoll       time.Duration
	maintainEvery    time.Duration
	settleIdle       time.Duration
	settleMax        time.Duration

	mu     sync.Mutex
	states map[string]*appState

	subMu sync.RWMutex
	subs  map[string]map[*subscriber]struct{}

	maintainMu sync.Mutex
}

// New returns a Collector for the files below opts.Dir. It creates the
// directory and finishes compressions that a previous process left half done.
func New(opts Options) (*Collector, error) {
	if opts.Dir == "" {
		return nil, errors.New("logs: Dir is required")
	}
	if opts.MaxLineBytes < 0 || opts.MaxLineBytes > MaxLineBytesLimit {
		return nil, fmt.Errorf("logs: MaxLineBytes must be between 1 and %d", MaxLineBytesLimit)
	}
	if opts.RetentionDays < 0 {
		return nil, errors.New("logs: RetentionDays must not be negative")
	}
	c := &Collector{
		dir:              filepath.Clean(opts.Dir),
		run:              opts.Runner,
		clk:              opts.Clock,
		loc:              opts.Location,
		apps:             opts.Apps,
		composePlugin:    opts.ComposePlugin,
		onAccess:         opts.OnAccess,
		log:              opts.Logger,
		retentionDays:    orDefault(opts.RetentionDays, 14),
		maxLine:          orDefault(opts.MaxLineBytes, 16<<10),
		subscriberBuffer: orDefault(opts.SubscriberBuffer, 1024),
		reconcileEvery:   orDefault(opts.ReconcileInterval, 15*time.Second),
		restartDelay:     orDefault(opts.RestartDelay, 2*time.Second),
		maxRestartDelay:  orDefault(opts.MaxRestartDelay, 15*time.Second),
		accessPoll:       orDefault(opts.AccessPollInterval, time.Second),
		maintainEvery:    orDefault(opts.MaintainInterval, time.Hour),
		settleIdle:       defaultSettleIdle,
		settleMax:        defaultSettleMax,
		states:           map[string]*appState{},
		subs:             map[string]map[*subscriber]struct{}{},
	}
	if c.clk == nil {
		c.clk = clock.System{}
	}
	if c.loc == nil {
		c.loc = time.UTC
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.maxRestartDelay < c.restartDelay {
		c.maxRestartDelay = c.restartDelay
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	if err := c.recoverCompressions(); err != nil {
		return nil, err
	}
	return c, nil
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

func checkSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("%w: slug %q", ErrInvalidName, slug)
	}
	return nil
}

func checkService(service string) error {
	if !servicePattern.MatchString(service) {
		return fmt.Errorf("%w: service %q", ErrInvalidName, service)
	}
	return nil
}

func (c *Collector) appDir(slug string) string { return filepath.Join(c.dir, slug) }

func (c *Collector) serviceDir(slug, service string) string {
	return filepath.Join(c.dir, slug, service)
}

// day returns the day file date of t.
func (c *Collector) day(t time.Time) string { return t.In(c.loc).Format(dateLayout) }

// sleep waits for d or until ctx is done, and reports whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
