package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/store"
)

const maxBodyBytes = 1 << 20

var (
	slugPattern     = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	memoryPattern   = regexp.MustCompile(`^[1-9][0-9]*[bkmg]?$`)
	servicePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	branchPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]*$`)
)

// reservedSlugs cannot be used because their hostnames belong to the platform.
var reservedSlugs = map[string]bool{"console": true, "www": true, "api": true}

type appJSON struct {
	Slug                string            `json:"slug"`
	RepoFullName        string            `json:"repo_full_name"`
	RepoURL             string            `json:"repo_url"`
	DefaultBranch       string            `json:"default_branch"`
	ComposePath         string            `json:"compose_path"`
	WebService          string            `json:"web_service"`
	WebPort             int               `json:"web_port"`
	HostPort            int               `json:"host_port"`
	HealthcheckPath     string            `json:"healthcheck_path"`
	DesiredState        string            `json:"desired_state"`
	Environment         string            `json:"environment"`
	RelaxPolicy         bool              `json:"relax_policy"`
	MemoryLimit         string            `json:"memory_limit"`
	ServiceMemoryLimits map[string]string `json:"service_memory_limits"`
	URL                 string            `json:"url"`
	CurrentDeployment   *deploymentJSON   `json:"current_deployment"`
	LatestDeployment    *deploymentJSON   `json:"latest_deployment"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

func (s *Server) appToJSON(r *http.Request, app store.App) (appJSON, error) {
	out := appJSON{
		Slug:                app.Slug,
		RepoFullName:        app.RepoFullName,
		RepoURL:             app.RepoURL,
		DefaultBranch:       app.DefaultBranch,
		ComposePath:         app.ComposePath,
		WebService:          app.WebService,
		WebPort:             app.WebPort,
		HostPort:            app.HostPort,
		HealthcheckPath:     app.HealthcheckPath,
		DesiredState:        app.DesiredState,
		Environment:         app.Environment,
		RelaxPolicy:         app.RelaxPolicy,
		MemoryLimit:         app.MemoryLimit,
		ServiceMemoryLimits: app.ServiceMemoryLimits,
		URL:                 "https://" + app.Slug + "." + s.cfg.BaseDomain,
		CreatedAt:           app.CreatedAt,
		UpdatedAt:           app.UpdatedAt,
	}
	if app.CurrentDeploymentID != nil {
		d, err := s.store.GetDeployment(r.Context(), *app.CurrentDeploymentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return appJSON{}, err
		}
		if err == nil {
			j := deploymentToJSON(d, app.Slug)
			out.CurrentDeployment = &j
		}
	}
	latest, err := s.store.ListDeployments(r.Context(), app.ID, 1)
	if err != nil {
		return appJSON{}, err
	}
	if len(latest) == 1 {
		j := deploymentToJSON(latest[0], app.Slug)
		out.LatestDeployment = &j
	}
	return out, nil
}

// appInput is the body of POST and PATCH /api/apps. Pointers distinguish
// "not sent" from zero values so that PATCH can update single fields.
type appInput struct {
	Slug                *string            `json:"slug"`
	RepoFullName        *string            `json:"repo_full_name"`
	RepoURL             *string            `json:"repo_url"`
	RepoID              *int64             `json:"repo_id"`
	DefaultBranch       *string            `json:"default_branch"`
	ComposePath         *string            `json:"compose_path"`
	WebService          *string            `json:"web_service"`
	WebPort             *int               `json:"web_port"`
	HealthcheckPath     *string            `json:"healthcheck_path"`
	Environment         *string            `json:"environment"`
	RelaxPolicy         *bool              `json:"relax_policy"`
	MemoryLimit         *string            `json:"memory_limit"`
	ServiceMemoryLimits *map[string]string `json:"service_memory_limits"`
}

// fieldErrors collects validation problems keyed by JSON field name.
type fieldErrors map[string]string

func (f fieldErrors) add(field, message string) {
	if _, exists := f[field]; !exists {
		f[field] = message
	}
}

// apply copies the sent fields onto app and validates the result.
func (in appInput) apply(app *store.App, allowFileRepos bool) fieldErrors {
	errs := fieldErrors{}
	if in.RepoFullName != nil {
		app.RepoFullName = strings.TrimSpace(*in.RepoFullName)
	}
	if in.RepoURL != nil {
		app.RepoURL = strings.TrimSpace(*in.RepoURL)
	}
	if in.RepoID != nil {
		app.RepoID = in.RepoID
	}
	if in.DefaultBranch != nil {
		app.DefaultBranch = strings.TrimSpace(*in.DefaultBranch)
	}
	if in.ComposePath != nil {
		app.ComposePath = strings.TrimSpace(*in.ComposePath)
	}
	if in.WebService != nil {
		app.WebService = strings.TrimSpace(*in.WebService)
	}
	if in.WebPort != nil {
		app.WebPort = *in.WebPort
	}
	if in.HealthcheckPath != nil {
		app.HealthcheckPath = strings.TrimSpace(*in.HealthcheckPath)
	}
	if in.Environment != nil {
		app.Environment = *in.Environment
	}
	if in.RelaxPolicy != nil {
		app.RelaxPolicy = *in.RelaxPolicy
	}
	if in.MemoryLimit != nil {
		app.MemoryLimit = strings.ToLower(strings.TrimSpace(*in.MemoryLimit))
	}
	if in.ServiceMemoryLimits != nil {
		app.ServiceMemoryLimits = map[string]string{}
		for name, limit := range *in.ServiceMemoryLimits {
			app.ServiceMemoryLimits[name] = strings.ToLower(strings.TrimSpace(limit))
		}
	}

	if app.RepoFullName == "" && app.RepoURL == "" {
		errs.add("repo_full_name", "a repository is required (repo_full_name or repo_url)")
	}
	if app.RepoFullName != "" && !repoNamePattern.MatchString(app.RepoFullName) {
		errs.add("repo_full_name", "must look like owner/name")
	}
	if app.RepoURL != "" {
		if err := gitrepo.ValidateURL(app.RepoURL, allowFileRepos); err != nil {
			errs.add("repo_url", err.Error())
		}
	}
	if !branchPattern.MatchString(app.DefaultBranch) || strings.Contains(app.DefaultBranch, "..") {
		errs.add("default_branch", "is not a valid branch name")
	}
	if app.ComposePath != "" {
		clean := filepath.Clean(app.ComposePath)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			errs.add("compose_path", "must be a path inside the repository")
		}
	}
	if !servicePattern.MatchString(app.WebService) {
		errs.add("web_service", "is not a valid service name")
	}
	if app.WebPort < 1 || app.WebPort > 65535 {
		errs.add("web_port", "must be between 1 and 65535")
	}
	if !strings.HasPrefix(app.HealthcheckPath, "/") || strings.ContainsAny(app.HealthcheckPath, " \t\r\n#") {
		errs.add("healthcheck_path", "must start with / and contain no spaces")
	}
	if app.Environment != store.EnvironmentProduction && app.Environment != store.EnvironmentStaging {
		errs.add("environment", "must be production or staging")
	}
	if app.MemoryLimit != "" && !memoryPattern.MatchString(app.MemoryLimit) {
		errs.add("memory_limit", "must be a number with an optional unit (b, k, m, g), for example 512m")
	}
	for name, limit := range app.ServiceMemoryLimits {
		if !servicePattern.MatchString(name) {
			errs.add("service_memory_limits", fmt.Sprintf("%q is not a valid service name", name))
		} else if !memoryPattern.MatchString(limit) {
			errs.add("service_memory_limits", fmt.Sprintf("limit of %s must be a number with an optional unit (b, k, m, g)", name))
		}
	}
	return errs
}

func validateSlug(slug string) string {
	switch {
	case !slugPattern.MatchString(slug):
		return "must be 1-40 lowercase letters, digits or hyphens, starting with a letter"
	case reservedSlugs[slug]:
		return "is reserved"
	}
	return ""
}

// decodeJSON reads a size-limited JSON body and rejects unknown fields so
// that typos surface instead of being ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON: "+jsonErrorSummary(err))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be a single JSON value")
		return false
	}
	return true
}

// jsonErrorSummary describes a decode error without echoing request content.
func jsonErrorSummary(err error) string {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("syntax error at offset %d", syntax.Offset)
	case errors.As(err, &typ):
		return fmt.Sprintf("field %q has the wrong type", typ.Field)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return strings.TrimPrefix(err.Error(), "json: ")
	default:
		return "malformed body"
	}
}

func writeValidationError(w http.ResponseWriter, errs fieldErrors) {
	writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: errorDetail{
		Code:    "validation_failed",
		Message: "some fields are invalid",
		Details: errs,
	}})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal server error")
}

// loadApp resolves the {slug} path value, writing a 404 when it is unknown.
func (s *Server) loadApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	app, err := s.store.GetApp(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app_not_found", "no such app")
		return store.App{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.App{}, false
	}
	return app, true
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]appJSON, 0, len(apps))
	for _, app := range apps {
		j, err := s.appToJSON(r, app)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var in appInput
	if !decodeJSON(w, r, &in) {
		return
	}
	app := store.App{
		DefaultBranch:   "main",
		WebService:      "web",
		HealthcheckPath: "/",
		Environment:     store.EnvironmentStaging,
	}
	errs := in.apply(&app, s.cfg.Deploy.AllowFileRepos)
	if in.Slug == nil {
		errs.add("slug", "is required")
	} else {
		app.Slug = *in.Slug
		if msg := validateSlug(app.Slug); msg != "" {
			errs.add("slug", msg)
		}
	}
	if in.WebPort == nil {
		errs["web_port"] = "is required"
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	created, err := s.store.CreateApp(r.Context(), app, s.cfg.Deploy.PortRangeStart, s.cfg.Deploy.PortRangeEnd, s.clock.Now())
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "slug_taken", "an app with this slug already exists")
		return
	case errors.Is(err, store.ErrNoFreePort):
		writeError(w, http.StatusConflict, "no_free_port", "the host port range is exhausted")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.syncRoutes(r, "app created")
	j, err := s.appToJSON(r, created)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	j, err := s.appToJSON(r, app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in appInput
	if !decodeJSON(w, r, &in) {
		return
	}
	errs := in.apply(&app, s.cfg.Deploy.AllowFileRepos)
	if in.Slug != nil && *in.Slug != app.Slug {
		// The slug names the Compose project, its volumes and the hostname.
		errs.add("slug", "cannot be changed")
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if err := s.store.UpdateApp(r.Context(), app, s.clock.Now()); err != nil {
		s.internalError(w, r, err)
		return
	}
	updated, err := s.store.GetAppByID(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	j, err := s.appToJSON(r, updated)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// syncRoutes reloads the reverse proxy after a routing-relevant change. A
// failure is logged, not returned: the database already holds the truth and
// the next sync (or restart) applies it.
func (s *Server) syncRoutes(r *http.Request, reason string) {
	if s.router == nil {
		return
	}
	if err := s.router.Sync(r.Context()); err != nil {
		s.log.Error("syncing routes failed", "reason", reason, "error", err)
	}
}
