package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"
)

// Desired states of an app.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"
)

// Environment labels of an app. Besides the label and the confirmation
// dialogs, production apps are protected first when the host runs out of
// memory (ARCHITECTURE 6.2); otherwise they are ordinary apps.
const (
	EnvironmentProduction = "production"
	EnvironmentStaging    = "staging"
)

// Bounds and default of App.IdleSleepMinutes (ARCHITECTURE 8.4).
const (
	DefaultIdleSleepMinutes = 30
	MinIdleSleepMinutes     = 10
	MaxIdleSleepMinutes     = 1440
)

// EnvSourceCompose is the default source of required variables for new
// apps (ARCHITECTURE 9.4). The values are defined by package envreq.
const EnvSourceCompose = "compose"

var (
	// ErrConflict is returned when a unique value (slug, hostname) is taken.
	ErrConflict = errors.New("already exists")
	// ErrNoFreePort is returned when the host port range is exhausted.
	ErrNoFreePort = errors.New("no free host port")
)

// App is one deployable Compose project.
type App struct {
	ID                  int64
	Slug                string
	RepoID              *int64
	RepoFullName        string
	RepoURL             string
	DefaultBranch       string
	ComposePath         string
	WebService          string
	WebPort             int
	HostPort            int
	HealthcheckPath     string
	DesiredState        string
	Environment         string
	RelaxPolicy         bool
	MemoryLimit         string
	ServiceMemoryLimits map[string]string
	// MemoryReservation and ServiceMemoryReservations set the memory a
	// production app reserves (ARCHITECTURE 6.2): "" for the default,
	// "0" for none.
	MemoryReservation         string
	ServiceMemoryReservations map[string]string
	// Backup settings (ARCHITECTURE 10.2).
	BackupExcludeVolumes []string
	DumpHookService      string
	DumpHookCommand      string
	BackupBeforeDeploy   bool
	// Database copy settings (ARCHITECTURE 10.6): the load hook reads a
	// dump from stdin; the post-load command runs after a copy.
	LoadHookService string
	LoadHookCommand string
	PostLoadService string
	PostLoadCommand string
	// EnvSource is where the console reads the variables the repository
	// needs: "compose", "dotenv" or "off" (ARCHITECTURE 9.4).
	EnvSource string
	// EnvExamplePath is the .env.example path read in dotenv mode.
	EnvExamplePath string
	// AccessProtection lets only the logged-in console user open the app
	// (ARCHITECTURE 8.3); ProtectionExemptPaths are path prefixes that skip it.
	AccessProtection      bool
	ProtectionExemptPaths []string
	// PromoteFromAppID is the staging app this (production) app promotes
	// releases from (ARCHITECTURE 9.7). nil: not paired.
	PromoteFromAppID *int64
	// AutoDeploy deploys the head of AutoDeployBranch when it is pushed
	// (ARCHITECTURE 5.2). AutoDeployBranch "" follows DefaultBranch.
	AutoDeploy       bool
	AutoDeployBranch string
	// AutoDeployWorkflow is the workflow file (.github/workflows/<name>.yml)
	// whose successful run on the branch deploys the app instead of the
	// push itself (ARCHITECTURE 5.2). "" deploys on push.
	AutoDeployWorkflow string
	// IdleSleep stops the app after IdleSleepMinutes without a request
	// through the proxy; the next request wakes it (ARCHITECTURE 8.4).
	IdleSleep        bool
	IdleSleepMinutes int
	// SleepingSince is when the app went to sleep; nil while it is awake.
	// It is a runtime state: DesiredState stays running.
	SleepingSince *time.Time
	// ReviewAppsAllowed lets review apps be created from this app; only
	// the owner changes it (ARCHITECTURE 9.9).
	ReviewAppsAllowed bool
	// BlueGreen deploys the public service side by side with the running
	// one and switches the proxy once the new one answers (ARCHITECTURE
	// 7.2). BlueGreenDrainSeconds is how long the old colour keeps running
	// after the switch.
	BlueGreen             bool
	BlueGreenDrainSeconds int
	// AltHostPort is the second host port (the green colour); 0 until
	// blue/green was first turned on. Set by AllocateAltHostPort.
	AltHostPort int
	// WebColor is the colour that receives the requests (ColorBlue on
	// HostPort, ColorGreen on AltHostPort); "" while the public service
	// runs as itself on HostPort. Set by the deploy engine.
	WebColor            string
	CurrentDeploymentID *int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// CloneURL is the URL git fetches from.
func (a App) CloneURL() string {
	if a.RepoURL != "" {
		return a.RepoURL
	}
	if a.RepoFullName != "" {
		return "https://github.com/" + a.RepoFullName + ".git"
	}
	return ""
}

// EffectiveAutoDeployBranch is the branch whose pushes deploy the app
// when AutoDeploy is on.
func (a App) EffectiveAutoDeployBranch() string {
	if a.AutoDeployBranch != "" {
		return a.AutoDeployBranch
	}
	return a.DefaultBranch
}

// DeployBranch is the branch a deployment is checked against when its
// request names no ref (ARCHITECTURE 7): the branch automatic deployments
// follow when they are on, otherwise the default branch.
func (a App) DeployBranch() string {
	if a.AutoDeploy {
		return a.EffectiveAutoDeployBranch()
	}
	return a.DefaultBranch
}

// UsesGitHubApp reports whether the app's repository is fetched through
// the GitHub App (repo_full_name without an explicit repo_url). Only such
// apps are mirrored to GitHub deployments and can deploy on push.
func (a App) UsesGitHubApp() bool { return a.RepoFullName != "" && a.RepoURL == "" }

// ProjectName is the Compose project name, which isolates the app's
// containers, networks and volumes.
func (a App) ProjectName() string { return "koroku-" + a.Slug }

// Colours of the public service of a blue/green app (ARCHITECTURE 7.2).
const (
	ColorBlue  = "blue"
	ColorGreen = "green"
)

// Bounds and default of App.BlueGreenDrainSeconds (ARCHITECTURE 7.2).
const (
	DefaultBlueGreenDrainSeconds = 10
	MaxBlueGreenDrainSeconds     = 300
)

// OtherColor returns the colour opposite to color; "" (the public service
// running as itself) counts as blue, so its opposite is green.
func OtherColor(color string) string {
	if color == ColorGreen {
		return ColorBlue
	}
	return ColorGreen
}

// ColorPort is the host port the given colour listens on: blue (and the
// public service running as itself) on HostPort, green on AltHostPort.
func (a App) ColorPort(color string) int {
	if color == ColorGreen {
		return a.AltHostPort
	}
	return a.HostPort
}

// LivePort is the host port the proxy sends the app's requests to.
func (a App) LivePort() int { return a.ColorPort(a.WebColor) }

// ColoredService is the name of the public service's copy of one colour
// (ARCHITECTURE 7.2): <web>-blue or <web>-green.
func (a App) ColoredService(color string) string { return a.WebService + "-" + color }

// LiveService maps a service name of the app to the one whose container
// runs: the public service runs as its WebColor copy while the app has a
// colour. Other names are returned as they are.
func (a App) LiveService(service string) string {
	if a.WebColor != "" && service == a.WebService {
		return a.ColoredService(a.WebColor)
	}
	return service
}

// UsesColors reports whether coloured copies of the public service may
// exist for the app: blue/green is on, or the public service still runs
// in a colour.
func (a App) UsesColors() bool { return a.BlueGreen || a.WebColor != "" }

// PublicServiceOf maps the service of a container to the service it is
// shown as: a coloured copy of the public service is the public service,
// with its colour. Other services are returned as they are, with no colour.
func (a App) PublicServiceOf(service string) (name, color string) {
	if a.UsesColors() && a.WebService != "" {
		for _, c := range []string{ColorBlue, ColorGreen} {
			if service == a.ColoredService(c) {
				return a.WebService, c
			}
		}
	}
	return service, ""
}

const appColumns = `id, slug, repo_id, repo_full_name, repo_url, default_branch, compose_path,
	web_service, web_port, host_port, healthcheck_path, desired_state, environment,
	relax_policy, memory_limit, service_memory_limits, memory_reservation, service_memory_reservations,
	current_deployment_id, created_at, updated_at,
	backup_exclude_volumes, dump_hook_service, dump_hook_command, backup_before_deploy,
	env_source, env_example_path,
	load_hook_service, load_hook_command, post_load_service, post_load_command,
	access_protection, protection_exempt_paths, promote_from_app_id,
	auto_deploy, auto_deploy_branch, auto_deploy_workflow,
	idle_sleep, idle_sleep_minutes, sleeping_since, review_apps_allowed,
	blue_green, blue_green_drain_seconds, alt_host_port, web_color`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanApp(row rowScanner) (App, error) {
	var (
		a                App
		repoID           sql.NullInt64
		currentID        sql.NullInt64
		promoteFrom      sql.NullInt64
		sleeping         sql.NullInt64
		altPort          sql.NullInt64
		webColor         sql.NullString
		limitsJSON       string
		reservationsJSON string
		excludeJSON      string
		exemptJSON       string
		created          int64
		updated          int64
	)
	err := row.Scan(&a.ID, &a.Slug, &repoID, &a.RepoFullName, &a.RepoURL, &a.DefaultBranch, &a.ComposePath,
		&a.WebService, &a.WebPort, &a.HostPort, &a.HealthcheckPath, &a.DesiredState, &a.Environment,
		&a.RelaxPolicy, &a.MemoryLimit, &limitsJSON, &a.MemoryReservation, &reservationsJSON,
		&currentID, &created, &updated,
		&excludeJSON, &a.DumpHookService, &a.DumpHookCommand, &a.BackupBeforeDeploy,
		&a.EnvSource, &a.EnvExamplePath,
		&a.LoadHookService, &a.LoadHookCommand, &a.PostLoadService, &a.PostLoadCommand,
		&a.AccessProtection, &exemptJSON, &promoteFrom,
		&a.AutoDeploy, &a.AutoDeployBranch, &a.AutoDeployWorkflow,
		&a.IdleSleep, &a.IdleSleepMinutes, &sleeping, &a.ReviewAppsAllowed,
		&a.BlueGreen, &a.BlueGreenDrainSeconds, &altPort, &webColor)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, fmt.Errorf("scan app: %w", err)
	}
	if repoID.Valid {
		a.RepoID = &repoID.Int64
	}
	if currentID.Valid {
		a.CurrentDeploymentID = &currentID.Int64
	}
	if promoteFrom.Valid {
		a.PromoteFromAppID = &promoteFrom.Int64
	}
	a.SleepingSince = fromNullMillis(sleeping)
	a.AltHostPort = int(altPort.Int64)
	a.WebColor = webColor.String
	a.ServiceMemoryLimits = map[string]string{}
	if limitsJSON != "" {
		if err := json.Unmarshal([]byte(limitsJSON), &a.ServiceMemoryLimits); err != nil {
			return App{}, fmt.Errorf("decode service_memory_limits of app %s: %w", a.Slug, err)
		}
	}
	a.ServiceMemoryReservations = map[string]string{}
	if reservationsJSON != "" {
		if err := json.Unmarshal([]byte(reservationsJSON), &a.ServiceMemoryReservations); err != nil {
			return App{}, fmt.Errorf("decode service_memory_reservations of app %s: %w", a.Slug, err)
		}
	}
	a.BackupExcludeVolumes = []string{}
	if excludeJSON != "" {
		if err := json.Unmarshal([]byte(excludeJSON), &a.BackupExcludeVolumes); err != nil {
			return App{}, fmt.Errorf("decode backup_exclude_volumes of app %s: %w", a.Slug, err)
		}
	}
	a.ProtectionExemptPaths = []string{}
	if exemptJSON != "" {
		if err := json.Unmarshal([]byte(exemptJSON), &a.ProtectionExemptPaths); err != nil {
			return App{}, fmt.Errorf("decode protection_exempt_paths of app %s: %w", a.Slug, err)
		}
	}
	a.CreatedAt = fromMillis(created)
	a.UpdatedAt = fromMillis(updated)
	return a, nil
}

func encodeStrings(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode string list: %w", err)
	}
	return string(b), nil
}

func encodeLimits(limits map[string]string) (string, error) {
	if limits == nil {
		limits = map[string]string{}
	}
	b, err := json.Marshal(limits)
	if err != nil {
		return "", fmt.Errorf("encode per-service memory settings: %w", err)
	}
	return string(b), nil
}

func nullInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// CreateApp inserts an app and assigns it the lowest free host port in
// [portStart, portEnd]. ID, HostPort and timestamps of the argument are ignored.
func (s *Store) CreateApp(ctx context.Context, app App, portStart, portEnd int, now time.Time) (App, error) {
	return s.CreateAppWithEnv(ctx, app, portStart, portEnd, now, nil)
}

// CreateAppWithEnv is CreateApp that also stores environment variables in
// the same transaction, so that an app never exists without the variables
// it was created with. The values are sealed with the new app's ID, which
// is only known inside the transaction: env receives it and returns the
// variables. A nil env stores none.
func (s *Store) CreateAppWithEnv(ctx context.Context, app App, portStart, portEnd int, now time.Time, env func(appID int64) ([]EnvVar, error)) (App, error) {
	return s.createApp(ctx, app, portStart, portEnd, now, env, nil)
}

// createApp is CreateAppWithEnv with a hook that writes more rows of the
// new app in the same transaction (a review app's, ARCHITECTURE 9.9).
func (s *Store) createApp(ctx context.Context, app App, portStart, portEnd int, now time.Time,
	env func(appID int64) ([]EnvVar, error), extra func(tx *sql.Tx, appID int64) error) (App, error) {
	limits, err := encodeLimits(app.ServiceMemoryLimits)
	if err != nil {
		return App{}, err
	}
	reservations, err := encodeLimits(app.ServiceMemoryReservations)
	if err != nil {
		return App{}, err
	}
	exclude, err := encodeStrings(app.BackupExcludeVolumes)
	if err != nil {
		return App{}, err
	}
	exempt, err := encodeStrings(app.ProtectionExemptPaths)
	if err != nil {
		return App{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return App{}, fmt.Errorf("create app: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Report a taken slug before anything else: it is the error the user can fix.
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE slug = ?`, app.Slug).Scan(&exists); err != nil {
		return App{}, fmt.Errorf("create app: %w", err)
	}
	if exists > 0 {
		return App{}, fmt.Errorf("app %q: %w", app.Slug, ErrConflict)
	}
	port, err := allocatePort(ctx, tx, portStart, portEnd)
	if err != nil {
		return App{}, err
	}
	if app.DesiredState == "" {
		app.DesiredState = DesiredRunning
	}
	if app.Environment == "" {
		app.Environment = EnvironmentStaging
	}
	if app.EnvSource == "" {
		app.EnvSource = EnvSourceCompose
	}
	if app.IdleSleepMinutes == 0 {
		app.IdleSleepMinutes = DefaultIdleSleepMinutes
	}
	if app.BlueGreenDrainSeconds == 0 {
		app.BlueGreenDrainSeconds = DefaultBlueGreenDrainSeconds
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO apps (
			slug, repo_id, repo_full_name, repo_url, default_branch, compose_path,
			web_service, web_port, host_port, healthcheck_path, desired_state, environment,
			relax_policy, memory_limit, service_memory_limits, memory_reservation, service_memory_reservations, created_at, updated_at,
			backup_exclude_volumes, dump_hook_service, dump_hook_command, backup_before_deploy,
			env_source, env_example_path,
			load_hook_service, load_hook_command, post_load_service, post_load_command,
			access_protection, protection_exempt_paths, promote_from_app_id,
			auto_deploy, auto_deploy_branch, auto_deploy_workflow,
			idle_sleep, idle_sleep_minutes, blue_green_drain_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.Slug, nullInt(app.RepoID), app.RepoFullName, app.RepoURL, app.DefaultBranch, app.ComposePath,
		app.WebService, app.WebPort, port, app.HealthcheckPath, app.DesiredState, app.Environment,
		app.RelaxPolicy, app.MemoryLimit, limits, app.MemoryReservation, reservations, toMillis(now), toMillis(now),
		exclude, app.DumpHookService, app.DumpHookCommand, app.BackupBeforeDeploy,
		app.EnvSource, app.EnvExamplePath,
		app.LoadHookService, app.LoadHookCommand, app.PostLoadService, app.PostLoadCommand,
		app.AccessProtection, exempt, nullInt(app.PromoteFromAppID),
		app.AutoDeploy, app.AutoDeployBranch, app.AutoDeployWorkflow,
		app.IdleSleep, app.IdleSleepMinutes, app.BlueGreenDrainSeconds)
	if err != nil {
		if isUniqueViolation(err) {
			return App{}, fmt.Errorf("app %q: %w", app.Slug, ErrConflict)
		}
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	created, err := scanApp(tx.QueryRowContext(ctx, `SELECT `+appColumns+` FROM apps WHERE id = ?`, id))
	if err != nil {
		return App{}, err
	}
	if env != nil {
		vars, err := env(id)
		if err != nil {
			return App{}, fmt.Errorf("create app: %w", err)
		}
		for _, v := range vars {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO env_vars (app_id, key, value_encrypted) VALUES (?, ?, ?)`,
				id, v.Key, v.ValueEncrypted); err != nil {
				if isUniqueViolation(err) {
					return App{}, fmt.Errorf("env var %q: %w", v.Key, ErrConflict)
				}
				return App{}, fmt.Errorf("create app: insert env var: %w", err)
			}
		}
	}
	if extra != nil {
		if err := extra(tx, id); err != nil {
			return App{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return App{}, fmt.Errorf("create app: %w", err)
	}
	return created, nil
}

func allocatePort(ctx context.Context, tx *sql.Tx, start, end int) (int, error) {
	// The second ports of blue/green apps (ARCHITECTURE 7.2) come from
	// the same range.
	rows, err := tx.QueryContext(ctx,
		`SELECT port FROM (SELECT host_port AS port FROM apps UNION SELECT alt_host_port FROM apps WHERE alt_host_port IS NOT NULL)
		WHERE port BETWEEN ? AND ? ORDER BY port`, start, end)
	if err != nil {
		return 0, fmt.Errorf("allocate port: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidate := start
	for rows.Next() {
		var used int
		if err := rows.Scan(&used); err != nil {
			return 0, fmt.Errorf("allocate port: %w", err)
		}
		if used > candidate {
			break
		}
		candidate = used + 1
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("allocate port: %w", err)
	}
	if candidate > end {
		return 0, ErrNoFreePort
	}
	return candidate, nil
}

// GetApp returns the app with the given slug.
func (s *Store) GetApp(ctx context.Context, slug string) (App, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appColumns+` FROM apps WHERE slug = ?`, slug))
}

// GetAppByID returns the app with the given ID.
func (s *Store) GetAppByID(ctx context.Context, id int64) (App, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appColumns+` FROM apps WHERE id = ?`, id))
}

// ListApps returns every app ordered by slug.
func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appColumns+` FROM apps ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var apps []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	return apps, nil
}

// UpdateApp saves the user-editable settings of an app. Slug, host ports,
// desired state, the sleeping state, the colour of the public service and
// the current deployment are not changed here.
func (s *Store) UpdateApp(ctx context.Context, app App, now time.Time) error {
	limits, err := encodeLimits(app.ServiceMemoryLimits)
	if err != nil {
		return err
	}
	reservations, err := encodeLimits(app.ServiceMemoryReservations)
	if err != nil {
		return err
	}
	exclude, err := encodeStrings(app.BackupExcludeVolumes)
	if err != nil {
		return err
	}
	exempt, err := encodeStrings(app.ProtectionExemptPaths)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET
			repo_id = ?, repo_full_name = ?, repo_url = ?, default_branch = ?, compose_path = ?,
			web_service = ?, web_port = ?, healthcheck_path = ?, environment = ?,
			relax_policy = ?, memory_limit = ?, service_memory_limits = ?,
			memory_reservation = ?, service_memory_reservations = ?, updated_at = ?,
			backup_exclude_volumes = ?, dump_hook_service = ?, dump_hook_command = ?, backup_before_deploy = ?,
			env_source = ?, env_example_path = ?,
			load_hook_service = ?, load_hook_command = ?, post_load_service = ?, post_load_command = ?,
			access_protection = ?, protection_exempt_paths = ?, promote_from_app_id = ?,
			auto_deploy = ?, auto_deploy_branch = ?, auto_deploy_workflow = ?,
			idle_sleep = ?, idle_sleep_minutes = ?, review_apps_allowed = ?,
			blue_green = ?, blue_green_drain_seconds = ?
		WHERE id = ?`,
		nullInt(app.RepoID), app.RepoFullName, app.RepoURL, app.DefaultBranch, app.ComposePath,
		app.WebService, app.WebPort, app.HealthcheckPath, app.Environment,
		app.RelaxPolicy, app.MemoryLimit, limits,
		app.MemoryReservation, reservations, toMillis(now),
		exclude, app.DumpHookService, app.DumpHookCommand, app.BackupBeforeDeploy,
		app.EnvSource, app.EnvExamplePath,
		app.LoadHookService, app.LoadHookCommand, app.PostLoadService, app.PostLoadCommand,
		app.AccessProtection, exempt, nullInt(app.PromoteFromAppID),
		app.AutoDeploy, app.AutoDeployBranch, app.AutoDeployWorkflow,
		app.IdleSleep, app.IdleSleepMinutes, app.ReviewAppsAllowed,
		app.BlueGreen, app.BlueGreenDrainSeconds, app.ID)
	if err != nil {
		return fmt.Errorf("update app: %w", err)
	}
	return requireOneRow(res, "update app")
}

// RecordAppRepoID gives the app the ID of its GitHub repository when it
// has none, and returns the ID the app has afterwards. An ID the app
// already has is never replaced: the first reading of a repository's ID is
// trusted and every later one is compared with it (ARCHITECTURE 5). One
// statement, so that two recordings cannot both win.
func (s *Store) RecordAppRepoID(ctx context.Context, appID, repoID int64) (int64, error) {
	var recorded int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE apps SET repo_id = COALESCE(repo_id, ?) WHERE id = ? RETURNING repo_id`,
		repoID, appID).Scan(&recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("record repository id of app %d: %w", appID, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("record repository id of app %d: %w", appID, err)
	}
	return recorded, nil
}

// AllocateAltHostPort gives the app its second host port (ARCHITECTURE
// 7.2), the lowest port in [portStart, portEnd] that no app uses as either
// port, and returns it. An app that has one keeps it.
func (s *Store) AllocateAltHostPort(ctx context.Context, appID int64, portStart, portEnd int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("allocate second port: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var current sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT alt_host_port FROM apps WHERE id = ?`, appID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("allocate second port: %w", ErrNotFound)
		}
		return 0, fmt.Errorf("allocate second port: %w", err)
	}
	if current.Valid {
		return int(current.Int64), nil
	}
	port, err := allocatePort(ctx, tx, portStart, portEnd)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE apps SET alt_host_port = ? WHERE id = ?`, port, appID); err != nil {
		return 0, fmt.Errorf("allocate second port: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("allocate second port: %w", err)
	}
	return port, nil
}

// SetAppWebColor records the colour that receives the app's requests
// (ARCHITECTURE 7.2); "" stores NULL, for a public service running as
// itself. The proxy configuration is built from it.
func (s *Store) SetAppWebColor(ctx context.Context, appID int64, color string) error {
	var value sql.NullString
	switch color {
	case "":
	case ColorBlue, ColorGreen:
		value = sql.NullString{String: color, Valid: true}
	default:
		return fmt.Errorf("set web colour: unknown colour %q", color)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET web_color = ? WHERE id = ?`, value, appID)
	if err != nil {
		return fmt.Errorf("set web colour: %w", err)
	}
	return requireOneRow(res, "set web colour")
}

// SetAppDesiredState records whether the app should be running or stopped.
// Either ends a sleep (ARCHITECTURE 8.4): a stopped app does not wake on a
// request, and a started one is awake.
func (s *Store) SetAppDesiredState(ctx context.Context, appID int64, state string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE apps SET desired_state = ?, sleeping_since = NULL, updated_at = ? WHERE id = ?`, state, toMillis(now), appID)
	if err != nil {
		return fmt.Errorf("set desired state: %w", err)
	}
	return requireOneRow(res, "set desired state")
}

// MarkAppSleeping records that a running, deployed app went to sleep at
// now (ARCHITECTURE 8.4). It reports false, changing nothing, when the app
// is stopped, not deployed or already asleep.
func (s *Store) MarkAppSleeping(ctx context.Context, appID int64, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET sleeping_since = ?
		WHERE id = ? AND desired_state = ? AND current_deployment_id IS NOT NULL AND sleeping_since IS NULL`,
		toMillis(now), appID, DesiredRunning)
	if err != nil {
		return false, fmt.Errorf("mark app sleeping: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark app sleeping: %w", err)
	}
	return n == 1, nil
}

// MarkAppAwake ends the sleep of an app. It reports whether the app was
// asleep.
func (s *Store) MarkAppAwake(ctx context.Context, appID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE apps SET sleeping_since = NULL WHERE id = ? AND sleeping_since IS NOT NULL`, appID)
	if err != nil {
		return false, fmt.Errorf("mark app awake: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark app awake: %w", err)
	}
	return n == 1, nil
}

// DeleteApp removes an app with its env vars and deployments.
func (s *Store) DeleteApp(ctx context.Context, appID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM apps WHERE id = ?`, appID)
	if err != nil {
		return fmt.Errorf("delete app: %w", err)
	}
	return requireOneRow(res, "delete app")
}

// AutoDeployApps returns the slugs of the apps that deploy on push
// (ARCHITECTURE 5.2), reading the database at path without changing it:
// it opens the file read-only and applies no migrations. `koroku check`
// uses it next to a running koroku. A missing database, or one from before
// the feature, has no such apps.
func AutoDeployApps(ctx context.Context, path string) ([]string, error) {
	return readOnlySlugs(ctx, path, `SELECT slug FROM apps WHERE auto_deploy = 1 ORDER BY slug`,
		"list apps that deploy on push")
}

// WorkflowGatedApps returns, like AutoDeployApps, the slugs of the apps
// that deploy on push only after a GitHub Actions workflow succeeded
// (ARCHITECTURE 5.2).
func WorkflowGatedApps(ctx context.Context, path string) ([]string, error) {
	return readOnlySlugs(ctx, path,
		`SELECT slug FROM apps WHERE auto_deploy = 1 AND auto_deploy_workflow != '' ORDER BY slug`,
		"list apps that wait for a workflow")
}

// readOnlySlugs runs a query of slugs on the database at path opened
// read-only. A missing file, table or column yields no slugs.
func readOnlySlugs(ctx context.Context, path, query, what string) ([]string, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	params := url.Values{}
	params.Add("mode", "ro")
	params.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "no such column") || strings.Contains(msg, "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func requireOneRow(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite reports constraint failures as text.
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
