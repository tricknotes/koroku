// Package cron parses the five-field schedules of registered tasks
// (ARCHITECTURE 9.6) and finds their next run times.
//
// A schedule has the fields minute, hour, day of month, month and day of
// week, separated by spaces. Each field is a comma separated list of `*`,
// a number, a range `a-b`, or a step `*/n` or `a-b/n`. Numbers only: names
// (JAN, MON) and shorthands (@daily) are not accepted. Day of week runs from
// 0 to 7, both 0 and 7 being Sunday. When both the day of month and the day
// of week are restricted (neither starts with `*`), a day matches if either
// does, as in cron.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// searchYears bounds the search for the next run. A schedule on February
// 29th waits at most eight years (1896 to 1904, 2096 to 2104).
const searchYears = 9

// MaxLength is the longest schedule accepted.
const MaxLength = 100

// Schedule is a parsed schedule.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	// domStar and dowStar are set when the field starts with `*`: the
	// day matches on the other field alone.
	domStar, dowStar bool
}

type field struct {
	name     string
	min, max int
}

var fields = [5]field{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 7},
}

// Normalize collapses the whitespace of a schedule, the form it is stored
// in.
func Normalize(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}

// Parse reads a schedule. Its errors describe the problem in a sentence
// that can be shown to the user.
func Parse(expr string) (Schedule, error) {
	if len(expr) > MaxLength {
		return Schedule{}, fmt.Errorf("must be at most %d characters", MaxLength)
	}
	parts := strings.Fields(expr)
	if len(parts) != len(fields) {
		return Schedule{}, fmt.Errorf("needs 5 fields (minute hour day-of-month month day-of-week) separated by spaces, got %d", len(parts))
	}
	var bits [5]uint64
	for i, part := range parts {
		b, err := parseField(part, fields[i])
		if err != nil {
			return Schedule{}, fmt.Errorf("%s: %w", fields[i].name, err)
		}
		bits[i] = b
	}
	s := Schedule{
		minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4],
		domStar: strings.HasPrefix(parts[2], "*"),
		dowStar: strings.HasPrefix(parts[4], "*"),
	}
	// Sunday is both 0 and 7.
	if s.dow&(1<<7) != 0 {
		s.dow |= 1
	}
	s.dow &^= 1 << 7
	return s, nil
}

func parseField(text string, f field) (uint64, error) {
	var bits uint64
	for _, item := range strings.Split(text, ",") {
		b, err := parseItem(item, f)
		if err != nil {
			return 0, err
		}
		bits |= b
	}
	return bits, nil
}

func parseItem(item string, f field) (uint64, error) {
	if item == "" {
		return 0, errors.New("has an empty item in its list")
	}
	rangePart, stepPart, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepPart)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("step %q must be a whole number of at least 1", stepPart)
		}
		step = n
	}
	lo, hi := f.min, f.max
	switch {
	case rangePart == "*":
	case strings.Contains(rangePart, "-"):
		a, b, _ := strings.Cut(rangePart, "-")
		var err error
		if lo, err = number(a, f); err != nil {
			return 0, err
		}
		if hi, err = number(b, f); err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("range %s goes backwards", rangePart)
		}
	default:
		if hasStep {
			return 0, fmt.Errorf("%q: a step needs * or a range before it (*/%s, a-b/%s)", item, stepPart, stepPart)
		}
		n, err := number(rangePart, f)
		if err != nil {
			return 0, err
		}
		lo, hi = n, n
	}
	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, nil
}

func number(text string, f field) (int, error) {
	n, err := strconv.Atoi(text)
	if err != nil || strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("%d is out of range %d-%d", n, f.min, f.max)
	}
	return n, nil
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }

// dayMatches applies the cron rule for the two day fields to a date.
func (s Schedule) dayMatches(day int, weekday time.Weekday) bool {
	dom, dow := has(s.dom, day), has(s.dow, int(weekday))
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}

// Next returns the first run strictly after t, reading the schedule on the
// wall clock of loc (nil means UTC). A wall clock time that does not exist
// on a day (the clocks go forward) is skipped; one that occurs twice (the
// clocks go back) runs once, at its first occurrence. It returns the
// zero time for a schedule that never runs (February 31st).
func (s Schedule) Next(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	// w walks the wall clock, kept as a UTC time so that adding a minute
	// or a day never meets a time zone transition.
	w := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, time.UTC).
		Add(time.Minute)
	end := w.AddDate(searchYears, 0, 0)
	for w.Before(end) {
		switch {
		case !has(s.month, int(w.Month())):
			w = time.Date(w.Year(), w.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayMatches(w.Day(), w.Weekday()):
			w = time.Date(w.Year(), w.Month(), w.Day()+1, 0, 0, 0, 0, time.UTC)
		case !has(s.hour, w.Hour()):
			w = time.Date(w.Year(), w.Month(), w.Day(), w.Hour()+1, 0, 0, 0, time.UTC)
		case !has(s.minute, w.Minute()):
			w = w.Add(time.Minute)
		default:
			at := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, loc)
			shown := at.In(loc)
			// A time that does not exist moves to another hour; a time
			// that occurs twice is taken at its first occurrence, so its
			// second one, if t lies between the two, is not a new run.
			exists := shown.Day() == w.Day() && shown.Hour() == w.Hour() && shown.Minute() == w.Minute()
			if exists && at.After(t) {
				return at
			}
			w = w.Add(time.Minute)
		}
	}
	return time.Time{}
}
