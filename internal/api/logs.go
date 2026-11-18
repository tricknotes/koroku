package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// LogLine is one line of an app's runtime log.
type LogLine struct {
	Service string    `json:"service"`
	Time    time.Time `json:"time"`
	Line    string    `json:"line"`
}

// LogQuery filters a search over the locally kept logs.
type LogQuery struct {
	Service  string
	From, To time.Time // zero means unbounded
	Contains string
	Limit    int
}

// LogArchive is one archived log object in object storage.
type LogArchive struct {
	Key     string    `json:"key"`
	Service string    `json:"service"`
	Hour    time.Time `json:"hour"`
	Size    int64     `json:"size"`
}

var (
	// ErrNotConfigured is returned by optional features that lack their
	// configuration (object storage, backups).
	ErrNotConfigured = errors.New("not configured")
	// ErrInvalidArchiveKey is returned for keys outside an app's log prefix.
	ErrInvalidArchiveKey = errors.New("invalid archive key")
)

// AppLogs gives access to collected runtime logs.
type AppLogs interface {
	// Subscribe streams new lines of an app until cancel is called.
	Subscribe(slug string) (lines <-chan LogLine, cancel func())
	// Tail returns the last n lines (service "" means all services).
	Tail(slug, service string, n int) ([]LogLine, error)
	Search(ctx context.Context, slug string, q LogQuery) (lines []LogLine, truncated bool, err error)
	ListArchives(ctx context.Context, slug, service string) ([]LogArchive, error)
	// ArchiveURL returns a short-lived download URL for an archive key.
	ArchiveURL(ctx context.Context, slug, key string) (string, error)
	// ArchiveStatus reports whether archiving is configured and how the
	// last run went.
	ArchiveStatus() LogArchiveStatus
}

const (
	defaultLogTail     = 200
	maxLogTail         = 2000
	defaultHistorySize = 1000
	maxHistorySize     = 5000
)

func (s *Server) appLogs(w http.ResponseWriter) (AppLogs, bool) {
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "log collection is not available")
		return nil, false
	}
	return s.logs, true
}

func parseService(r *http.Request) (string, bool) {
	service := r.URL.Query().Get("service")
	return service, service == "" || logServicePattern.MatchString(service)
}

func parseBoundedInt(raw string, def, max int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// handleLiveLogs streams the app's runtime log as Server-Sent Events: first
// the last `tail` lines, then new lines as they are collected. Each message
// carries one log line as JSON. The stream ends when the client disconnects.
func (s *Server) handleLiveLogs(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	logs, ok := s.appLogs(w)
	if !ok {
		return
	}
	service, valid := parseService(r)
	if !valid {
		writeValidationError(w, fieldErrors{"service": "is not a valid service name"})
		return
	}
	tail, valid := parseBoundedInt(r.URL.Query().Get("tail"), defaultLogTail, maxLogTail)
	if !valid {
		writeValidationError(w, fieldErrors{"tail": fmt.Sprintf("must be between 1 and %d", maxLogTail)})
		return
	}

	// Subscribe before reading the tail so that no line falls in between;
	// a line that appears in both is filtered out below.
	live, cancel := logs.Subscribe(app.Slug)
	defer cancel()
	recent, err := logs.Tail(app.Slug, service, tail)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var last LogLine
	send := func(line LogLine) error {
		data, err := json.Marshal(line)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		return err
	}
	for _, line := range recent {
		if err := send(line); err != nil {
			return
		}
		last = line
	}
	_ = rc.Flush()

	heartbeat := time.NewTicker(logHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case line, open := <-live:
			if !open {
				return
			}
			if service != "" && line.Service != service {
				continue
			}
			if !line.Time.After(last.Time) && coveredByTail(recent, line) {
				continue
			}
			if err := send(line); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

// coveredByTail reports whether a live line was already sent with the tail.
func coveredByTail(tail []LogLine, line LogLine) bool {
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i].Time.Before(line.Time) {
			return false
		}
		if tail[i] == line {
			return true
		}
	}
	return false
}

func (s *Server) handleLogHistory(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	logs, ok := s.appLogs(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	errs := fieldErrors{}
	service, valid := parseService(r)
	if !valid {
		errs.add("service", "is not a valid service name")
	}
	q := LogQuery{Service: service, Contains: query.Get("q")}
	if len(q.Contains) > 200 {
		errs.add("q", "is too long")
	}
	parseTime := func(field string) time.Time {
		raw := query.Get(field)
		if raw == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			errs.add(field, "must be an RFC 3339 timestamp")
		}
		return t
	}
	q.From, q.To = parseTime("from"), parseTime("to")
	if !q.From.IsZero() && !q.To.IsZero() && q.To.Before(q.From) {
		errs.add("to", "must not be before from")
	}
	if q.Limit, valid = parseBoundedInt(query.Get("limit"), defaultHistorySize, maxHistorySize); !valid {
		errs.add("limit", fmt.Sprintf("must be between 1 and %d", maxHistorySize))
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	lines, truncated, err := logs.Search(r.Context(), app.Slug, q)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if lines == nil {
		lines = []LogLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "truncated": truncated})
}

func (s *Server) handleLogArchives(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	logs, ok := s.appLogs(w)
	if !ok {
		return
	}
	service, valid := parseService(r)
	if !valid {
		writeValidationError(w, fieldErrors{"service": "is not a valid service name"})
		return
	}
	archives, err := logs.ListArchives(r.Context(), app.Slug, service)
	if errors.Is(err, ErrNotConfigured) {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "object storage is not configured")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if archives == nil {
		archives = []LogArchive{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"archives": archives})
}

func (s *Server) handleLogArchiveURL(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	logs, ok := s.appLogs(w)
	if !ok {
		return
	}
	url, err := logs.ArchiveURL(r.Context(), app.Slug, r.URL.Query().Get("key"))
	switch {
	case errors.Is(err, ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "not_configured", "object storage is not configured")
	case errors.Is(err, ErrInvalidArchiveKey):
		writeValidationError(w, fieldErrors{"key": "is not a log archive of this app"})
	case err != nil:
		s.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"url": url})
	}
}
