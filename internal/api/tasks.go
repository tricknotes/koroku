package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// TaskRunner runs one-off commands in an app's services (ARCHITECTURE 9.6).
type TaskRunner interface {
	RunTask(ctx context.Context, app store.App, spec deploy.TaskSpec) (store.TaskRun, error)
	CancelTask(ctx context.Context, id int64) error
	TaskLogPath(slug string, id int64) string
}

// Limits of registered tasks and commands.
const (
	maxTasks          = 20
	maxTaskNameRunes  = 60
	maxTaskCommandLen = 2000
)

type taskJSON struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Command string `json:"command"`
}

type taskRunJSON struct {
	ID           int64      `json:"id"`
	AppSlug      string     `json:"app_slug"`
	TaskName     string     `json:"task_name"`
	Service      string     `json:"service"`
	Command      string     `json:"command"`
	DeploymentID *int64     `json:"deployment_id"`
	Status       string     `json:"status"`
	ExitCode     *int       `json:"exit_code"`
	Error        string     `json:"error"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	Actor        *actorJSON `json:"actor"`
}

func taskRunToJSON(run store.TaskRun, slug string) taskRunJSON {
	return taskRunJSON{
		ID:           run.ID,
		AppSlug:      slug,
		TaskName:     run.TaskName,
		Service:      run.Service,
		Command:      run.Command,
		DeploymentID: run.DeploymentID,
		Status:       run.Status,
		ExitCode:     run.ExitCode,
		Error:        run.Error,
		StartedAt:    run.StartedAt,
		FinishedAt:   run.FinishedAt,
		Actor:        actorToJSON(run.Actor),
	}
}

func tasksToJSON(tasks []store.Task) []taskJSON {
	out := make([]taskJSON, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskJSON{Name: t.Name, Service: t.Service, Command: t.Command})
	}
	return out
}

// validateCommand checks a command to run with `sh -c`. It is one line:
// the log shows it as the first line of the output.
func validateCommand(command string) string {
	switch {
	case command == "":
		return "is required"
	case len(command) > maxTaskCommandLen:
		return fmt.Sprintf("must be at most %d characters", maxTaskCommandLen)
	case strings.ContainsAny(command, "\n\r\x00"), !utf8.ValidString(command):
		return "must be a single line of text"
	}
	return ""
}

func validateTaskName(name string) string {
	switch {
	case name == "":
		return "is required"
	case !utf8.ValidString(name), utf8.RuneCountInString(name) > maxTaskNameRunes:
		return fmt.Sprintf("must be at most %d characters", maxTaskNameRunes)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "must not contain control characters"
	}
	return ""
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	tasks, err := s.store.ListTasks(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasksToJSON(tasks)})
}

// handlePutTasks replaces the registered tasks. Changing what a production
// app runs with one click needs a recent login, like running it does.
func (s *Server) handlePutTasks(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in struct {
		Tasks []taskJSON `json:"tasks"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	errs := fieldErrors{}
	if in.Tasks == nil {
		errs.add("tasks", "is required")
	}
	if len(in.Tasks) > maxTasks {
		errs.add("tasks", fmt.Sprintf("at most %d tasks can be registered", maxTasks))
	}
	tasks := make([]store.Task, 0, len(in.Tasks))
	seen := map[string]bool{}
	for i, t := range in.Tasks {
		field := func(name string) string { return fmt.Sprintf("tasks[%d].%s", i, name) }
		task := store.Task{
			Name:    strings.TrimSpace(t.Name),
			Service: strings.TrimSpace(t.Service),
			Command: strings.TrimSpace(t.Command),
		}
		if msg := validateTaskName(task.Name); msg != "" {
			errs.add(field("name"), msg)
		} else if seen[task.Name] {
			errs.add(field("name"), "is used by another task")
		}
		seen[task.Name] = true
		if !servicePattern.MatchString(task.Service) {
			errs.add(field("service"), "is not a valid service name")
		}
		if msg := validateCommand(task.Command); msg != "" {
			errs.add(field("command"), msg)
		}
		tasks = append(tasks, task)
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if app.Environment == store.EnvironmentProduction && !s.requireRecentAuth(w, r) {
		return
	}
	if err := s.store.ReplaceTasks(r.Context(), app.ID, tasks); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasksToJSON(tasks)})
}

func (s *Server) handleListTaskRuns(w http.ResponseWriter, r *http.Request) {
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
	runs, err := s.store.ListTaskRuns(r.Context(), app.ID, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]taskRunJSON, 0, len(runs))
	for _, run := range runs {
		out = append(out, taskRunToJSON(run, app.Slug))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// handleCreateTaskRun runs a registered task ({task}) or a command typed
// on the spot ({service, command}).
func (s *Server) handleCreateTaskRun(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "running commands is not available")
		return
	}
	var in struct {
		Task    *string `json:"task"`
		Service *string `json:"service"`
		Command *string `json:"command"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	errs := fieldErrors{}
	var spec deploy.TaskSpec
	switch {
	case in.Task != nil && (in.Service != nil || in.Command != nil):
		errs.add("task", "give either task or service and command, not both")
	case in.Task != nil:
		spec.TaskName = strings.TrimSpace(*in.Task)
		if spec.TaskName == "" {
			errs.add("task", "is required")
		}
	default:
		if in.Service != nil {
			spec.Service = strings.TrimSpace(*in.Service)
		}
		if in.Command != nil {
			spec.Command = strings.TrimSpace(*in.Command)
		}
		if !servicePattern.MatchString(spec.Service) {
			errs.add("service", "is not a valid service name")
		}
		if msg := validateCommand(spec.Command); msg != "" {
			errs.add("command", msg)
		}
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if spec.TaskName != "" {
		task, err := s.store.GetTask(r.Context(), app.ID, spec.TaskName)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "task_not_found", "no such task")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		spec.Service, spec.Command = task.Service, task.Command
	}
	if app.Environment == store.EnvironmentProduction && !s.requireRecentAuth(w, r) {
		return
	}

	run, err := s.tasks.RunTask(r.Context(), app, spec)
	var unknown *deploy.UnknownServiceError
	if errors.As(err, &unknown) {
		writeValidationError(w, fieldErrors{"service": "is not a service of the running release"})
		return
	}
	if s.writeOpError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, taskRunToJSON(run, app.Slug))
}

// loadTaskRun resolves the {id} path value together with its app.
func (s *Server) loadTaskRun(w http.ResponseWriter, r *http.Request) (store.TaskRun, store.App, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, "task_run_not_found", "no such task run")
		return store.TaskRun{}, store.App{}, false
	}
	run, err := s.store.GetTaskRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "task_run_not_found", "no such task run")
		return store.TaskRun{}, store.App{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.TaskRun{}, store.App{}, false
	}
	app, err := s.store.GetAppByID(r.Context(), run.AppID)
	if err != nil {
		s.internalError(w, r, err)
		return store.TaskRun{}, store.App{}, false
	}
	return run, app, true
}

func (s *Server) handleGetTaskRun(w http.ResponseWriter, r *http.Request) {
	run, app, ok := s.loadTaskRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, taskRunToJSON(run, app.Slug))
}

func (s *Server) handleCancelTaskRun(w http.ResponseWriter, r *http.Request) {
	run, app, ok := s.loadTaskRun(w, r)
	if !ok {
		return
	}
	if s.tasks == nil {
		writeError(w, http.StatusConflict, "not_cancelable", "the command has already finished")
		return
	}
	err := s.tasks.CancelTask(r.Context(), run.ID)
	switch {
	case errors.Is(err, deploy.ErrNotCancelable):
		writeError(w, http.StatusConflict, "not_cancelable", "the command has already finished")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	current, err := s.store.GetTaskRun(r.Context(), run.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, taskRunToJSON(current, app.Slug))
}

// handleTaskRunLogs streams the output of a run like a deploy log.
func (s *Server) handleTaskRunLogs(w http.ResponseWriter, r *http.Request) {
	run, app, ok := s.loadTaskRun(w, r)
	if !ok {
		return
	}
	path := ""
	if s.tasks != nil {
		path = s.tasks.TaskLogPath(app.Slug, run.ID)
	}
	serveJobLog(w, r, path, func(ctx context.Context) (string, bool, error) {
		current, err := s.store.GetTaskRun(ctx, run.ID)
		return current.Status, store.IsTerminal(current.Status), err
	})
}
