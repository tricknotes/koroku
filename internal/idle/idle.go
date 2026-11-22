// Package idle watches the requests that reach the apps through the reverse
// proxy and puts the apps that opted in to sleep when none came for their
// idle time (ARCHITECTURE 8.4).
//
// Requests are learnt from the per-app access logs: the log collector hands
// every new entry to Observe. The last request of each app is kept in memory
// only; at startup every app counts as just used.
package idle

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// DefaultInterval is how often the idle time of the apps is looked at.
const DefaultInterval = time.Minute

// Sleeper puts an app to sleep. *deploy.Engine implements it; it returns
// deploy.ErrBusy when the app is in use and should be tried again later.
type Sleeper interface {
	Sleep(ctx context.Context, app store.App) error
}

// Tracker keeps the time of each app's last request and puts idle apps to
// sleep.
type Tracker struct {
	Store   *store.Store
	Sleeper Sleeper
	Clock   clock.Clock
	Logger  *slog.Logger
	// AccessLogDir holds the access log of each app, <slug>.log. The files
	// are looked at as a safeguard in case the collector stops delivering.
	AccessLogDir string
	// ConsoleURL is the console's origin: the access gate of a protected
	// app redirects to <ConsoleURL>/auth/gate (ARCHITECTURE 8.3).
	ConsoleURL string
	// Busy, when set, reports that nothing may be put to sleep now (a
	// backup runs, and its dump hook may be using a container).
	Busy func() bool

	mu   sync.Mutex
	apps map[string]*appState
}

type appState struct {
	lastActive time.Time
	// entries counts the access log entries heard since the last check,
	// whether they count as a request or not.
	entries   int
	protected bool

	// What the last check saw, to notice that the app woke or changed.
	seen       bool
	sleeping   bool
	running    bool
	deployment int64

	fileSeen bool
	fileSize int64
	fileMod  time.Time
}

func (t *Tracker) now() time.Time {
	if t.Clock == nil {
		return time.Now()
	}
	return t.Clock.Now()
}

func (t *Tracker) logger() *slog.Logger {
	if t.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return t.Logger
}

// state returns the app's state, creating it as just used. t.mu is held.
func (t *Tracker) state(slug string) *appState {
	if t.apps == nil {
		t.apps = map[string]*appState{}
	}
	st, ok := t.apps[slug]
	if !ok {
		st = &appState{lastActive: t.now()}
		t.apps[slug] = st
	}
	return st
}

// Observe takes one new access log entry of the app named slug.
func (t *Tracker) Observe(slug string, entry []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state(slug)
	st.entries++
	if counts(entry, st.protected, t.ConsoleURL) {
		st.lastActive = t.now()
	}
}

// counts reports whether an access log entry is a request that keeps the
// app awake. For a protected app, what the gate answered without letting
// the request through (the redirect to the console's login, 401) does not:
// a crawler that cannot log in must not keep the app awake.
func counts(entry []byte, protected bool, consoleURL string) bool {
	if !protected {
		return true
	}
	var e struct {
		Status      int                 `json:"status"`
		RespHeaders map[string][]string `json:"resp_headers"`
	}
	if json.Unmarshal(entry, &e) != nil {
		return true
	}
	switch e.Status {
	case 401:
		return false
	case 302:
		gate := strings.TrimSuffix(consoleURL, "/") + "/auth/gate"
		for name, values := range e.RespHeaders {
			if !strings.EqualFold(name, "Location") {
				continue
			}
			for _, v := range values {
				if consoleURL != "" && strings.HasPrefix(v, gate) {
					return false
				}
			}
		}
	}
	return true
}

// Check looks at every app once and puts those idle for longer than their
// setting to sleep.
func (t *Tracker) Check(ctx context.Context) {
	apps, err := t.Store.ListApps(ctx)
	if err != nil {
		t.logger().Error("listing apps for the idle check failed", "error", err)
		return
	}
	now := t.now()
	var idle []store.App
	t.mu.Lock()
	present := map[string]bool{}
	for _, app := range apps {
		present[app.Slug] = true
		st := t.state(app.Slug)
		st.protected = app.AccessProtection
		running := app.DesiredState == store.DesiredRunning && app.CurrentDeploymentID != nil
		sleeping := app.SleepingSince != nil
		var deployment int64
		if app.CurrentDeploymentID != nil {
			deployment = *app.CurrentDeploymentID
		}
		// An app that woke up, was started or got a new release counts
		// as just used: what happened before says nothing now.
		if st.seen && ((st.sleeping && !sleeping) || (!st.running && running) || st.deployment != deployment) {
			st.lastActive = now
		}
		st.seen, st.sleeping, st.running, st.deployment = true, sleeping, running, deployment
		t.checkFile(app.Slug, st, now)
		st.entries = 0

		minutes := app.IdleSleepMinutes
		if minutes <= 0 {
			minutes = store.DefaultIdleSleepMinutes
		}
		if app.IdleSleep && running && !sleeping && now.Sub(st.lastActive) >= time.Duration(minutes)*time.Minute {
			idle = append(idle, app)
		}
	}
	for slug := range t.apps {
		if !present[slug] {
			delete(t.apps, slug)
		}
	}
	t.mu.Unlock()

	if len(idle) == 0 || (t.Busy != nil && t.Busy()) {
		return
	}
	for _, app := range idle {
		if ctx.Err() != nil {
			return
		}
		err := t.Sleeper.Sleep(ctx, app)
		switch {
		case errors.Is(err, deploy.ErrBusy):
			t.logger().Debug("not putting a busy app to sleep", "app", app.Slug)
		case err != nil:
			t.logger().Error("putting an idle app to sleep failed", "app", app.Slug, "error", err)
		default:
			// So that the next check notices when it wakes up.
			t.mu.Lock()
			if st, ok := t.apps[app.Slug]; ok {
				st.sleeping = true
			}
			t.mu.Unlock()
		}
	}
}

// checkFile is the safeguard against a collector that stopped delivering:
// an access log that changed while no entry was heard counts as a
// request. t.mu is held.
func (t *Tracker) checkFile(slug string, st *appState, now time.Time) {
	if t.AccessLogDir == "" {
		return
	}
	info, err := os.Stat(filepath.Join(t.AccessLogDir, slug+".log"))
	if err != nil {
		// No file yet (no request since it was configured), or it is
		// being rotated: nothing to compare.
		st.fileSeen = false
		return
	}
	changed := !st.fileSeen || info.Size() != st.fileSize || !info.ModTime().Equal(st.fileMod)
	if st.fileSeen && changed && st.entries == 0 {
		st.lastActive = now
	}
	st.fileSeen, st.fileSize, st.fileMod = true, info.Size(), info.ModTime()
}

// Run checks the apps every interval until ctx ends.
func (t *Tracker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.Check(ctx)
		}
	}
}

// LastActive returns when the app last counted as used; ok is false for
// an app the tracker has not seen yet.
func (t *Tracker) LastActive(slug string) (at time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.apps[slug]
	if !ok {
		return time.Time{}, false
	}
	return st.lastActive, true
}
