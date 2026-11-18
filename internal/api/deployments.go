package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/store"
)

// Deployer is the part of the deploy engine the API drives.
type Deployer interface {
	Enqueue(ctx context.Context, app store.App, ref, sha, message string) (store.Deployment, error)
	Cancel(ctx context.Context, id int64) error
	LogPath(slug string, id int64) string
}

// RefResolver turns a branch or tag of an app's repository into a commit.
type RefResolver interface {
	ResolveRef(ctx context.Context, app store.App, ref string) (sha, message string, err error)
}

// Router reloads the reverse proxy configuration from the database.
type Router interface {
	Sync(ctx context.Context) error
}

type deploymentJSON struct {
	ID            int64      `json:"id"`
	AppSlug       string     `json:"app_slug"`
	Ref           string     `json:"ref"`
	CommitSHA     string     `json:"commit_sha"`
	CommitMessage string     `json:"commit_message"`
	Status        string     `json:"status"`
	Error         string     `json:"error"`
	QueuedAt      time.Time  `json:"queued_at"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

func deploymentToJSON(d store.Deployment, slug string) deploymentJSON {
	return deploymentJSON{
		ID:            d.ID,
		AppSlug:       slug,
		Ref:           d.Ref,
		CommitSHA:     d.CommitSHA,
		CommitMessage: d.CommitMessage,
		Status:        d.Status,
		Error:         d.Error,
		QueuedAt:      d.QueuedAt,
		StartedAt:     d.StartedAt,
		FinishedAt:    d.FinishedAt,
	}
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	list, err := s.store.ListDeployments(r.Context(), app.ID, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]deploymentJSON, 0, len(list))
	for _, d := range list {
		out = append(out, deploymentToJSON(d, app.Slug))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": out})
}

type createDeploymentInput struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// handleCreateDeployment starts a deployment of {sha}, or of the commit that
// {ref} points at right now. Rolling back is deploying an older SHA.
func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in createDeploymentInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Ref, in.SHA = strings.TrimSpace(in.Ref), strings.ToLower(strings.TrimSpace(in.SHA))

	errs := fieldErrors{}
	if in.SHA != "" && !gitrepo.IsFullSHA(in.SHA) {
		errs.add("sha", "must be a full commit SHA")
	}
	if in.Ref != "" {
		if err := gitrepo.ValidateRef(in.Ref); err != nil {
			errs.add("ref", "is not a valid branch or tag name")
		}
	}
	if in.SHA == "" && in.Ref == "" {
		errs.add("ref", "ref or sha is required")
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}

	sha, message := in.SHA, ""
	if sha == "" {
		if s.refs == nil {
			writeError(w, http.StatusUnprocessableEntity, "ref_unresolvable", "resolving refs is not available; pass a sha")
			return
		}
		resolveCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		var err error
		sha, message, err = s.refs.ResolveRef(resolveCtx, app, in.Ref)
		if err != nil {
			s.log.Warn("resolving ref failed", "app", app.Slug, "error", err)
			writeError(w, http.StatusUnprocessableEntity, "ref_unresolvable",
				fmt.Sprintf("could not resolve %q in the repository", in.Ref))
			return
		}
	}

	d, err := s.deployer.Enqueue(r.Context(), app, in.Ref, sha, message)
	var diskFull *deploy.DiskFullError
	switch {
	case errors.As(err, &diskFull):
		writeJSON(w, http.StatusInsufficientStorage, errorBody{Error: errorDetail{
			Code:    "disk_full",
			Message: diskFull.Error(),
			Details: map[string]any{"used_percent": diskFull.UsedPercent, "limit_percent": diskFull.LimitPercent},
		}})
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, deploymentToJSON(d, app.Slug))
}

// loadDeployment resolves the {id} path value together with its app.
func (s *Server) loadDeployment(w http.ResponseWriter, r *http.Request) (store.Deployment, store.App, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, "deployment_not_found", "no such deployment")
		return store.Deployment{}, store.App{}, false
	}
	d, err := s.store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment_not_found", "no such deployment")
		return store.Deployment{}, store.App{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.Deployment{}, store.App{}, false
	}
	app, err := s.store.GetAppByID(r.Context(), d.AppID)
	if err != nil {
		s.internalError(w, r, err)
		return store.Deployment{}, store.App{}, false
	}
	return d, app, true
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	d, app, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, deploymentToJSON(d, app.Slug))
}

func (s *Server) handleCancelDeployment(w http.ResponseWriter, r *http.Request) {
	d, app, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	err := s.deployer.Cancel(r.Context(), d.ID)
	switch {
	case errors.Is(err, deploy.ErrNotCancelable):
		writeError(w, http.StatusConflict, "not_cancelable", "the deployment has already finished")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	// A running deployment stops asynchronously; report the current state.
	current, err := s.store.GetDeployment(r.Context(), d.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, deploymentToJSON(current, app.Slug))
}

// logPollInterval is how often a live log stream checks for new output.
var logPollInterval = 300 * time.Millisecond

const logHeartbeatInterval = 15 * time.Second

// handleDeploymentLogs streams the deploy log.
//
// By default it is a Server-Sent Events stream: every log line is one
// message whose id is the byte offset after the line, so a reconnecting
// EventSource resumes with Last-Event-ID. When the deployment has finished
// and the log is drained, an "end" event carries the final status.
// With ?format=text the log so far is returned as plain text.
func (s *Server) handleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	d, app, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	path := s.deployer.LogPath(app.Slug, d.ID)

	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if f, err := os.Open(path); err == nil {
			defer func() { _ = f.Close() }()
			_, _ = io.Copy(w, f)
		}
		return
	}

	offset := int64(0)
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			offset = n
		}
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	ctx := r.Context()
	lastWrite := time.Now()
	for {
		// Read the status before draining: a deployment that finishes
		// between the two is caught by the next round.
		current, err := s.store.GetDeployment(ctx, d.ID)
		if err != nil {
			return
		}
		next, wrote, err := streamLogLines(w, path, offset)
		if err != nil {
			return
		}
		offset = next
		if wrote {
			lastWrite = time.Now()
			_ = rc.Flush()
		}
		if store.IsTerminal(current.Status) && !wrote {
			_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", current.Status)
			_ = rc.Flush()
			return
		}
		if time.Since(lastWrite) >= logHeartbeatInterval {
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			_ = rc.Flush()
			lastWrite = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(logPollInterval):
		}
	}
}

// streamLogLines writes the complete lines found after offset as SSE
// messages and returns the offset after the last one. A trailing partial
// line is left for the next call.
func streamLogLines(w io.Writer, path string, offset int64) (next int64, wrote bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return offset, false, nil
	}
	if err != nil {
		return offset, false, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, false, err
	}
	reader := bufio.NewReader(f)
	next = offset
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// io.EOF with a partial line: wait until it is complete.
			return next, wrote, nil
		}
		next += int64(len(line))
		text := strings.TrimRight(line, "\r\n")
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", next, text); err != nil {
			return next, wrote, err
		}
		wrote = true
	}
}
