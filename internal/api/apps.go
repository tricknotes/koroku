package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/envreq"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/store"
	"go.yaml.in/yaml/v3"
)

const maxBodyBytes = 1 << 20

var (
	slugPattern     = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	memoryPattern   = regexp.MustCompile(`^[1-9][0-9]*[bkmg]?$`)
	servicePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	branchPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]*$`)
	// workflowPathPattern is a workflow file (ARCHITECTURE 5.2).
	workflowPathPattern = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$`)
)

// maxExemptPaths bounds the paths an app can exempt from access protection.
const maxExemptPaths = 20

// reservedSlugs cannot be used because their hostnames belong to the platform.
var reservedSlugs = map[string]bool{"console": true, "www": true, "api": true}

type appJSON struct {
	Slug          string `json:"slug"`
	RepoFullName  string `json:"repo_full_name"`
	RepoURL       string `json:"repo_url"`
	DefaultBranch string `json:"default_branch"`
	ComposePath   string `json:"compose_path"`
	// ComposeSource is where the Compose file comes from: "repository" or
	// "inline" (ARCHITECTURE 6.3). ComposeContent is the file written in
	// koroku; null, with ComposeContentHidden true, for a role below
	// manage and for an API token.
	ComposeSource        string            `json:"compose_source"`
	ComposeContent       *string           `json:"compose_content"`
	ComposeContentHidden bool              `json:"compose_content_hidden"`
	WebService           string            `json:"web_service"`
	WebPort              int               `json:"web_port"`
	HostPort             int               `json:"host_port"`
	HealthcheckPath      string            `json:"healthcheck_path"`
	DesiredState         string            `json:"desired_state"`
	Environment          string            `json:"environment"`
	RelaxPolicy          bool              `json:"relax_policy"`
	MemoryLimit          string            `json:"memory_limit"`
	ServiceMemoryLimits  map[string]string `json:"service_memory_limits"`
	MemoryReservation    string            `json:"memory_reservation"`
	ServiceReservations  map[string]string `json:"service_memory_reservations"`
	BackupExcludeVolumes []string          `json:"backup_exclude_volumes"`
	DumpHookService      string            `json:"dump_hook_service"`
	DumpHookCommand      string            `json:"dump_hook_command"`
	BackupBeforeDeploy   bool              `json:"backup_before_deploy"`
	DumpIntervalHours    int               `json:"dump_interval_hours"`
	PITRService          string            `json:"pitr_service"`
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
	AutoDeployWorkflow   string            `json:"auto_deploy_workflow"`
	IdleSleep            bool              `json:"idle_sleep"`
	IdleSleepMinutes     int               `json:"idle_sleep_minutes"`
	// SleepingSince is when the app went to sleep while idle; null while
	// it is awake (ARCHITECTURE 8.4).
	SleepingSince     *time.Time      `json:"sleeping_since"`
	URL               string          `json:"url"`
	PrimaryURL        string          `json:"primary_url"`
	PendingDomains    int             `json:"pending_domains"`
	CurrentDeployment *deploymentJSON `json:"current_deployment"`
	LatestDeployment  *deploymentJSON `json:"latest_deployment"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	// Role is what the requesting user may do on the app: "owner" for the
	// owner, otherwise the member's role (ARCHITECTURE 10.7).
	Role string `json:"role"`
	// Review describes a review app; null for other apps (ARCHITECTURE
	// 9.9).
	Review *reviewJSON `json:"review"`
	// ReviewAppsAllowed is whether the owner lets review apps be created
	// from this app; ReviewApps counts them, null on a review app.
	ReviewAppsAllowed bool               `json:"review_apps_allowed"`
	ReviewApps        *reviewSummaryJSON `json:"review_apps"`
	// Blue/green deploys of the public service (ARCHITECTURE 7.2).
	// AltHostPort is the second port (null until first turned on), WebColor
	// the colour that receives the requests (null while the public service
	// runs as itself). BlueGreenProblems lists what keeps the running
	// release's Compose file from blue/green deploys; null when unknown
	// (not deployed, or deployed before the check).
	BlueGreen             bool                 `json:"blue_green"`
	BlueGreenDrainSeconds int                  `json:"blue_green_drain_seconds"`
	AltHostPort           *int                 `json:"alt_host_port"`
	WebColor              *string              `json:"web_color"`
	BlueGreenProblems     *[]compose.Violation `json:"blue_green_problems"`
}

// blueGreenChecker tells what keeps an app's running release from
// blue/green deploys. *deploy.Engine implements it.
type blueGreenChecker interface {
	BlueGreenProblems(app store.App) ([]compose.Violation, bool)
}

func (s *Server) appToJSON(r *http.Request, app store.App) (appJSON, error) {
	out := appJSON{
		Slug:                  app.Slug,
		RepoFullName:          app.RepoFullName,
		RepoURL:               app.RepoURL,
		DefaultBranch:         app.DefaultBranch,
		ComposePath:           app.ComposePath,
		ComposeSource:         app.ComposeSource,
		WebService:            app.WebService,
		WebPort:               app.WebPort,
		HostPort:              app.HostPort,
		HealthcheckPath:       app.HealthcheckPath,
		DesiredState:          app.DesiredState,
		Environment:           app.Environment,
		RelaxPolicy:           app.RelaxPolicy,
		MemoryLimit:           app.MemoryLimit,
		ServiceMemoryLimits:   app.ServiceMemoryLimits,
		MemoryReservation:     app.MemoryReservation,
		ServiceReservations:   app.ServiceMemoryReservations,
		BackupExcludeVolumes:  app.BackupExcludeVolumes,
		DumpHookService:       app.DumpHookService,
		DumpHookCommand:       app.DumpHookCommand,
		BackupBeforeDeploy:    app.BackupBeforeDeploy,
		DumpIntervalHours:     app.DumpIntervalHours,
		PITRService:           app.PITRService,
		LoadHookService:       app.LoadHookService,
		LoadHookCommand:       app.LoadHookCommand,
		PostLoadService:       app.PostLoadService,
		PostLoadCommand:       app.PostLoadCommand,
		EnvSource:             app.EnvSource,
		EnvExamplePath:        app.EnvExamplePath,
		AccessProtection:      app.AccessProtection,
		ProtectionExempt:      app.ProtectionExemptPaths,
		AutoDeploy:            app.AutoDeploy,
		AutoDeployBranch:      app.AutoDeployBranch,
		AutoDeployWorkflow:    app.AutoDeployWorkflow,
		IdleSleep:             app.IdleSleep,
		IdleSleepMinutes:      app.IdleSleepMinutes,
		SleepingSince:         app.SleepingSince,
		ReviewAppsAllowed:     app.ReviewAppsAllowed,
		BlueGreen:             app.BlueGreen,
		BlueGreenDrainSeconds: app.BlueGreenDrainSeconds,
		WebColor:              optionalString(app.WebColor),
		URL:                   "https://" + app.Slug + "." + s.cfg.BaseDomain,
		CreatedAt:             app.CreatedAt,
		UpdatedAt:             app.UpdatedAt,
	}
	if app.AltHostPort != 0 {
		port := app.AltHostPort
		out.AltHostPort = &port
	}
	if checker, ok := s.deployer.(blueGreenChecker); ok {
		if problems, known := checker.BlueGreenProblems(app); known {
			if problems == nil {
				problems = []compose.Violation{}
			}
			out.BlueGreenProblems = &problems
		}
	}
	lvl, err := s.appLevel(r, app)
	if err != nil {
		return appJSON{}, err
	}
	out.Role = lvl.String()
	// The file may carry literal values, like the variables only these
	// roles read (ARCHITECTURE 6.3).
	if _, viaToken := tokenFrom(r); lvl >= levelManage && !viaToken {
		content := app.ComposeContent
		out.ComposeContent = &content
	} else {
		out.ComposeContentHidden = true
	}
	// The primary URL and the number of domains waiting for DNS let the app
	// list render without one domain request per app.
	out.PrimaryURL = out.URL
	promoteFrom, err := s.promoteFromSlug(r.Context(), app)
	if err != nil {
		return appJSON{}, err
	}
	out.PromoteFrom = promoteFrom
	if out.Review, err = s.reviewToJSON(r.Context(), app); err != nil {
		return appJSON{}, err
	}
	if out.Review == nil {
		sum, err := s.store.SummarizeReviewApps(r.Context(), app.ID)
		if err != nil {
			return appJSON{}, err
		}
		out.ReviewApps = &reviewSummaryJSON{Count: sum.Count, Failed: sum.Failed, Deploying: sum.Deploying}
	}
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
	ComposeSource        *string            `json:"compose_source"`
	ComposeContent       *string            `json:"compose_content"`
	WebService           *string            `json:"web_service"`
	WebPort              *int               `json:"web_port"`
	HealthcheckPath      *string            `json:"healthcheck_path"`
	Environment          *string            `json:"environment"`
	RelaxPolicy          *bool              `json:"relax_policy"`
	MemoryLimit          *string            `json:"memory_limit"`
	ServiceMemoryLimits  *map[string]string `json:"service_memory_limits"`
	MemoryReservation    *string            `json:"memory_reservation"`
	ServiceReservations  *map[string]string `json:"service_memory_reservations"`
	BackupExcludeVolumes *[]string          `json:"backup_exclude_volumes"`
	DumpHookService      *string            `json:"dump_hook_service"`
	DumpHookCommand      *string            `json:"dump_hook_command"`
	BackupBeforeDeploy   *bool              `json:"backup_before_deploy"`
	DumpIntervalHours    *int               `json:"dump_interval_hours"`
	PITRService          *string            `json:"pitr_service"`
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
	AutoDeployWorkflow   *string            `json:"auto_deploy_workflow"`
	IdleSleep            *bool              `json:"idle_sleep"`
	IdleSleepMinutes     *int               `json:"idle_sleep_minutes"`
	BlueGreen            *bool              `json:"blue_green"`
	BlueGreenDrain       *int               `json:"blue_green_drain_seconds"`
	// ReviewAppsAllowed is read by PATCH only (ARCHITECTURE 9.9).
	ReviewAppsAllowed *bool `json:"review_apps_allowed"`
}

// validateReservations checks the memory reservations (ARCHITECTURE 6.2)
// against Docker's minimum and against the limit known when saving: the
// per-service limit, else the app-wide one, else the configured default.
// A repository's own lower limit is only known at deploy time, where the
// override cuts the reservation down to it.
func validateReservations(app store.App, defaultLimit string, errs fieldErrors) {
	limitOf := func(service string) string {
		if v := app.ServiceMemoryLimits[service]; v != "" {
			return v
		}
		if app.MemoryLimit != "" {
			return app.MemoryLimit
		}
		return defaultLimit
	}
	check := func(field, what, value, service string) {
		if value == "" || value == "0" {
			return
		}
		if !memoryPattern.MatchString(value) {
			errs.add(field, what+" must be 0 or a number with an optional unit (b, k, m, g), for example 256m")
			return
		}
		bytes, _ := compose.ParseMemory(value)
		if bytes < compose.MinReservation {
			errs.add(field, what+" must be at least 6m, or 0 to reserve nothing")
			return
		}
		limitText := limitOf(service)
		if limit, ok := compose.ParseMemory(limitText); ok && bytes > limit {
			errs.add(field, fmt.Sprintf("%s must not exceed the memory limit (%s)", what, limitText))
		}
	}
	check("memory_reservation", "the reservation", app.MemoryReservation, "")
	for _, name := range slices.Sorted(maps.Keys(app.ServiceMemoryReservations)) {
		if !servicePattern.MatchString(name) {
			errs.add("service_memory_reservations", fmt.Sprintf("%q is not a valid service name", truncateForError(name)))
			continue
		}
		value := app.ServiceMemoryReservations[name]
		if value == "" {
			errs.add("service_memory_reservations", fmt.Sprintf("the reservation of %s is empty", name))
			continue
		}
		check("service_memory_reservations", "the reservation of "+name, value, name)
	}
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
	repoBefore := [2]string{app.RepoFullName, app.RepoURL}
	if in.RepoFullName != nil {
		app.RepoFullName = strings.TrimSpace(*in.RepoFullName)
	}
	if in.RepoURL != nil {
		app.RepoURL = strings.TrimSpace(*in.RepoURL)
	}
	switch {
	case in.RepoID != nil:
		if *in.RepoID <= 0 {
			errs.add("repo_id", "must be the positive ID of the repository")
		}
		app.RepoID = in.RepoID
	case repoBefore != [2]string{app.RepoFullName, app.RepoURL}:
		// The ID belongs to the repository the app pointed at before. The
		// next deployment records the ID of the new one (ARCHITECTURE 5).
		app.RepoID = nil
	}
	if in.DefaultBranch != nil {
		app.DefaultBranch = strings.TrimSpace(*in.DefaultBranch)
	}
	if in.ComposePath != nil {
		app.ComposePath = strings.TrimSpace(*in.ComposePath)
	}
	if in.ComposeSource != nil {
		app.ComposeSource = strings.TrimSpace(*in.ComposeSource)
	}
	if in.ComposeContent != nil {
		// Textareas of some clients send CRLF line ends.
		app.ComposeContent = strings.ReplaceAll(*in.ComposeContent, "\r\n", "\n")
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
	if in.MemoryReservation != nil {
		app.MemoryReservation = strings.ToLower(strings.TrimSpace(*in.MemoryReservation))
	}
	if in.ServiceReservations != nil {
		app.ServiceMemoryReservations = map[string]string{}
		for name, value := range *in.ServiceReservations {
			app.ServiceMemoryReservations[name] = strings.ToLower(strings.TrimSpace(value))
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
	if in.PITRService != nil {
		app.PITRService = strings.TrimSpace(*in.PITRService)
		if app.PITRService != "" && !servicePattern.MatchString(app.PITRService) {
			errs.add("pitr_service", "is not a valid service name")
		}
	}
	if in.DumpIntervalHours != nil {
		if !slices.Contains(store.AllowedDumpIntervals, *in.DumpIntervalHours) {
			errs.add("dump_interval_hours", "must be one of 1, 2, 3, 4, 6, 8, 12 and 24")
		} else {
			app.DumpIntervalHours = *in.DumpIntervalHours
		}
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
	if in.AutoDeployWorkflow != nil {
		app.AutoDeployWorkflow = strings.TrimSpace(*in.AutoDeployWorkflow)
	}

	if in.IdleSleep != nil {
		app.IdleSleep = *in.IdleSleep
	}
	if in.IdleSleepMinutes != nil {
		app.IdleSleepMinutes = *in.IdleSleepMinutes
	}
	if m := app.IdleSleepMinutes; m < store.MinIdleSleepMinutes || m > store.MaxIdleSleepMinutes {
		errs.add("idle_sleep_minutes", fmt.Sprintf("must be between %d and %d", store.MinIdleSleepMinutes, store.MaxIdleSleepMinutes))
	}

	if in.BlueGreen != nil {
		app.BlueGreen = *in.BlueGreen
	}
	if in.BlueGreenDrain != nil {
		app.BlueGreenDrainSeconds = *in.BlueGreenDrain
	}
	if d := app.BlueGreenDrainSeconds; d < 0 || d > store.MaxBlueGreenDrainSeconds {
		errs.add("blue_green_drain_seconds", fmt.Sprintf("must be between 0 and %d", store.MaxBlueGreenDrainSeconds))
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
	if w := app.AutoDeployWorkflow; w != "" && (!workflowPathPattern.MatchString(w) || len(w) > 255) {
		// GitHub runs only the files directly in .github/workflows.
		errs.add("auto_deploy_workflow", "must be a .yml or .yaml file directly in .github/workflows/")
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
	validateComposeSource(*app, errs)
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

// validateComposeSource checks where the Compose file comes from and the
// file written in koroku (ARCHITECTURE 6.3). What the file configures is
// checked by the deployment, like a repository's file.
func validateComposeSource(app store.App, errs fieldErrors) {
	inline := app.ComposeSource == store.ComposeSourceInline
	if !inline && app.ComposeSource != store.ComposeSourceRepository {
		errs.add("compose_source", "must be repository or inline")
	}
	content := app.ComposeContent
	switch {
	case len(content) > store.MaxComposeContentBytes:
		errs.add("compose_content", fmt.Sprintf("must be at most %d KiB", store.MaxComposeContentBytes>>10))
	case strings.ContainsRune(content, 0):
		errs.add("compose_content", "must be text")
	case !inline:
		// Kept as it is until the app deploys it.
	case strings.TrimSpace(content) == "":
		errs.add("compose_content", "is required when the Compose file is written in koroku")
	default:
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
			// The error names the line; the text is the user's own.
			errs.add("compose_content", "is not a YAML mapping: "+truncateForError(strings.TrimPrefix(err.Error(), "yaml: ")))
		} else if len(doc) == 0 {
			errs.add("compose_content", "is empty: write at least the services")
		}
	}
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
	case reservedSlugs[slug], strings.EqualFold(slug+"."+s.cfg.BaseDomain, s.cfg.ConsoleHost):
		// The console's own hostname can be configured to anything, in any
		// letter case; hostnames are compared without it.
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
		writeMissing(w, missingApp)
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
		DefaultBranch:     "main",
		ComposeSource:     store.ComposeSourceRepository,
		WebService:        "web",
		HealthcheckPath:   "/",
		Environment:       store.EnvironmentStaging,
		EnvSource:         envreq.SourceCompose,
		IdleSleepMinutes:  store.DefaultIdleSleepMinutes,
		DumpIntervalHours: store.DefaultDumpIntervalHours,
	}
	errs := in.apply(&app, s.cfg.Deploy.AllowFileRepos)
	validateReservations(app, s.cfg.Deploy.DefaultMemoryLimit, errs)
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
	s.checkIdleSleep(store.App{}, app, errs)
	if app.BlueGreen {
		// It is set on the deploy tab of an existing app, which gives it
		// its second port (ARCHITECTURE 7.2).
		errs.add("blue_green", "turn it on once the app exists")
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
	envKeys := make([]string, 0, len(body.Env))
	for _, v := range body.Env {
		envKeys = append(envKeys, v.Key)
	}
	s.recordActivity(r, created.ID, actAppCreated, map[string]any{"environment": created.Environment, "env_added": envKeys})
	s.syncRoutes(r, "app created")
	j, err := s.appToJSON(r, created)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

// checkIdleSleep refuses turning idle sleep on where requests cannot be
// counted (ARCHITECTURE 8.4). An app that has it on keeps it.
func (s *Server) checkIdleSleep(before, after store.App, errs fieldErrors) {
	if after.IdleSleep && !before.IdleSleep && !s.idleSleepAvailable() {
		errs.add("idle_sleep", "needs the per-app access logs of Caddy ([caddy] enabled and access_log_dir)")
	}
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
	validateReservations(app, s.cfg.Deploy.DefaultMemoryLimit, errs)
	if in.ReviewAppsAllowed != nil {
		app.ReviewAppsAllowed = *in.ReviewAppsAllowed
	}
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
	s.checkIdleSleep(before, app, errs)
	if app.ReviewAppsAllowed && !before.ReviewAppsAllowed {
		// Only an app review apps can be made from is allowed them.
		reason, err := s.reviewParentProblem(r.Context(), app)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if reason != "" {
			errs.add("review_apps_allowed", "needs a staging app that is fetched through the GitHub App and is not a review app itself")
		}
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if (touchesSafeguards(before, app) || enablesProductionAutoDeploy(before, app) ||
		changesProductionCommands(before, app)) && !s.requireRecentAuth(w, r) {
		return
	}
	promoteBefore, err := s.promoteFromSlug(r.Context(), before)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if app.BlueGreen && app.AltHostPort == 0 {
		// The second port is kept from the first time on (ARCHITECTURE 7.2).
		port, err := s.store.AllocateAltHostPort(r.Context(), app.ID, s.cfg.Deploy.PortRangeStart, s.cfg.Deploy.PortRangeEnd)
		if errors.Is(err, store.ErrNoFreePort) {
			writeValidationError(w, fieldErrors{"blue_green": "no host port is free for the second copy of the public service"})
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		app.AltHostPort = port
	}
	if err := s.store.UpdateApp(r.Context(), app, s.clock.Now()); err != nil {
		s.internalError(w, r, err)
		return
	}
	// Protection, exempt paths and the environment (noindex) shape the
	// proxy configuration; an unchanged one is not reloaded.
	s.syncRoutes(r, "app updated")
	if old := before.PITRService; old != "" && old != app.PITRService && s.pitr != nil {
		// The archiving of the old service is turned off before a
		// deployment removes its mount (ARCHITECTURE 10.9). It restarts
		// the database, so it does not hold the request.
		go func(app store.App) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
			defer cancel()
			if err := s.pitr.Disable(ctx, app, old); err != nil {
				s.log.Error("turning point-in-time recovery off failed", "app", app.Slug, "service", old, "error", err)
			}
		}(before)
	}
	updated, err := s.store.GetAppByID(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	promoteAfter, err := s.promoteFromSlug(r.Context(), updated)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if changes := settingChanges(before, updated, promoteBefore, promoteAfter); len(changes) > 0 {
		s.recordActivity(r, app.ID, actSettingsChanged, map[string]any{"changes": changes})
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
// code comes from, links to other apps, making a production app deploy
// pushes by itself, and allowing review apps.
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
	if before.ReviewAppsAllowed != after.ReviewAppsAllowed {
		// Pull request code would receive the app's variables and data.
		fields = append(fields, "review_apps_allowed")
	}
	if before.RepoFullName != after.RepoFullName || before.RepoURL != after.RepoURL ||
		!sameID(before.RepoID, after.RepoID) {
		fields = append(fields, "repository")
	}
	// The file decides what runs with the app's variables and volumes,
	// like the repository does (ARCHITECTURE 6.3).
	if before.ComposeSource != after.ComposeSource {
		fields = append(fields, "compose_source")
	}
	if before.ComposeContent != after.ComposeContent {
		fields = append(fields, "compose_content")
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
		// The Compose file written in koroku (ARCHITECTURE 6.3).
		before.ComposeSource != after.ComposeSource ||
		before.ComposeContent != after.ComposeContent ||
		// Opening the app to everyone (ARCHITECTURE 8.3).
		(before.AccessProtection && !after.AccessProtection) ||
		// Handing its variables and data to pull requests (ARCHITECTURE 9.9).
		(!before.ReviewAppsAllowed && after.ReviewAppsAllowed) ||
		!slices.Equal(before.ProtectionExemptPaths, after.ProtectionExemptPaths)
}

// enablesProductionAutoDeploy reports whether an update makes a production
// app deploy pushes, or pushes of another branch, or after another
// workflow (or none), without anyone pressing a button. Like deploying it by hand, that needs a recent login
// (ARCHITECTURE 10.3).
func enablesProductionAutoDeploy(before, after store.App) bool {
	if after.Environment != store.EnvironmentProduction || !after.AutoDeploy {
		return false
	}
	return !before.AutoDeploy || before.EffectiveAutoDeployBranch() != after.EffectiveAutoDeployBranch() ||
		before.AutoDeployWorkflow != after.AutoDeployWorkflow
}

// changesProductionCommands reports whether an update changes a command
// that runs inside the containers of a production app: the dump hook (every
// backup runs it), the load hook and the command after a load (they run in
// database copies, once the app is a staging app). Saving one is a way to
// run a command in the app, which needs a recent login like a one-off
// command does (ARCHITECTURE 10.3).
func changesProductionCommands(before, after store.App) bool {
	if before.Environment != store.EnvironmentProduction && after.Environment != store.EnvironmentProduction {
		return false
	}
	return before.DumpHookService != after.DumpHookService ||
		before.DumpHookCommand != after.DumpHookCommand ||
		// Point-in-time recovery writes the settings of that service's
		// database and restarts it (ARCHITECTURE 10.9).
		before.PITRService != after.PITRService ||
		before.LoadHookService != after.LoadHookService ||
		before.LoadHookCommand != after.LoadHookCommand ||
		before.PostLoadService != after.PostLoadService ||
		before.PostLoadCommand != after.PostLoadCommand
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
