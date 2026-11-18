package logs

import (
	"fmt"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

func TestCollectLargeBurstIsNotDuplicatedOnResume(t *testing.T) {
	// More lines than one commit holds, spread over two minutes so that
	// only the tail is inside the resume overlap.
	const n = 3000
	var burst []held
	for i := 0; i < n; i++ {
		burst = append(burst, held{fmt.Sprintf("web-%d", 1+i%2), t0.Add(time.Duration(i) * 40 * time.Millisecond), fmt.Sprintf("line %d", i)})
	}
	dir := t.TempDir()
	count := func() int { return len(serviceContent(t, dir, "blog", "web")) }

	f1 := newFixtureIn(t, dir, func(o *Options) {
		o.Runner = &runner.Fake{Handler: daemon(burst[:n-1], burst[n-1])}
		o.Apps = oneApp(blog)
	})
	stop := f1.startRun()
	waitFor(t, "the burst", func() bool { return count() == n })
	stop()

	extra := held{"web-1", t0.Add(n * 40 * time.Millisecond), "after the restart"}
	fake := &runner.Fake{Handler: daemon(append(burst, extra))}
	f2 := newFixtureIn(t, dir, func(o *Options) { o.Runner = fake; o.Apps = oneApp(blog) })
	f2.startRun()
	waitFor(t, "the line after the restart", func() bool { return count() >= n+1 })

	got := texts(serviceContent(t, dir, "blog", "web"))
	if len(got) != n+1 || got[n] != "after the restart" {
		t.Fatalf("%d lines stored, want %d ending with the new one", len(got), n+1)
	}
	for i := 0; i < n; i++ {
		if got[i] != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d = %q", i, got[i])
		}
	}
	wantSince(t, fake.Calls()[0], burst[n-1].ts.Add(-resumeOverlap))
}

func TestResumeRemembersOnlyWhatAStreamCanRepeat(t *testing.T) {
	var r resume
	r.loaded = true
	note := func(i int) {
		ts := t0.Add(time.Duration(i) * 100 * time.Millisecond)
		r.note(ts, entryKey("web", ts, fmt.Sprint(i)))
	}
	for i := 0; i < 20000; i++ {
		note(i)
	}
	// 30s of overlap at ten entries per second, plus the slack of pruning
	// lazily.
	if len(r.recent) < 301 || len(r.recent) > 3000 {
		t.Errorf("%d entries remembered, want about the overlap", len(r.recent))
	}
	since, all, seen := r.begin()
	want := t0.Add(19999 * 100 * time.Millisecond).Add(-resumeOverlap)
	if all || !since.Equal(want) || len(seen) != 301 {
		t.Errorf("begin = %v, all=%v, %d seen; want %v, false, 301", since, all, len(seen), want)
	}

	// While a stream is open, everything from its start is remembered.
	r.open, r.floor = true, since
	for i := 20000; i < 40000; i++ {
		note(i)
	}
	again, all, seen := r.begin()
	if all || !again.Equal(since) || len(seen) != 301+20000 {
		t.Errorf("open stream: begin = %v, all=%v, %d seen; want %v, false, %d", again, all, len(seen), since, 301+20000)
	}
	r.open = false
	for i := 40000; i < 60000; i++ {
		note(i)
	}
	if len(r.recent) > 3000 {
		t.Errorf("%d entries remembered after the stream settled", len(r.recent))
	}

	// Nothing written yet: start from the beginning.
	var empty resume
	if _, all, seen := empty.begin(); !all || len(seen) != 0 {
		t.Errorf("empty state: all=%v, %d seen", all, len(seen))
	}
}
