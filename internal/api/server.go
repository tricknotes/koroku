// Package api implements the HTTP interface of paasd: the JSON REST API,
// SSE streams and delivery of the embedded single-page app.
package api

import (
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
	// Refs resolves branch names for deployments. Optional.
	Refs RefResolver
	// Router reloads the reverse proxy after routing changes. Optional.
	Router Router
	// Domains verifies custom domains. Optional.
	Domains DomainChecker
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
	refs     RefResolver
	router   Router
	domains  DomainChecker
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
	return &Server{
		cfg:      opts.Config,
		store:    opts.Store,
		system:   opts.System,
		web:      opts.Web,
		log:      logger,
		version:  opts.Version,
		clock:    clk,
		box:      opts.Box,
		deployer: opts.Deployer,
		refs:     opts.Refs,
		router:   opts.Router,
		domains:  opts.Domains,
	}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/system", s.handleSystem)

	mux.HandleFunc("GET /api/apps", s.handleListApps)
	mux.HandleFunc("POST /api/apps", s.handleCreateApp)
	mux.HandleFunc("GET /api/apps/{slug}", s.handleGetApp)
	mux.HandleFunc("PATCH /api/apps/{slug}", s.handleUpdateApp)
	mux.HandleFunc("GET /api/apps/{slug}/env", s.handleGetEnv)
	mux.HandleFunc("PUT /api/apps/{slug}/env", s.handlePutEnv)
	mux.HandleFunc("GET /api/apps/{slug}/deployments", s.handleListDeployments)
	mux.HandleFunc("POST /api/apps/{slug}/deployments", s.handleCreateDeployment)

	mux.HandleFunc("GET /api/apps/{slug}/domains", s.handleListDomains)
	mux.HandleFunc("POST /api/apps/{slug}/domains", s.handleAddDomain)
	mux.HandleFunc("POST /api/apps/{slug}/domains/{id}/verify", s.handleVerifyDomain)
	mux.HandleFunc("PATCH /api/apps/{slug}/domains/{id}", s.handleUpdateDomain)
	mux.HandleFunc("DELETE /api/apps/{slug}/domains/{id}", s.handleDeleteDomain)

	mux.HandleFunc("GET /api/deployments/{id}", s.handleGetDeployment)
	mux.HandleFunc("GET /api/deployments/{id}/logs", s.handleDeploymentLogs)
	mux.HandleFunc("POST /api/deployments/{id}/cancel", s.handleCancelDeployment)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
	})
	mux.Handle("/", s.spaHandler())
	return s.recoverPanics(s.logRequests(mux))
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

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// The query string is omitted on purpose: it may carry OAuth codes.
		s.log.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
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
			_, _ = w.Write([]byte("The frontend is not built. Run `make web` and rebuild paasd.\n"))
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
