package logs

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultSearchLimit applies when Query.Limit is not positive.
	DefaultSearchLimit = 1000
	// MaxSearchLimit caps Query.Limit and the n of Tail.
	MaxSearchLimit = 10000
)

type subscriber struct {
	ch   chan Line
	once sync.Once
}

// Subscribe returns the lines of slug as they are written, for all services
// including AccessService. The channel is closed by cancel, which is safe to
// call more than once. A subscriber that does not keep up loses lines; it
// never slows down collection.
func (c *Collector) Subscribe(slug string) (<-chan Line, func()) {
	sub := &subscriber{ch: make(chan Line, c.subscriberBuffer)}
	c.subMu.Lock()
	if c.subs[slug] == nil {
		c.subs[slug] = map[*subscriber]struct{}{}
	}
	c.subs[slug][sub] = struct{}{}
	c.subMu.Unlock()

	cancel := func() {
		sub.once.Do(func() {
			c.subMu.Lock()
			delete(c.subs[slug], sub)
			if len(c.subs[slug]) == 0 {
				delete(c.subs, slug)
			}
			close(sub.ch)
			c.subMu.Unlock()
		})
	}
	return sub.ch, cancel
}

func (c *Collector) publish(slug string, lines []Line) {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	for sub := range c.subs[slug] {
		for _, line := range lines {
			select {
			case sub.ch <- line:
			default:
				// Full: this subscriber misses the line.
			}
		}
	}
}

// Services returns the services of slug that have local log files, sorted.
// Pseudo-services (leading underscore) are included.
func (c *Collector) Services(slug string) ([]string, error) {
	if err := checkSlug(slug); err != nil {
		return nil, err
	}
	return listNames(c.appDir(slug), checkService)
}

// servicesFor resolves the service filter of a read: one validated name, or
// every service of the app.
func (c *Collector) servicesFor(slug, service string) ([]string, error) {
	if err := checkSlug(slug); err != nil {
		return nil, err
	}
	if service == "" {
		return listNames(c.appDir(slug), checkService)
	}
	if err := checkService(service); err != nil {
		return nil, err
	}
	return []string{service}, nil
}

// Tail returns the last n lines of slug from the local files, oldest first.
// An empty service means all services, merged by time. n is capped at
// MaxSearchLimit.
func (c *Collector) Tail(slug, service string, n int) ([]Line, error) {
	services, err := c.servicesFor(slug, service)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, nil
	}
	if n > MaxSearchLimit {
		n = MaxSearchLimit
	}
	var all []Line
	for _, name := range services {
		lines, err := c.tailService(slug, name, n)
		if err != nil {
			return nil, err
		}
		all = append(all, lines...)
	}
	sortByTime(all)
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// tailService returns the last n lines of one service in file order.
func (c *Collector) tailService(slug, service string, n int) ([]Line, error) {
	days, err := listDays(c.serviceDir(slug, service))
	if err != nil {
		return nil, err
	}
	// newestFirst collects lines from the end of the newest day backwards.
	var newestFirst []Line
	for i := len(days) - 1; i >= 0 && len(newestFirst) < n; i-- {
		d := days[i]
		before := len(newestFirst)
		err := scanEntriesReverse(d.plainPath(), func(ts time.Time, text []byte) bool {
			newestFirst = append(newestFirst, Line{Service: service, Time: ts, Text: string(text)})
			return len(newestFirst) < n
		})
		if err != nil {
			return nil, err
		}
		// When the plain file gave nothing, the day may have been
		// compressed since the directory was listed.
		if len(newestFirst) >= n || (!d.gz && len(newestFirst) > before) {
			continue
		}
		// A gzip file can only be read forwards: keep its last lines.
		want := n - len(newestFirst)
		last, err := lastEntries(d, service, want)
		if err != nil {
			return nil, err
		}
		for j := len(last) - 1; j >= 0; j-- {
			newestFirst = append(newestFirst, last[j])
		}
	}
	for i, j := 0, len(newestFirst)-1; i < j; i, j = i+1, j-1 {
		newestFirst[i], newestFirst[j] = newestFirst[j], newestFirst[i]
	}
	return newestFirst, nil
}

// lastEntries returns the last n entries of the gzip file of d, oldest first.
func lastEntries(d dayFiles, service string, n int) ([]Line, error) {
	zr, err := openGzipOnly(d)
	if err != nil || zr == nil {
		return nil, err
	}
	defer zr.Close()
	ring := make([]Line, 0, n)
	next := 0
	err = scanEntries(context.Background(), zr, func(ts time.Time, text []byte) error {
		line := Line{Service: service, Time: ts, Text: string(text)}
		if len(ring) < n {
			ring = append(ring, line)
		} else {
			ring[next] = line
			next = (next + 1) % n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return append(ring[next:len(ring):len(ring)], ring[:next]...), nil
}

// Query selects lines for Search.
type Query struct {
	// Service limits the search to one service. Empty means all.
	Service string
	// From and To bound the time of the lines: From is inclusive, To is
	// exclusive. A zero value leaves that side open.
	From, To time.Time
	// Contains is a case-sensitive substring the text must contain. Empty
	// matches everything.
	Contains string
	// Limit is the maximum number of lines returned. Values below 1 mean
	// DefaultSearchLimit; values above MaxSearchLimit are capped.
	Limit int
}

// Result is the answer of Search.
type Result struct {
	// Lines are the oldest matching lines, oldest first.
	Lines []Line
	// Truncated reports that more lines matched than Limit allowed.
	Truncated bool
}

// Search returns the lines of slug in the local files that match q. Both
// plain and compressed day files are read.
func (c *Collector) Search(ctx context.Context, slug string, q Query) (Result, error) {
	services, err := c.servicesFor(slug, q.Service)
	if err != nil {
		return Result{}, err
	}
	limit := q.Limit
	if limit < 1 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	var fromDay, toDay string
	if !q.From.IsZero() {
		fromDay = c.day(q.From)
	}
	if !q.To.IsZero() {
		toDay = c.day(q.To)
	}
	needle := []byte(q.Contains)

	var all []Line
	truncated := false
	for _, service := range services {
		days, err := listDays(c.serviceDir(slug, service))
		if err != nil {
			return Result{}, err
		}
		// One line more than the limit per service is enough to know
		// whether the merged result is complete.
		found := 0
		for _, d := range days {
			if (fromDay != "" && d.date < fromDay) || (toDay != "" && d.date > toDay) {
				continue
			}
			if found > limit {
				break
			}
			stream, err := openDay(d)
			if err != nil {
				return Result{}, err
			}
			err = scanEntries(ctx, stream, func(ts time.Time, text []byte) error {
				if (!q.From.IsZero() && ts.Before(q.From)) || (!q.To.IsZero() && !ts.Before(q.To)) {
					return nil
				}
				if len(needle) > 0 && !bytes.Contains(text, needle) {
					return nil
				}
				all = append(all, Line{Service: service, Time: ts, Text: string(text)})
				found++
				if found > limit {
					return errStop
				}
				return nil
			})
			stream.Close()
			if err != nil {
				return Result{}, err
			}
		}
	}
	sortByTime(all)
	if len(all) > limit {
		all, truncated = all[:limit], true
	}
	return Result{Lines: all, Truncated: truncated}, nil
}

// sortByTime orders lines by time and keeps the file order of lines with the
// same timestamp.
func sortByTime(lines []Line) {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.Before(lines[j].Time) })
}
