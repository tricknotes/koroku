package logs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A day file that was damaged from outside (a huge line, bytes that are not
// entries) must not stop the readers or the archiver.
func TestDamagedDayFile(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Set(at(18, 12, 0))
	dir := filepath.Join(f.dir, "blog", "web")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("\x00\xffgarbage ", 150_000) // 1.35 MB without a newline
	content := "2026-11-18T01:00:00.000000000Z first\n" +
		huge + "\n" +
		"no timestamp here\n" +
		"\n" +
		"2026-11-18T02:00:00.000000000Z second\n" +
		"2026-11-18T03:00:00.000000000Z " + strings.Repeat("y", 70_000) + "\n" +
		"2026-11-18T04:00:00.000000000Z last\n"
	path := filepath.Join(dir, "2026-11-18.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	lines, err := f.c.Tail("blog", "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 || lines[0].Text != "first" || lines[1].Text != "second" || len(lines[2].Text) != 70_000 || lines[3].Text != "last" {
		t.Errorf("Tail returned %d lines: %q ...", len(lines), lineTexts(lines)[:min(2, len(lines))])
	}
	res, err := f.c.Search(context.Background(), "blog", Query{Contains: "s"})
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "Search", lineTexts(res.Lines), []string{"first", "second", "last"})

	// The archive is a copy of the bytes, damage included.
	store := newFlakyStore()
	a := newArchiver(t, f, store, nil)
	// The file's last entry, 04:00 UTC, is 13:00 in the fixture's location.
	mustArchive(t, a, at(18, 16, 10))
	got := strings.Join(objectLines(t, store, "logs/blog/web/2026/11/18/12.log.gz"), "\n") + "\n"
	if got != content {
		t.Errorf("archived %d bytes, want the %d bytes of the file", len(got), len(content))
	}

	// New lines are still appended and found, and the day compresses.
	f.put("blog", "web", at(18, 14, 0), "after the damage")
	f.clock.Set(at(19, 1, 0))
	mustMaintain(t, f.c)
	lines, err = f.c.Tail("blog", "web", 2)
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "Tail of the compressed day", lineTexts(lines), []string{"last", "after the damage"})
}
