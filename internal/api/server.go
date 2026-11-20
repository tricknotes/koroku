// Package api implements the HTTP interface of koroku: the JSON REST API,
// SSE streams and delivery of the embedded single-page app.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/envreq"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

// Options are the dependencies of the HTTP server.
type Options struct {
	Config config.Config
	Store  *store.Store
	System system.Prober
	// Web is the built single-page app (index.html at the root). It may be
	// nil or empty when the frontend has not been built.
	Web     fs.FS
	Logger  *slog.Logger
	Version string

	Clock clock.Clock
	// Box encrypts environment variable values.
	Box      *secrets.Box
	Deployer Deployer
	// Promoter queues promotions (ARCHITECTURE 9.7). Optional.
	Promoter Promoter
	// Refs resolves branch names for deployments. Optional.
	Refs RefResolver
	// PublicFiles reads public repositories without credentials, for
	// the variable comparison when the GitHub App cannot (ARCHITECTURE
	// 9.4). Optional.
	PublicFiles envreq.Files
	// Router reloads the reverse proxy after routing changes. Optional.
	Router Router
	// Domains verifies custom domains. Optional.
	Domains DomainChecker
	// Auth configures login and request protection.
	Auth AuthOptions
	// Ops runs start/stop/restart/delete and host cleanup.
	Ops AppOperator
	// Tasks runs one-off commands. Optional.
	Tasks TaskRunner
	// Logs serves collected runtime logs. Optional.
	Logs AppLogs
	// Backups runs backups and restores. Optional.
	Backups Backups
	// Volumes lists the named volumes of an app. Optional.
	Volumes AppVolumes
	// Copier copies databases into staging apps. Optional.
	Copier DBCopier
	// RouteStatus reports the last proxy configuration load. Optional.
	RouteStatus func() RouteStatus
}

// Server routes HTTP requests.
type Server struct {
	cfg     config.Config
	store   *store.Store
	system  system.Prober
	web     fs.FS
	log     *slog.Logger
	version string

	clock    clock.Clock
	box      *secrets.Box
	deployer Deployer
	promoter Promoter
	refs     RefResolver
	router   Router
	domains  DomainChecker
	auth     AuthOptions
	ops      AppOperator
	tasks    TaskRunner
	logs     AppLogs
	backups  Backups
	volumes  AppVolumes
	copier   DBCopier
	envReqs  *envreq.Reader
	// publicFiles reads public repositories anonymously (Options.PublicFiles).
	publicFiles envreq.Files

	routeStatus func() RouteStatus

	// callbackLimiter bounds OAuth code exchanges (see handleCallback).
	callbackLimiter *rateLimiter
	// tokenLimiter bounds the requests of each API token.
	tokenLimiter *tokenLimiter
}

// New builds a Server.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.System{}
	}
	var files envreq.Files
	if opts.Auth.GitHub != nil {
		files = opts.Auth.GitHub
	}
	return &Server{
		envReqs:     envreq.NewReader(files, opts.PublicFiles),
		publicFiles: opts.PublicFiles,
		cfg:         opts.Config,
		store:       opts.Store,
		system:      opts.System,
		web:         opts.Web,
		log:         logger,
		version:     opts.Version,
		clock:       clk,
		box:         opts.Box,
		deployer:    opts.Deployer,
		promoter:    opts.Promoter,
		refs:        opts.Refs,
		router:      opts.Router,
		domains:     opts.Domains,
		auth:        opts.Auth,
		ops:         opts.Ops,
		tasks:       opts.Tasks,
		logs:        opts.Logs,
		backups:     opts.Backups,
		volumes:     opts.Volumes,
		copier:      opts.Copier,

		routeStatus: opts.RouteStatus,

		// One person logs in a few times a week; ten a minute is generous.
		callbackLimiter: &rateLimiter{limit: 10, window: time.Minute},
		tokenLimiter:    &tokenLimiter{},
	}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.recoverPanics(s.logRequests(s.securityHeaders(s.hostDispatch(s.guard(s.routes()), s.gateMux()))))
}

// routes registers every endpoint. The guard asks the same mux which route
// a request would reach, to decide what an API token may use.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /auth/github/login", s.handleLogin)
	mux.HandleFunc("GET /auth/github/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/gate", s.handleGateStart)
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/revoke-others", s.handleRevokeOtherSessions)
	mux.HandleFunc("GET /api/login-events", s.handleLoginEvents)
	mux.HandleFunc("GET /api/tokens", s.handleListTokens)
	mux.HandleFunc("POST /api/tokens", s.handleCreateToken)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.handleDeleteToken)

	mux.HandleFunc("GET /api/system", s.handleSystem)
	mux.HandleFunc("GET /api/system/docker", s.handleDockerUsage)
	mux.HandleFunc("GET /api/system/containers", s.handleContainerResources)
	mux.HandleFunc("POST /api/system/prune", s.handlePrune)

	mux.HandleFunc("GET /api/repos", s.handleListRepos)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/branches", s.handleListBranches)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/commits", s.handleListCommits)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/compose", s.handleDetectCompose)

	mux.HandleFunc("GET /api/apps", s.handleListApps)
	mux.HandleFunc("POST /api/apps", s.handleCreateApp)
	mux.HandleFunc("GET /api/apps/{slug}", s.handleGetApp)
	mux.HandleFunc("PATCH /api/apps/{slug}", s.handleUpdateApp)
	mux.HandleFunc("DELETE /api/apps/{slug}", s.handleDeleteApp)
	mux.HandleFunc("POST /api/apps/{slug}/start", s.handleAppOperation(AppOperator.Start))
	mux.HandleFunc("POST /api/apps/{slug}/stop", s.handleAppOperation(AppOperator.Stop))
	mux.HandleFunc("POST /api/apps/{slug}/restart", s.handleAppOperation(AppOperator.Restart))
	mux.HandleFunc("GET /api/apps/{slug}/services", s.handleListServices)
	mux.HandleFunc("GET /api/apps/{slug}/memory", s.handleMemoryHistory)
	mux.HandleFunc("GET /api/apps/{slug}/env", s.handleGetEnv)
	mux.HandleFunc("PUT /api/apps/{slug}/env", s.handlePutEnv)
	mux.HandleFunc("GET /api/apps/{slug}/env/requirements", s.handleEnvRequirements)
	mux.HandleFunc("PUT /api/apps/{slug}/env/{key}", s.handlePutEnvVar)
	mux.HandleFunc("DELETE /api/apps/{slug}/env/{key}", s.handleDeleteEnvVar)
	mux.HandleFunc("GET /api/apps/{slug}/deployments", s.handleListDeployments)
	mux.HandleFunc("POST /api/apps/{slug}/deployments", s.handleCreateDeployment)
	mux.HandleFunc("GET /api/apps/{slug}/promotion", s.handlePromotionPreview)

	mux.HandleFunc("GET /api/apps/{slug}/domains", s.handleListDomains)
	mux.HandleFunc("POST /api/apps/{slug}/domains", s.handleAddDomain)
	mux.HandleFunc("POST /api/apps/{slug}/domains/{id}/verify", s.handleVerifyDomain)
	mux.HandleFunc("PATCH /api/apps/{slug}/domains/{id}", s.handleUpdateDomain)
	mux.HandleFunc("DELETE /api/apps/{slug}/domains/{id}", s.handleDeleteDomain)

	mux.HandleFunc("GET /api/apps/{slug}/logs", s.handleLiveLogs)
	mux.HandleFunc("GET /api/apps/{slug}/logs/history", s.handleLogHistory)
	mux.HandleFunc("GET /api/apps/{slug}/logs/archives", s.handleLogArchives)
	mux.HandleFunc("GET /api/apps/{slug}/logs/archives/url", s.handleLogArchiveURL)

	mux.HandleFunc("GET /api/backups", s.handleBackupStatus)
	mux.HandleFunc("POST /api/backups", s.handleStartBackup)
	mux.HandleFunc("GET /api/apps/{slug}/backups", s.handleListSnapshots)
	mux.HandleFunc("POST /api/apps/{slug}/backups/restore", s.handleRestoreVolume)
	mux.HandleFunc("GET /api/apps/{slug}/backups/{snapshot}/download", s.handleDownloadDump)
	mux.HandleFunc("GET /api/apps/{slug}/volumes", s.handleListVolumes)

	mux.HandleFunc("GET /api/apps/{slug}/db-copies", s.handleListDBCopies)
	mux.HandleFunc("POST /api/apps/{slug}/db-copies", s.handleStartDBCopy)
	mux.HandleFunc("GET /api/db-copies/{id}", s.handleGetDBCopy)
	mux.HandleFunc("GET /api/db-copies/{id}/logs", s.handleDBCopyLogs)
	mux.HandleFunc("GET /api/apps/{slug}/tasks", s.handleListTasks)
	mux.HandleFunc("PUT /api/apps/{slug}/tasks", s.handlePutTasks)
	mux.HandleFunc("GET /api/apps/{slug}/task-runs", s.handleListTaskRuns)
	mux.HandleFunc("POST /api/apps/{slug}/task-runs", s.handleCreateTaskRun)
	mux.HandleFunc("GET /api/task-runs/{id}", s.handleGetTaskRun)
	mux.HandleFunc("GET /api/task-runs/{id}/logs", s.handleTaskRunLogs)
	mux.HandleFunc("POST /api/task-runs/{id}/cancel", s.handleCancelTaskRun)

	mux.HandleFunc("GET /api/deployments/{id}", s.handleGetDeployment)
	mux.HandleFunc("GET /api/deployments/{id}/logs", s.handleDeploymentLogs)
	mux.HandleFunc("POST /api/deployments/{id}/cancel", s.handleCancelDeployment)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
	})
	mux.Handle("/", s.spaHandler())
	return mux
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details carries structured context such as policy violations.
	Details any `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach Flush on the underlying writer (SSE).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// requestInfo carries what inner handlers learn about a request back to
// the request log.
type requestInfo struct {
	// tokenID names the API token the request used; the value never
	// reaches the log.
	tokenID string
}

type requestInfoKey struct{}

func requestInfoFrom(r *http.Request) *requestInfo {
	info, _ := r.Context().Value(requestInfoKey{}).(*requestInfo)
	return info
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		info := &requestInfo{}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info)))
		// The query string is omitted on purpose: it may carry OAuth codes.
		// Headers are never logged: they carry cookies and API tokens.
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if info.tokenID != "" {
			attrs = append(attrs, "token_id", info.tokenID)
		}
		s.log.Info("http request", attrs...)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// spaHandler serves built frontend files and falls back to index.html so
// that client-side routes work on reload.
func (s *Server) spaHandler() http.Handler {
	var files http.Handler
	if s.web != nil {
		files = http.FileServerFS(s.web)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		index, err := s.readIndex()
		if err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("The frontend is not built. Run `make web` and rebuild koroku.\n"))
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" && name != "index.html" {
			if info, err := fs.Stat(s.web, name); err == nil && !info.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					// Vite emits content-hashed file names under assets/.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(index)
	})
}

func (s *Server) readIndex() ([]byte, error) {
	if s.web == nil {
		return nil, fs.ErrNotExist
	}
	return fs.ReadFile(s.web, "index.html")
}
