package logs

import (
	"testing"
	"time"
)

// TestCollectDropsLinesComposePrintsTwice: `docker compose logs --follow`
// started while a container comes up can attach to it twice and then prints
// each of its lines twice. Only one copy may be stored, also when the stream
// is resumed later.
func TestCollectDropsLinesComposePrintsTwice(t *testing.T) {
	base := t0.Add(408282315 * time.Nanosecond)
	twice := func(lines ...string) []string {
		var out []string
		for _, line := range lines {
			out = append(out, line, line)
		}
		return out
	}
	first := []string{
		composeLine("web-1", base, "booted"),
		composeLine("web-1", base.Add(time.Second), "tick 1"),
		// Another replica printing the same text at the same instant is
		// not an echo.
		composeLine("web-2", base.Add(time.Second), "tick 1"),
		// The same text again, later: not an echo either.
		composeLine("web-1", base.Add(2*time.Second), "tick 1"),
	}
	s := &script{}
	s.add(
		prints(twice(first...)...),
		// The resumed stream starts inside the overlap and doubles as well.
		printsAndStays(twice(append(first, composeLine("web-1", base.Add(3*time.Second), "tick 2"))...)...),
	)
	f := newFixture(t, func(o *Options) { o.Runner = s.runner(); o.Apps = oneApp(blog) })
	f.startRun()
	waitFor(t, "the line after the restart", func() bool {
		return len(serviceContent(t, f.dir, "blog", "web")) >= 5
	})
	wantStrings(t, "stored lines", texts(serviceContent(t, f.dir, "blog", "web")),
		[]string{"booted", "tick 1", "tick 1", "tick 1", "tick 2"})
}

func TestReplayFilterForgetsOldLines(t *testing.T) {
	filter := replayFilter{}
	if filter.printedBefore("web-1", 1) {
		t.Fatal("a first line was taken for an echo")
	}
	if !filter.printedBefore("web-1", 1) {
		t.Fatal("the echo of a line was not recognised")
	}
	if filter.printedBefore("web-2", 1) {
		t.Fatal("a line of another container was taken for an echo")
	}
	// Push key 1 out of the window of web-1.
	for i := range replayWindow {
		if filter.printedBefore("web-1", uint64(1000+i)) {
			t.Fatalf("new key %d was taken for an echo", 1000+i)
		}
	}
	if filter.printedBefore("web-1", 1) {
		t.Error("a key outside the window is still remembered")
	}
	if got := len(filter["web-1"].ring); got != replayWindow {
		t.Errorf("ring holds %d keys, want %d", got, replayWindow)
	}
	if got := len(filter["web-1"].count); got != replayWindow {
		t.Errorf("count holds %d keys, want %d", got, replayWindow)
	}
	// The newest keys are still recognised.
	if !filter.printedBefore("web-1", uint64(1000+replayWindow-1)) {
		t.Errorf("key %d inside the window was forgotten", 1000+replayWindow-1)
	}
}
