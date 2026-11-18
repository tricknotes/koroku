package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strconv"
	"time"
)

// accessTail copies the access log that the reverse proxy writes for one app
// into the pseudo-service AccessService. The proxy rotates the file itself
// (rename and start a new one); a file truncated in place is handled too.
type accessTail struct {
	c   *Collector
	app App
	res resume

	file   *os.File
	offset int64
	sp     splitter
	chunk  []byte

	// catchUp is set while reading what the file already held when the
	// tail started: some of it was collected before the restart.
	catchUp bool
	since   time.Time
	all     bool
	seen    map[uint64]int

	batch []Line
	keys  []uint64
}

func (a *accessTail) run(ctx context.Context) {
	a.sp = splitter{max: a.c.maxLine, emit: a.line}
	a.chunk = make([]byte, 64<<10)
	defer a.closeFile()
	failing := false
	for {
		err := a.poll(ctx)
		switch {
		case err != nil && !failing:
			failing = true
			a.c.log.Warn("access log tail failed; retrying", "app", a.app.Slug, "error", err)
		case err == nil && failing:
			failing = false
			a.c.log.Info("access log tail recovered", "app", a.app.Slug)
		}
		if err != nil {
			// What reached the disk is unknown: start over from the files.
			a.closeFile()
			a.res.loaded = false
			a.batch, a.keys = nil, nil
		}
		if !sleep(ctx, a.c.accessPoll) {
			return
		}
	}
}

func (a *accessTail) closeFile() {
	if a.file != nil {
		_ = a.file.Close()
		a.file = nil
	}
}

// poll reads everything that was added to the access log since the last call.
func (a *accessTail) poll(ctx context.Context) error {
	if !a.res.loaded {
		isAccess := func(service string) bool { return service == AccessService }
		if err := a.res.load(ctx, a.c, a.app.Slug, isAccess, false, time.Time{}); err != nil {
			return err
		}
		a.since, a.all, a.seen = a.res.begin()
		a.catchUp = true
	}
	// One round per file: the current one, then its successors if the log
	// was rotated meanwhile.
	for round := 0; round < 4; round++ {
		if a.file == nil {
			f, err := os.Open(a.app.AccessLogPath)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					a.catchUp = false
					return nil
				}
				return fmt.Errorf("open access log: %w", err)
			}
			a.file, a.offset = f, 0
			a.sp.reset()
		}
		if err := a.drain(); err != nil {
			return err
		}
		a.catchUp = false

		open, err := a.file.Stat()
		if err != nil {
			return fmt.Errorf("stat access log: %w", err)
		}
		current, err := os.Stat(a.app.AccessLogPath)
		switch {
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("stat access log: %w", err)
		case err != nil || !os.SameFile(open, current):
			// Rotated. Take what was written before the rename, then
			// move on to the new file.
			if err := a.drain(); err != nil {
				return err
			}
			a.closeFile()
			if err != nil {
				return nil // the new file does not exist yet
			}
		case current.Size() < a.offset:
			// Truncated in place: start again from the top.
			if _, err := a.file.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewind access log: %w", err)
			}
			a.offset = 0
			a.sp.reset()
		default:
			return nil
		}
	}
	return nil
}

// drain reads the open file to its end and writes the complete lines.
func (a *accessTail) drain() error {
	for {
		n, err := a.file.Read(a.chunk)
		if n > 0 {
			a.offset += int64(n)
			_, _ = a.sp.Write(a.chunk[:n])
			if werr := a.commit(); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read access log: %w", err)
		}
	}
}

func (a *accessTail) line(raw []byte, truncated bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return
	}
	ts, ok := accessTime(raw)
	if a.catchUp && !a.all {
		// Entries from before the restart. Without a timestamp there is
		// no telling whether they were collected already.
		if !ok || ts.Before(a.since) {
			return
		}
	}
	if !ok {
		ts = a.c.clk.Now().UTC()
	}
	text := cleanText(raw, a.c.maxLine, truncated)
	key := entryKey(AccessService, ts, text)
	if a.catchUp && a.seen[key] > 0 {
		a.seen[key]--
		return
	}
	a.batch = append(a.batch, Line{Service: AccessService, Time: ts, Text: text})
	a.keys = append(a.keys, key)
}

func (a *accessTail) commit() error {
	if len(a.batch) == 0 {
		return nil
	}
	if err := a.c.write(a.app.Slug, a.batch); err != nil {
		return err
	}
	for i, line := range a.batch {
		a.res.note(line.Time, a.keys[i])
	}
	a.batch, a.keys = nil, a.keys[:0]
	return nil
}

// accessTime extracts the time of a JSON access log entry from its "ts"
// field: seconds since the epoch as a number (Caddy's default) or an
// RFC 3339 string.
func accessTime(raw []byte) (time.Time, bool) {
	var entry struct {
		TS json.RawMessage `json:"ts"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || len(entry.TS) == 0 {
		return time.Time{}, false
	}
	if entry.TS[0] == '"' {
		var s string
		if err := json.Unmarshal(entry.TS, &s); err != nil {
			return time.Time{}, false
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}, false
		}
		return ts.UTC(), true
	}
	seconds, err := strconv.ParseFloat(string(entry.TS), 64)
	if err != nil || seconds <= 0 || seconds > 1e11 {
		return time.Time{}, false
	}
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(math.Round(frac*1e9))).UTC(), true
}
