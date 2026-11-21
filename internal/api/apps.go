package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/envreq"
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

// maxExemptPaths bounds the paths an app can exempt from access protection.
const maxExemptPaths = 20

// reservedSlugs cannot be used because their hostnames belong to the platform.
var reservedSlugs = map[string]bool{"console": true, "www": true, "api": true}

type appJSON struct {
	Slug                 string            `json:"slug"`
	RepoFullName         string            `json:"repo_full_name"`
	RepoURL              string            `json:"repo_url"`
	DefaultBranch        string            `json:"default_branch"`
	ComposePath          string            `json:"compose_path"`
	WebService           string            `json:"web_service"`
	WebPort              int               `json:"web_port"`
	HostPort             int               `json:"host_port"`
	HealthcheckPath      string            `json:"healthcheck_path"`
	DesiredState         string            `json:"desired_state"`
	Environment          string            `json:"environment"`
	RelaxPolicy          bool              `json:"relax_policy"`
	MemoryLimit          string            `json:"memory_limit"`
	ServiceMemoryLimits  map[string]string `json:"service_memory_limits"`
	BackupExcludeVolumes []string          `json:"backup_exclude_volumes"`
	DumpHookService      string            `json:"dump_hook_service"`
	DumpHookCommand      string            `json:"dump_hook_command"`
	BackupBeforeDeploy   bool              `json:"backup_before_deploy"`
	LoadHookService      string            `json:"load_hook_service"`
	LoadHookCommand      string            `json:"load_hook_command"`
	PostLoadService      string            `json:"post_load_service"`
	PostLoadCommand      string            `json:"post_load_command"`
	EnvSource            string            `json:"env_source"`
	EnvExamplePath       string            `json:"env_example_path"`
	AccessProtection     bool              `json:"access_protection"`
	ProtectionExempt     []string          `json:"protection_exempt_paths"`
	PromoteFrom          string            `json:"promote_from"`
	AutoDeploy           bool              `json:"auto_deploy"`
	AutoDeployBranch     string            `json:"auto_deploy_branch"`
	URL                  string            `json:"url"`
	PrimaryURL           string            `json:"primary_url"`
	PendingDomains       int               `json:"pending_domains"`
	CurrentDeployment    *deploymentJSON   `json:"current_deployment"`
	LatestDeployment     *deploymentJSON   `json:"latest_deployment"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
	// Role is what the requesting user may do on the app: "owner" for the
	// owner, otherwise the member's role (ARCHITECTURE 10.7).
	Role string `json:"role"`
}

func (s *Server) appToJSON(r *http.Request, app store.App) (appJSON, error) {
	out := appJSON{
		Slug:                 app.Slug,
		RepoFullName:         app.RepoFullName,
		RepoURL:              app.RepoURL,
		DefaultBranch:        app.DefaultBranch,
		ComposePath:          app.ComposePath,
		WebService:           app.WebService,
		WebPort:              app.WebPort,
		HostPort:             app.HostPort,
		HealthcheckPath:      app.HealthcheckPath,
		DesiredState:         app.DesiredState,
		Environment:          app.Environment,
		RelaxPolicy:          app.RelaxPolicy,
		MemoryLimit:          app.MemoryLimit,
		ServiceMemoryLimits:  app.ServiceMemoryLimits,
		BackupExcludeVolumes: app.BackupExcludeVolumes,
		DumpHookService:      app.DumpHookService,
		DumpHookCommand:      app.DumpHookCommand,
		BackupBeforeDeploy:   app.BackupBeforeDeploy,
		LoadHookService:      app.LoadHookService,
		LoadHookCommand:      app.LoadHookCommand,
		PostLoadService:      app.PostLoadService,
		PostLoadCommand:      app.PostLoadCommand,
		EnvSource:            app.EnvSource,
		EnvExamplePath:       app.EnvExamplePath,
		AccessProtection:     app.AccessProtection,
		ProtectionExempt:     app.ProtectionExemptPaths,
		AutoDeploy:           app.AutoDeploy,
		AutoDeployBranch:     app.AutoDeployBranch,
		URL:                  "https://" + app.Slug + "." + s.cfg.BaseDomain,
		CreatedAt:            app.CreatedAt,
		UpdatedAt:            app.UpdatedAt,
	}
	lvl, err := s.appLevel(r, app)
	if err != nil {
		return appJSON{}, err
	}
	out.Role = lvl.String()
	// The primary URL and the number of domains waiting for DNS let the app
	// list render without one domain request per app.
	out.PrimaryURL = out.URL
	promoteFrom, err := s.promoteFromSlug(r.Context(), app)
	if err != nil {
		return appJSON{}, err
	}
	out.PromoteFrom = promoteFrom
	domainList, err := s.store.ListDomains(r.Context(), app.ID)
	if err != nil {
		return appJSON{}, err
	}
	for _, d := range domainList {
		switch {
		case d.Status == store.DomainPending:
			out.PendingDomains++
		case d.IsPrimary:
			out.PrimaryURL = "https://" + d.Hostname
		}
	}
	if app.CurrentDeploymentID != nil {
		d, err := s.store.GetDeployment(r.Context(), *app.CurrentDeploymentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return appJSON{}, err
		}
		if err == nil {
			j := s.deploymentToJSON(r.Context(), app, d)
			out.CurrentDeployment = &j
		}
	}
	latest, err := s.store.ListDeployments(r.Context(), app.ID, 1)
	if err != nil {
		return appJSON{}, err
	}
	if len(latest) == 1 {
		j := s.deploymentToJSON(r.Context(), app, latest[0])
		out.LatestDeployment = &j
	}
	return out, nil
}

// appInput is the body of POST and PATCH /api/apps. Pointers distinguish
// "not sent" from zero values so that PATCH can update single fields.
type appInput struct {
	Slug                 *string            `json:"slug"`
	RepoFullName         *string            `json:"repo_full_name"`
	RepoURL              *string            `json:"repo_url"`
	RepoID               *int64             `json:"repo_id"`
	DefaultBranch        *string            `json:"default_branch"`
	ComposePath          *string            `json:"compose_path"`
	WebService           *string            `json:"web_service"`
	WebPort              *int               `json:"web_port"`
	HealthcheckPath      *string            `json:"healthcheck_path"`
	Environment          *string            `json:"environment"`
	RelaxPolicy          *bool              `json:"relax_policy"`
	MemoryLimit          *string            `json:"memory_limit"`
	ServiceMemoryLimits  *map[string]string `json:"service_memory_limits"`
	BackupExcludeVolumes *[]string          `json:"backup_exclude_volumes"`
	DumpHookService      *string            `json:"dump_hook_service"`
	DumpHookCommand      *string            `json:"dump_hook_command"`
	BackupBeforeDeploy   *bool              `json:"backup_before_deploy"`
	LoadHookService      *string            `json:"load_hook_service"`
	LoadHookCommand      *string            `json:"load_hook_command"`
	PostLoadService      *string            `json:"post_load_service"`
	PostLoadCommand      *string            `json:"post_load_command"`
	EnvSource            *string            `json:"env_source"`
	EnvExamplePath       *string            `json:"env_example_path"`
	AccessProtection     *bool              `json:"access_protection"`
	ProtectionExempt     *[]string          `json:"protection_exempt_paths"`
	PromoteFrom          *string            `json:"promote_from"`
	AutoDeploy           *bool              `json:"auto_deploy"`
	AutoDeployBranch     *string            `json:"auto_deploy_branch"`
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

	if in.BackupExcludeVolumes != nil {
		app.BackupExcludeVolumes = []string{}
		for _, name := range *in.BackupExcludeVolumes {
			if name = strings.TrimSpace(name); name != "" && !slices.Contains(app.BackupExcludeVolumes, name) {
				app.BackupExcludeVolumes = append(app.BackupExcludeVolumes, name)
			}
		}
	}
	if in.DumpHookService != nil {
		app.DumpHookService = strings.TrimSpace(*in.DumpHookService)
	}
	if in.DumpHookCommand != nil {
		app.DumpHookCommand = strings.TrimSpace(*in.DumpHookCommand)
	}
	if in.BackupBeforeDeploy != nil {
		app.BackupBeforeDeploy = *in.BackupBeforeDeploy
	}
	if in.LoadHookService != nil {
		app.LoadHookService = strings.TrimSpace(*in.LoadHookService)
	}
	if in.LoadHookCommand != nil {
		app.LoadHookCommand = strings.TrimSpace(*in.LoadHookCommand)
	}
	if in.PostLoadService != nil {
		app.PostLoadService = strings.TrimSpace(*in.PostLoadService)
	}
	if in.PostLoadCommand != nil {
		app.PostLoadCommand = strings.TrimSpace(*in.PostLoadCommand)
	}

	if in.EnvSource != nil {
		app.EnvSource = strings.TrimSpace(*in.EnvSource)
	}
	if in.EnvExamplePath != nil {
		app.EnvExamplePath = strings.TrimSpace(*in.EnvExamplePath)
	}
	if in.AccessProtection != nil {
		app.AccessProtection = *in.AccessProtection
	}
	if in.ProtectionExempt != nil {
		app.ProtectionExemptPaths = []string{}
		for _, p := range *in.ProtectionExempt {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(app.ProtectionExemptPaths, p) {
				app.ProtectionExemptPaths = append(app.ProtectionExemptPaths, p)
			}
		}
	}

	if in.AutoDeploy != nil {
		app.AutoDeploy = *in.AutoDeploy
	}
	if in.AutoDeployBranch != nil {
		app.AutoDeployBranch = strings.TrimSpace(*in.AutoDeployBranch)
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
	if b := app.AutoDeployBranch; b != "" && (!branchPattern.MatchString(b) || strings.Contains(b, "..") || len(b) > 255) {
		errs.add("auto_deploy_branch", "is not a valid branch name")
	}
	if app.AutoDeploy && !app.UsesGitHubApp() {
		// Pushes arrive through the GitHub App's webhook (ARCHITECTURE 5.2).
		errs.add("auto_deploy", "needs a repository fetched through the GitHub App (repo_full_name without repo_url)")
	}
	if app.ComposePath != "" {
		clean := filepath.Clean(app.ComposePath)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			errs.add("compose_path", "must be a path inside the repository")
		}
	}
	if !envreq.ValidSource(app.EnvSource) {
		errs.add("env_source", "must be compose, dotenv or off")
	}
	if app.EnvExamplePath != "" {
		clean := path.Clean(app.EnvExamplePath)
		if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || len(app.EnvExamplePath) > 255 {
			errs.add("env_example_path", "must be a file path inside the repository")
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
	for _, name := range app.BackupExcludeVolumes {
		if !servicePattern.MatchString(name) {
			errs.add("backup_exclude_volumes", fmt.Sprintf("%q is not a valid volume name", truncateForError(name)))
		}
	}
	if len(app.ProtectionExemptPaths) > maxExemptPaths {
		errs.add("protection_exempt_paths", fmt.Sprintf("at most %d paths", maxExemptPaths))
	}
	for _, p := range app.ProtectionExemptPaths {
		if err := caddy.CheckExemptPath(p); err != nil {
			errs.add("protection_exempt_paths", fmt.Sprintf("%q %s", truncateForError(p), err))
		}
	}
	validateHook(errs, "dump_hook", app.DumpHookService, app.DumpHookCommand)
	validateHook(errs, "load_hook", app.LoadHookService, app.LoadHookCommand)
	validateHook(errs, "post_load", app.PostLoadService, app.PostLoadCommand)
	return errs
}

// validateHook checks a service and command pair (the dump hook, the load
// hook, the post-load command): both or neither, a valid service name and
// a single line of command.
func validateHook(errs fieldErrors, prefix, service, command string) {
	switch {
	case service == "" && command == "":
	case service == "":
		errs.add(prefix+"_service", "is required when a command is set")
	case command == "":
		errs.add(prefix+"_command", "is required when a service is set")
	case !servicePattern.MatchString(service):
		errs.add(prefix+"_service", "is not a valid service name")
	case len(command) > 1000 || strings.ContainsAny(command, "\n\r\x00"):
		errs.add(prefix+"_command", "must be a single line of at most 1000 characters")
	}
}

func (s *Server) validateSlug(slug string) string {
	switch {
	case !slugPattern.MatchString(slug):
		return "must be 1-40 lowercase letters, digits or hyphens, starting with a letter"
	case reservedSlugs[slug], slug+"."+s.cfg.BaseDomain == s.cfg.ConsoleHost:
		// The console's own hostname can be configured to anything.
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

// handleListApps lists every app for the owner, and the apps a member has
// a role on for a member.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var roles map[int64]string
	if a, _ := actorFrom(r); !a.Owner {
		if roles, err = s.store.RolesOf(r.Context(), a.UserID); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	out := make([]appJSON, 0, len(apps))
	for _, app := range apps {
		if roles != nil && roles[app.ID] == "" {
			continue
		}
		j, err := s.appToJSON(r, app)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

// createAppInput is the body of POST /api/apps: the app's fields and the
// environment variables to store with it (ARCHITECTURE 9.8). PATCH takes
// no variables.
type createAppInput struct {
	appInput
	Env []envVarJSON `json:"env"`
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var body createAppInput
	if !decodeJSON(w, r, &body) {
		return
	}
	in := body.appInput
	app := store.App{
		DefaultBranch:   "main",
		WebService:      "web",
		HealthcheckPath: "/",
		Environment:     store.EnvironmentStaging,
		EnvSource:       envreq.SourceCompose,
	}
	errs := in.apply(&app, s.cfg.Deploy.AllowFileRepos)
	if in.AccessProtection == nil {
		// Staging apps start closed, production apps open (ARCHITECTURE 8.3).
		app.AccessProtection = app.Environment == store.EnvironmentStaging
	}
	if in.Slug == nil {
		errs.add("slug", "is required")
	} else {
		app.Slug = *in.Slug
		if msg := s.validateSlug(app.Slug); msg != "" {
			errs.add("slug", msg)
		}
	}
	if in.WebPort == nil {
		errs["web_port"] = "is required"
	}
	if err := s.applyPromoteFrom(r.Context(), in, &app, errs); err != nil {
		s.internalError(w, r, err)
		return
	}
	sealEnv := s.sealCreateEnv(body.Env, errs)
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	// Saving variables needs a recent login (ARCHITECTURE 10.3); it is
	// checked before anything is created.
	if (len(body.Env) > 0 || enablesProductionAutoDeploy(store.App{}, app)) && !s.requireRecentAuth(w, r) {
		return
	}
	created, err := s.store.CreateAppWithEnv(r.Context(), app, s.cfg.Deploy.PortRangeStart, s.cfg.Deploy.PortRangeEnd, s.clock.Now(), sealEnv)
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
	before := app
	errs := in.apply(&app, s.cfg.Deploy.AllowFileRepos)
	if in.Slug != nil && *in.Slug != app.Slug {
		// The slug names the Compose project, its volumes and the hostname.
		errs.add("slug", "cannot be changed")
	}
	if !isOwner(r) {
		// Refused before the pairing is validated, so that a member cannot
		// learn from the error which other apps exist.
		fields, err := s.ownerOnlyChanges(r, before, app, in)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if len(fields) > 0 {
			writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
				Code:    "forbidden_role",
				Message: "only the owner can change these settings",
				Details: map[string]any{"required": levelOwner.String(), "fields": fields},
			}})
			return
		}
	}
	if err := s.applyPromoteFrom(r.Context(), in, &app, errs); err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if (touchesSafeguards(before, app) || enablesProductionAutoDeploy(before, app)) && !s.requireRecentAuth(w, r) {
		return
	}
	if err := s.store.UpdateApp(r.Context(), app, s.clock.Now()); err != nil {
		s.internalError(w, r, err)
		return
	}
	// Protection, exempt paths and the environment (noindex) shape the
	// proxy configuration; an unchanged one is not reloaded.
	s.syncRoutes(r, "app updated")
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

// ownerOnlyChanges lists the settings an update changes that only the owner
// may change (ARCHITECTURE 10.7): the environment (production asks for a
// confirmation before each deploy), turning the policy check off, where the
// code comes from, links to other apps, and making a production app deploy
// pushes by itself.
// after has the input applied except promote_from, which is compared by
// name.
func (s *Server) ownerOnlyChanges(r *http.Request, before, after store.App, in appInput) ([]string, error) {
	var fields []string
	if before.Environment != after.Environment {
		fields = append(fields, "environment")
	}
	if enablesProductionAutoDeploy(before, after) {
		fields = append(fields, "auto_deploy")
	}
	if before.RelaxPolicy != after.RelaxPolicy {
		fields = append(fields, "relax_policy")
	}
	if before.RepoFullName != after.RepoFullName || before.RepoURL != after.RepoURL ||
		!sameID(before.RepoID, after.RepoID) {
		fields = append(fields, "repository")
	}
	if in.PromoteFrom != nil {
		current, err := s.promoteFromSlug(r.Context(), before)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(*in.PromoteFrom) != current {
			fields = append(fields, "promote_from")
		}
	}
	return fields, nil
}

func sameID(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// touchesSafeguards reports whether an update touches a setting that guards
// the app: the environment (production asks for confirmation), the policy
// check, the repository the code comes from, or who may open the app. Such changes need a recent
// login (ARCHITECTURE 10.3).
func touchesSafeguards(before, after store.App) bool {
	sameRepoID := (before.RepoID == nil) == (after.RepoID == nil) &&
		(before.RepoID == nil || *before.RepoID == *after.RepoID)
	return before.Environment != after.Environment ||
		before.RelaxPolicy != after.RelaxPolicy ||
		before.RepoFullName != after.RepoFullName ||
		before.RepoURL != after.RepoURL ||
		!sameRepoID ||
		// Opening the app to everyone (ARCHITECTURE 8.3).
		(before.AccessProtection && !after.AccessProtection) ||
		!slices.Equal(before.ProtectionExemptPaths, after.ProtectionExemptPaths)
}

// enablesProductionAutoDeploy reports whether an update makes a production
// app deploy pushes, or pushes of another branch, without anyone pressing
// a button. Like deploying it by hand, that needs a recent login
// (ARCHITECTURE 10.3).
func enablesProductionAutoDeploy(before, after store.App) bool {
	if after.Environment != store.EnvironmentProduction || !after.AutoDeploy {
		return false
	}
	return !before.AutoDeploy || before.EffectiveAutoDeployBranch() != after.EffectiveAutoDeployBranch()
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
