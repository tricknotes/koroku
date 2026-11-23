package logs

import "testing"

// TestCollectRecordsColoursUnderThePublicService: the lines of the coloured
// copies of a blue/green app's public service go to the public service
// (ARCHITECTURE 7.2).
func TestCollectRecordsColoursUnderThePublicService(t *testing.T) {
	s := &script{}
	s.add(printsAndStays(
		"web-blue-1   | 2026-11-18T10:00:00Z old",
		"web-green-1  | 2026-11-18T10:00:01Z new",
		"db-1         | 2026-11-18T10:00:02Z ready",
		"",
	))
	app := App{Slug: "blog", Project: "koroku-blog", PublicService: "web"}
	f := newFixture(t, func(o *Options) { o.Runner = s.runner(); o.Apps = oneApp(app) })
	f.startRun()
	waitFor(t, "the last line", func() bool { return len(serviceContent(t, f.dir, "blog", "db")) == 1 })
	wantStrings(t, "web", dayContent(t, f.dir, "blog", "web", "2026-11-18"),
		[]string{"2026-11-18T10:00:00.000000000Z old", "2026-11-18T10:00:01.000000000Z new"})
	services, err := f.c.Services("blog")
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "Services", services, []string{"db", "web"})
}
