package logs

import (
	"errors"
	"strings"
	"testing"
)

func TestArchiveStatus(t *testing.T) {
	const secret = "s3cr3t-in-a-log-line"
	dir := t.TempDir()
	f := newFixtureIn(t, dir, nil)
	f.clock.Set(at(18, 3, 0))
	store := newFlakyStore()
	a := newArchiver(t, f, store, nil)
	if s := a.Status(); !s.LastAttempt.IsZero() || !s.LastSuccess.IsZero() || s.LastError != "" {
		t.Errorf("status before the first run = %+v, want it empty", s)
	}

	f.put("blog", "web", at(18, 0, 5), "line with "+secret)
	store.failWith(func(string) (bool, error) { return false, errors.New("bucket unreachable") })
	stop := start(t, a.Run)
	waitFor(t, "the failed run", func() bool { return a.Status().LastError != "" })
	s := a.Status()
	if !s.LastAttempt.Equal(at(18, 3, 0)) || !s.LastSuccess.IsZero() {
		t.Errorf("after a failed run: %+v", s)
	}
	if !strings.Contains(s.LastError, "logs/blog/web/2026/11/17/20.log.gz") || !strings.Contains(s.LastError, "bucket unreachable") {
		t.Errorf("LastError = %q, want the key and the cause", s.LastError)
	}
	if strings.Contains(s.LastError, secret) {
		t.Errorf("LastError contains log content: %q", s.LastError)
	}

	// The retry succeeds.
	store.failWith(nil)
	f.clock.Set(at(18, 3, 11))
	waitFor(t, "the successful retry", func() bool { return a.Status().LastError == "" })
	s = a.Status()
	if !s.LastAttempt.Equal(at(18, 3, 11)) || !s.LastSuccess.Equal(at(18, 3, 11)) {
		t.Errorf("after a successful run: %+v", s)
	}

	// A later failure keeps the time of the last success.
	f.put("blog", "web", at(18, 3, 30), "next window")
	store.failWith(func(string) (bool, error) { return false, errors.New("bucket unreachable") })
	f.clock.Set(at(18, 4, 10))
	waitFor(t, "the next failed run", func() bool { return a.Status().LastError != "" })
	s = a.Status()
	if !s.LastAttempt.Equal(at(18, 4, 10)) || !s.LastSuccess.Equal(at(18, 3, 11)) {
		t.Errorf("after a later failure: %+v", s)
	}
	stop()

	// The time of the last success survives a restart; the error does not.
	f2 := newFixtureIn(t, dir, nil)
	s = newArchiver(t, f2, store, nil).Status()
	if !s.LastSuccess.Equal(at(18, 3, 11)) || !s.LastAttempt.IsZero() || s.LastError != "" {
		t.Errorf("after a restart: %+v", s)
	}
}

func TestArchiveStatusBoundsTheError(t *testing.T) {
	f := newFixture(t, nil)
	a := newArchiver(t, f, newFlakyStore(), nil)
	a.record(errors.New(strings.Repeat("ログ", 2000)))
	if s := a.Status(); len(s.LastError) > maxStatusError+3 || !strings.HasSuffix(s.LastError, "...") {
		t.Errorf("LastError has %d bytes", len(s.LastError))
	}
}
