package cron

import (
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s is not available: %v", name, err)
	}
	return loc
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"", "needs 5 fields"},
		{"* * * *", "got 4"},
		{"* * * * * *", "got 6"},
		{"60 * * * *", "minute: 60 is out of range 0-59"},
		{"* 24 * * *", "hour: 24 is out of range 0-23"},
		{"* * 0 * *", "day of month: 0 is out of range 1-31"},
		{"* * 32 * *", "day of month: 32 is out of range 1-31"},
		{"* * * 13 *", "month: 13 is out of range 1-12"},
		{"* * * 0 *", "month: 0 is out of range 1-12"},
		{"* * * * 8", "day of week: 8 is out of range 0-7"},
		{"*/0 * * * *", `step "0" must be a whole number`},
		{"*/x * * * *", `step "x" must be`},
		{"5/10 * * * *", "a step needs * or a range"},
		{"10-5 * * * *", "range 10-5 goes backwards"},
		{"1,,2 * * * *", "empty item"},
		{"a * * * *", `"a" is not a number`},
		{"+5 * * * *", `"+5" is not a number`},
		{"* * * JAN *", `"JAN" is not a number`},
		{"* * * * MON", `"MON" is not a number`},
		{"@daily", "needs 5 fields"},
		{"1-2-3 * * * *", `"2-3" is not a number`},
		{strings.Repeat("1,", 60) + "1 * * * *", "at most 100 characters"},
	}
	for _, tt := range tests {
		_, err := Parse(tt.expr)
		if err == nil {
			t.Errorf("Parse(%q) succeeded", tt.expr)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Parse(%q) = %v, want it to mention %q", tt.expr, err, tt.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("  0   3 * *\t* "); got != "0 3 * * *" {
		t.Errorf("Normalize = %q", got)
	}
}

// runs lists the next n runs after from.
func runs(t *testing.T, expr string, from time.Time, loc *time.Location, n int) []string {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	var out []string
	at := from
	for range n {
		at = s.Next(at, loc)
		if at.IsZero() {
			out = append(out, "never")
			break
		}
		out = append(out, at.In(loc).Format("2006-01-02 15:04 Mon"))
	}
	return out
}

func TestNext(t *testing.T) {
	tokyo := mustLoad(t, "Asia/Tokyo")
	// Wednesday 2026-11-18 10:07:30 in Tokyo.
	from := time.Date(2026, 11, 18, 10, 7, 30, 0, tokyo)
	tests := []struct {
		expr string
		want []string
	}{
		{"* * * * *", []string{"2026-11-18 10:08 Wed", "2026-11-18 10:09 Wed"}},
		{"*/15 * * * *", []string{"2026-11-18 10:15 Wed", "2026-11-18 10:30 Wed", "2026-11-18 10:45 Wed", "2026-11-18 11:00 Wed"}},
		{"0 3 * * *", []string{"2026-11-19 03:00 Thu", "2026-11-20 03:00 Fri"}},
		{"7 10 * * *", []string{"2026-11-19 10:07 Thu"}},
		{"8 10 * * *", []string{"2026-11-18 10:08 Wed", "2026-11-19 10:08 Thu"}},
		{"0,30 9-17/4 * * *", []string{"2026-11-18 13:00 Wed", "2026-11-18 13:30 Wed", "2026-11-18 17:00 Wed", "2026-11-18 17:30 Wed", "2026-11-19 09:00 Thu"}},
		{"0 9 * * 1-5", []string{"2026-11-19 09:00 Thu", "2026-11-20 09:00 Fri", "2026-11-23 09:00 Mon"}},
		// 0 and 7 are both Sunday.
		{"0 0 * * 0", []string{"2026-11-22 00:00 Sun", "2026-11-29 00:00 Sun"}},
		{"0 0 * * 7", []string{"2026-11-22 00:00 Sun", "2026-11-29 00:00 Sun"}},
		{"0 0 * * 6,7", []string{"2026-11-21 00:00 Sat", "2026-11-22 00:00 Sun", "2026-11-28 00:00 Sat"}},
		// Across the end of a month and of a year.
		{"0 0 1 * *", []string{"2026-12-01 00:00 Tue", "2027-01-01 00:00 Fri", "2027-02-01 00:00 Mon"}},
		{"0 0 31 * *", []string{"2026-12-31 00:00 Thu", "2027-01-31 00:00 Sun", "2027-03-31 00:00 Wed"}},
		{"30 23 31 12 *", []string{"2026-12-31 23:30 Thu", "2027-12-31 23:30 Fri"}},
		{"0 12 * 2 *", []string{"2027-02-01 12:00 Mon"}},
		// Both day fields restricted: either one matches.
		{"0 0 1 * 1", []string{"2026-11-23 00:00 Mon", "2026-11-30 00:00 Mon", "2026-12-01 00:00 Tue", "2026-12-07 00:00 Mon"}},
		// A day field starting with * leaves the day to the other one.
		{"0 0 */2 * 1", []string{"2026-11-23 00:00 Mon", "2026-12-07 00:00 Mon"}},
		{"0 0 1 * *", []string{"2026-12-01 00:00 Tue"}},
		// Leap days.
		{"0 0 29 2 *", []string{"2028-02-29 00:00 Tue", "2032-02-29 00:00 Sun"}},
		// Never.
		{"0 0 31 2 *", []string{"never"}},
		{"0 0 30 2 *", []string{"never"}},
		{"0 0 31 4,6,9,11 *", []string{"never"}},
	}
	for _, tt := range tests {
		got := runs(t, tt.expr, from, tokyo, len(tt.want))
		if strings.Join(got, ", ") != strings.Join(tt.want, ", ") {
			t.Errorf("%q from %s:\n got  %v\n want %v", tt.expr, from, got, tt.want)
		}
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	s, err := Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	if got := s.Next(at, nil); !got.Equal(at.AddDate(0, 0, 1)) {
		t.Errorf("Next(03:00) = %s, want the next day", got)
	}
	if got := s.Next(at.Add(-time.Nanosecond), nil); !got.Equal(at) {
		t.Errorf("Next(just before 03:00) = %s", got)
	}
}

func TestNextReadsTheZone(t *testing.T) {
	tokyo := mustLoad(t, "Asia/Tokyo")
	s, err := Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-18 12:00 UTC is 21:00 in Tokyo: 03:00 Tokyo comes at 18:00 UTC.
	got := s.Next(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC), tokyo)
	if want := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got.UTC(), want)
	}
}

func TestNextAcrossDaylightSaving(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	// 2026-03-08: 02:00 to 02:59 does not exist in New York.
	got := runs(t, "30 2 * * *", time.Date(2026, 3, 6, 12, 0, 0, 0, ny), ny, 3)
	want := []string{"2026-03-07 02:30 Sat", "2026-03-09 02:30 Mon", "2026-03-10 02:30 Tue"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("skipped hour: got %v, want %v", got, want)
	}
	// Every 30 minutes keeps going across the gap.
	got = runs(t, "*/30 * * * *", time.Date(2026, 3, 8, 1, 0, 0, 0, ny), ny, 3)
	want = []string{"2026-03-08 01:30 Sun", "2026-03-08 03:00 Sun", "2026-03-08 03:30 Sun"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("hourly across the gap: got %v, want %v", got, want)
	}

	// 2026-11-01: 01:00 to 01:59 occurs twice. A run at 01:30 happens
	// once that day, also when counting starts during the repeated hour.
	s, err := Parse("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	starts := []time.Time{
		time.Date(2026, 10, 31, 12, 0, 0, 0, ny),
		time.Date(2026, 11, 1, 5, 40, 0, 0, time.UTC), // 01:40 EDT, the first 01:40
		time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC), // 01:10 EST, the second 01:10
	}
	for _, start := range starts {
		dayEnd := time.Date(2026, 11, 2, 0, 0, 0, 0, ny)
		count := 0
		for at := s.Next(start, ny); at.Before(dayEnd); at = s.Next(at, ny) {
			if at.In(ny).Day() == 1 {
				count++
			}
		}
		if count > 1 {
			t.Errorf("from %s: %d runs on the day the clocks go back", start.UTC(), count)
		}
	}
	if first := s.Next(starts[0], ny); first.In(ny).Format("15:04") != "01:30" || first.In(ny).Day() != 1 {
		t.Errorf("first run on the repeated day = %s", first.In(ny))
	}
}

func TestEverySchedulePartsCombine(t *testing.T) {
	s, err := Parse("5,10-12,20-40/10,*/30 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	var minutes []int
	for m := range 60 {
		if has(s.minute, m) {
			minutes = append(minutes, m)
		}
	}
	want := []int{0, 5, 10, 11, 12, 20, 30, 40}
	if len(minutes) != len(want) {
		t.Fatalf("minutes = %v, want %v", minutes, want)
	}
	for i := range want {
		if minutes[i] != want[i] {
			t.Fatalf("minutes = %v, want %v", minutes, want)
		}
	}
}
