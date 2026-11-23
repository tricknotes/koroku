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
	// EnqueueRollback and ReleaseAvailable start a kept release again
	// without building it (ARCHITECTURE 7.1).
	EnqueueRollback(ctx context.Context, app store.App, fromID int64) (store.Deployment, error)
	ReleaseAvailable(app store.App, id int64) bool
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
	// SourceDeploymentID is set on rollbacks; RollbackAvailable says
	// whether this deployment's release can be started again.
	SourceDeploymentID *int64 `json:"source_deployment_id"`
	RollbackAvailable  bool   `json:"rollback_available"`
	// PromotedFrom names the staging deployment a promotion deployed.
	PromotedFrom *promotedFromJSON `json:"promoted_from"`
	// Actor is who asked for the deployment; null when nobody did.
	Actor *actorJSON `json:"actor"`
	// Trigger is manual or push (ARCHITECTURE 5.2); Pusher is the GitHub
	// login behind a push, "" otherwise.
	Trigger string `json:"trigger"`
	Pusher  string `json:"pusher"`
	// ContainersReplaced says whether the run touched the running
	// containers (ARCHITECTURE 9.5); null when the run did not end normally.
	ContainersReplaced *bool `json:"containers_replaced"`
}

// actorJSON names the user who started an operation.
type actorJSON struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func actorToJSON(a *store.Actor) *actorJSON {
	if a == nil {
		return nil
	}
	return &actorJSON{ID: a.UserID, Login: a.Login}
}

type promotedFromJSON struct {
	DeploymentID int64  `json:"deployment_id"`
	AppSlug      string `json:"app_slug"`
}

// deploymentToJSON is the package-level deploymentToJSON with what needs
// the deployer and the database: whether the release can be started again
// (ARCHITECTURE 7.1) and the promotion source (ARCHITECTURE 9.7).
func (s *Server) deploymentToJSON(ctx context.Context, app store.App, d store.Deployment) deploymentJSON {
	j := deploymentToJSON(d, app.Slug)
	j.RollbackAvailable = d.Status == store.StatusSucceeded && s.deployer.ReleaseAvailable(app, deploy.RollbackTarget(d))
	if d.PromotedFromID == nil {
		return j
	}
	source, err := s.store.GetDeployment(ctx, *d.PromotedFromID)
	if err != nil {
		return j
	}
	sourceApp, err := s.store.GetAppByID(ctx, source.AppID)
	if err != nil {
		return j
	}
	j.PromotedFrom = &promotedFromJSON{DeploymentID: source.ID, AppSlug: sourceApp.Slug}
	return j
}

func deploymentToJSON(d store.Deployment, slug string) deploymentJSON {
	return deploymentJSON{
		SourceDeploymentID: d.SourceDeploymentID,
		ID:                 d.ID,
		AppSlug:            slug,
		Ref:                d.Ref,
		CommitSHA:          d.CommitSHA,
		CommitMessage:      d.CommitMessage,
		Status:             d.Status,
		Error:              d.Error,
		QueuedAt:           d.QueuedAt,
		StartedAt:          d.StartedAt,
		FinishedAt:         d.FinishedAt,
		Actor:              actorToJSON(d.Actor),
		Trigger:            d.Trigger,
		Pusher:             d.Pusher,
		ContainersReplaced: d.ContainersReplaced,
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
	var list []store.Deployment
	var err error
	if raw := r.URL.Query().Get("finished_since"); raw != "" {
		// The deployments within the memory history's range (ARCHITECTURE 9.5).
		since, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "invalid_finished_since", "finished_since must be an RFC 3339 time")
			return
		}
		list, err = s.store.ListDeploymentsFinishedSince(r.Context(), app.ID, since, limit)
	} else {
		list, err = s.store.ListDeployments(r.Context(), app.ID, limit)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]deploymentJSON, 0, len(list))
	for _, d := range list {
		out = append(out, s.deploymentToJSON(r.Context(), app, d))
	}
	var pending *pendingAutoDeployJSON
	if p, err := s.store.GetPendingAutoDeploy(r.Context(), app.ID); err == nil {
		pending = &pendingAutoDeployJSON{
			Ref: p.Ref, CommitSHA: p.CommitSHA, CommitMessage: p.CommitMessage,
			Pusher: p.Pusher, Workflow: p.Workflow, ReceivedAt: p.ReceivedAt,
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": out, "pending_auto_deploy": pending})
}

// pendingAutoDeployJSON is an automatic deployment kept until the command
// running on the app ends (ARCHITECTURE 5.2).
type pendingAutoDeployJSON struct {
	Ref           string    `json:"ref"`
	CommitSHA     string    `json:"commit_sha"`
	CommitMessage string    `json:"commit_message"`
	Pusher        string    `json:"pusher"`
	Workflow      string    `json:"workflow"`
	ReceivedAt    time.Time `json:"received_at"`
}

type createDeploymentInput struct {
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	RollbackTo *int64 `json:"rollback_to"`
	// PromoteFromDeployment promotes the source app's current release
	// (ARCHITECTURE 9.7) instead of deploying ref or sha.
	PromoteFromDeployment *int64 `json:"promote_from_deployment"`
}

// handleCreateDeployment starts a deployment of {sha}, or of the commit that
// {ref} points at right now. Rolling back is deploying an older SHA.
func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if app.Environment == store.EnvironmentProduction && !s.requireRecentAuth(w, r) {
		return
	}
	var in createDeploymentInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Ref, in.SHA = strings.TrimSpace(in.Ref), strings.ToLower(strings.TrimSpace(in.SHA))
	if in.RollbackTo != nil {
		s.createRollback(w, r, app, in)
		return
	}

	if in.PromoteFromDeployment != nil {
		if in.Ref != "" || in.SHA != "" {
			writeValidationError(w, fieldErrors{"promote_from_deployment": "cannot be combined with ref or sha"})
			return
		}
		s.handlePromote(w, r, app, *in.PromoteFromDeployment)
		return
	}

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
	if s.writeEnqueueError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, s.deploymentToJSON(r.Context(), app, d))
}

// writeEnqueueError answers a failed enqueue and reports whether it did.
func (s *Server) writeEnqueueError(w http.ResponseWriter, r *http.Request, err error) bool {
	var diskFull *deploy.DiskFullError
	switch {
	case errors.Is(err, deploy.ErrBusy):
		writeError(w, http.StatusConflict, "busy", "a command is running on the app; wait for it or cancel it")
		return true
	case errors.As(err, &diskFull):
		writeJSON(w, http.StatusInsufficientStorage, errorBody{Error: errorDetail{
			Code:    "disk_full",
			Message: diskFull.Error(),
			Details: map[string]any{"used_percent": diskFull.UsedPercent, "limit_percent": diskFull.LimitPercent},
		}})
		return true
	case err != nil:
		s.internalError(w, r, err)
		return true
	}
	return false
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
	writeJSON(w, http.StatusOK, s.deploymentToJSON(r.Context(), app, d))
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
	writeJSON(w, http.StatusAccepted, s.deploymentToJSON(r.Context(), app, current))
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
	serveJobLog(w, r, s.deployer.LogPath(app.Slug, d.ID), func(ctx context.Context) (string, bool, error) {
		current, err := s.store.GetDeployment(ctx, d.ID)
		return current.Status, store.IsTerminal(current.Status), err
	})
}

// serveJobLog streams the log file of a job (a deployment, a database
// copy) as Server-Sent Events, or as plain text with ?format=text. status
// reports the job's status and whether it has ended.
func serveJobLog(w http.ResponseWriter, r *http.Request, path string,
	status func(context.Context) (string, bool, error)) {
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
		// Read the status before draining: a job that finishes between
		// the two is caught by the next round.
		current, ended, err := status(ctx)
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
		if ended && !wrote {
			_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", current)
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
		// A carriage return ends a line in the SSE format, so one left in a
		// log line could smuggle in fields such as "event: end".
		text := strings.ReplaceAll(strings.TrimRight(line, "\r\n"), "\r", " ")
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", next, text); err != nil {
			return next, wrote, err
		}
		wrote = true
	}
}

// createRollback queues a rollback to a kept release (ARCHITECTURE 7.1).
func (s *Server) createRollback(w http.ResponseWriter, r *http.Request, app store.App, in createDeploymentInput) {
	if in.Ref != "" || in.SHA != "" || in.PromoteFromDeployment != nil {
		writeValidationError(w, fieldErrors{"rollback_to": "cannot be combined with ref, sha or promote_from_deployment"})
		return
	}
	d, err := s.deployer.EnqueueRollback(r.Context(), app, *in.RollbackTo)
	switch {
	case errors.Is(err, deploy.ErrBusy):
		writeError(w, http.StatusConflict, "busy", "a command is running on the app; wait for it or cancel it")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, deploy.ErrOtherApp):
		writeValidationError(w, fieldErrors{"rollback_to": "no such deployment of this app"})
	case errors.Is(err, deploy.ErrReleaseUnavailable):
		writeError(w, http.StatusUnprocessableEntity, "release_unavailable",
			"the release of this deployment is not kept any more; deploy its commit again instead")
	case err != nil:
		s.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusAccepted, s.deploymentToJSON(r.Context(), app, d))
	}
}
