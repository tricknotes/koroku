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
	"github.com/tricknotes/koroku/internal/envpresets"
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
	// PushDeployer queues deployments of GitHub pushes (ARCHITECTURE 5.2).
	// Optional; without it the webhook refuses every delivery.
	PushDeployer PushDeployer
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
	// Users resolves GitHub logins when the owner adds a member. Optional.
	Users UserLookup
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
	// pushDeployer, webhookLimiter and deliveries serve the GitHub webhook
	// (ARCHITECTURE 5.2).
	pushDeployer   PushDeployer
	webhookLimiter *rateLimiter
	deliveries     *deliverySet
	refs           RefResolver
	router         Router
	domains        DomainChecker
	auth           AuthOptions
	ops            AppOperator
	tasks          TaskRunner
	logs           AppLogs
	backups        Backups
	volumes        AppVolumes
	copier         DBCopier
	users          UserLookup
	envReqs        *envreq.Reader
	// envPresets reads repositories for the create screen (ARCHITECTURE 9.8).
	envPresets *envpresets.Reader
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
	var presetFiles envpresets.Files
	if opts.Auth.GitHub != nil {
		files = opts.Auth.GitHub
		presetFiles = opts.Auth.GitHub
	}
	return &Server{
		envReqs:     envreq.NewReader(files, opts.PublicFiles),
		publicFiles: opts.PublicFiles,
		envPresets:  envpresets.NewReader(presetFiles, opts.PublicFiles),
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
		users:       opts.Users,

		routeStatus: opts.RouteStatus,

		// One person logs in a few times a week; ten a minute is generous.
		callbackLimiter: &rateLimiter{limit: 10, window: time.Minute},
		tokenLimiter:    &tokenLimiter{},

		pushDeployer: opts.PushDeployer,
		// Pushes come a few at a time; the limit keeps a flood of signed or
		// unsigned deliveries from costing more than reading their bodies.
		webhookLimiter: &rateLimiter{limit: 60, window: time.Minute},
		deliveries:     newDeliverySet(deliveryLimit, deliveryTTL),
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

	// The webhook authenticates by its signature, not by a session or a
	// role (ARCHITECTURE 5.2).
	mux.HandleFunc("POST "+WebhookPath, s.handleGitHubWebhook)
	mux.HandleFunc("/webhooks/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	for _, rt := range s.apiRoutes() {
		mux.Handle(rt.pattern, s.authorize(rt.access, rt.handler))
	}

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
	})
	mux.Handle("/", s.spaHandler())
	return mux
}

// apiRoutes lists every API endpoint with what it requires (ARCHITECTURE
// 10.7). A route is reachable only through this table, so none can be
// added without saying who may use it.
func (s *Server) apiRoutes() []route {
	return []route{
		// Each user's own sessions, login history and API tokens; the owner
		// sees everyone's.
		{"GET /api/sessions", anyUser, s.handleListSessions},
		{"DELETE /api/sessions/{id}", anyUser, s.handleDeleteSession},
		{"POST /api/sessions/revoke-others", anyUser, s.handleRevokeOtherSessions},
		{"GET /api/login-events", anyUser, s.handleLoginEvents},
		{"GET /api/tokens", anyUser, s.handleListTokens},
		{"POST /api/tokens", anyUser, s.handleCreateToken},
		{"DELETE /api/tokens/{id}", anyUser, s.handleDeleteToken},

		{"GET /api/members", ownerOnly, s.handleListMembers},
		{"POST /api/members", ownerOnly, s.handleAddMember},
		{"DELETE /api/members/{id}", ownerOnly, s.handleDeleteMember},
		{"PUT /api/members/{id}/roles/{slug}", ownerOnly, s.handlePutMemberRole},
		{"DELETE /api/members/{id}/roles/{slug}", ownerOnly, s.handleDeleteMemberRole},

		// The host summary of the app list; members get it without the
		// error texts, which may name other apps.
		{"GET /api/system", anyUser, s.handleSystem},
		{"GET /api/system/docker", ownerOnly, s.handleDockerUsage},
		{"GET /api/system/containers", ownerOnly, s.handleContainerResources},
		{"GET /api/system/github", ownerOnly, s.handleGitHubApp},
		{"POST /api/system/prune", ownerOnly, s.handlePrune},

		{"GET /api/repos", ownerOnly, s.handleListRepos},
		{"GET /api/repos/{owner}/{repo}/branches", onRepo(levelView), s.handleListBranches},
		{"GET /api/repos/{owner}/{repo}/commits", onRepo(levelView), s.handleListCommits},
		{"GET /api/repos/{owner}/{repo}/compose", ownerOnly, s.handleDetectCompose},
		{"GET /api/repos/{owner}/{repo}/env-presets", ownerOnly, s.handleEnvPresets},

		{"GET /api/apps", anyUser, s.handleListApps},
		{"POST /api/apps", ownerOnly, s.handleCreateApp},
		{"GET /api/apps/{slug}", onApp(levelView), s.handleGetApp},
		// Owner-only settings are refused inside the handler.
		{"PATCH /api/apps/{slug}", onApp(levelManage), s.handleUpdateApp},
		{"DELETE /api/apps/{slug}", ownerOnly, s.handleDeleteApp},
		{"POST /api/apps/{slug}/start", onApp(levelDeploy), s.handleAppOperation(AppOperator.Start)},
		{"POST /api/apps/{slug}/stop", onApp(levelDeploy), s.handleAppOperation(AppOperator.Stop)},
		{"POST /api/apps/{slug}/restart", onApp(levelDeploy), s.handleAppOperation(AppOperator.Restart)},
		{"GET /api/apps/{slug}/services", onApp(levelView), s.handleListServices},
		{"GET /api/apps/{slug}/memory", onApp(levelView), s.handleMemoryHistory},
		{"GET /api/apps/{slug}/activity", onApp(levelView), s.handleListActivity},
		// Below manage, only the keys are returned.
		{"GET /api/apps/{slug}/env", onApp(levelView), s.handleGetEnv},
		{"PUT /api/apps/{slug}/env", onApp(levelManage), s.handlePutEnv},
		{"GET /api/apps/{slug}/env/requirements", onApp(levelView), s.handleEnvRequirements},
		{"PUT /api/apps/{slug}/env/{key}", onApp(levelManage), s.handlePutEnvVar},
		{"DELETE /api/apps/{slug}/env/{key}", onApp(levelManage), s.handleDeleteEnvVar},
		// The token is never returned, so viewing needs no more than view.
		{"GET /api/apps/{slug}/registry-credential", onApp(levelView), s.handleGetRegistryCredential},
		{"PUT /api/apps/{slug}/registry-credential", onApp(levelManage), s.handlePutRegistryCredential},
		{"DELETE /api/apps/{slug}/registry-credential", onApp(levelManage), s.handleDeleteRegistryCredential},
		{"GET /api/apps/{slug}/deployments", onApp(levelView), s.handleListDeployments},
		// Deploying, rolling back and promoting into the app.
		{"POST /api/apps/{slug}/deployments", onApp(levelDeploy), s.handleCreateDeployment},
		{"GET /api/apps/{slug}/promotion", onApp(levelDeploy), s.handlePromotionPreview},

		{"GET /api/apps/{slug}/domains", onApp(levelView), s.handleListDomains},
		{"POST /api/apps/{slug}/domains", onApp(levelManage), s.handleAddDomain},
		{"POST /api/apps/{slug}/domains/{id}/verify", onApp(levelManage), s.handleVerifyDomain},
		{"PATCH /api/apps/{slug}/domains/{id}", onApp(levelManage), s.handleUpdateDomain},
		{"DELETE /api/apps/{slug}/domains/{id}", onApp(levelManage), s.handleDeleteDomain},

		{"GET /api/apps/{slug}/logs", onApp(levelView), s.handleLiveLogs},
		{"GET /api/apps/{slug}/logs/history", onApp(levelView), s.handleLogHistory},
		{"GET /api/apps/{slug}/logs/archives", onApp(levelView), s.handleLogArchives},
		{"GET /api/apps/{slug}/logs/archives/url", onApp(levelView), s.handleLogArchiveURL},

		// The overall backup state; members get it without the host's run
		// history.
		{"GET /api/backups", anyUser, s.handleBackupStatus},
		// There is no backup of a single app: a member who manages an app
		// may start the full run that includes it.
		{"POST /api/backups", onAnyApp(levelManage), s.handleStartBackup},
		{"GET /api/apps/{slug}/backups", onApp(levelView), s.handleListSnapshots},
		{"POST /api/apps/{slug}/backups/restore", onApp(levelManage), s.handleRestoreVolume},
		{"GET /api/apps/{slug}/backups/{snapshot}/download", onApp(levelManage), s.handleDownloadDump},
		{"GET /api/apps/{slug}/volumes", onApp(levelView), s.handleListVolumes},

		{"GET /api/apps/{slug}/db-copies", onApp(levelView), s.handleListDBCopies},
		// The source app needs manage as well; checked in the handler.
		{"POST /api/apps/{slug}/db-copies", onApp(levelManage), s.handleStartDBCopy},
		{"GET /api/db-copies/{id}", onDBCopy(levelView), s.handleGetDBCopy},
		{"GET /api/db-copies/{id}/logs", onDBCopy(levelView), s.handleDBCopyLogs},
		{"GET /api/apps/{slug}/tasks", onApp(levelView), s.handleListTasks},
		{"PUT /api/apps/{slug}/tasks", onApp(levelManage), s.handlePutTasks},
		{"GET /api/apps/{slug}/task-runs", onApp(levelView), s.handleListTaskRuns},
		{"POST /api/apps/{slug}/task-runs", onApp(levelManage), s.handleCreateTaskRun},
		{"GET /api/task-runs/{id}", onTaskRun(levelView), s.handleGetTaskRun},
		{"GET /api/task-runs/{id}/logs", onTaskRun(levelView), s.handleTaskRunLogs},
		{"POST /api/task-runs/{id}/cancel", onTaskRun(levelManage), s.handleCancelTaskRun},

		{"GET /api/deployments/{id}", onDeployment(levelView), s.handleGetDeployment},
		{"GET /api/deployments/{id}/logs", onDeployment(levelView), s.handleDeploymentLogs},
		{"POST /api/deployments/{id}/cancel", onDeployment(levelDeploy), s.handleCancelDeployment},
	}
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
	// login is the GitHub login of the user the request acts as.
	login string
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
		if info.login != "" {
			attrs = append(attrs, "user", info.login)
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
