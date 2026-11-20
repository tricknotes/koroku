package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Desired states of an app.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"
)

// Environment labels of an app. They are for display and confirmation
// dialogs only; staging and production are otherwise ordinary apps.
const (
	EnvironmentProduction = "production"
	EnvironmentStaging    = "staging"
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
	// Backup settings (ARCHITECTURE 10.2).
	BackupExcludeVolumes []string
	DumpHookService      string
	DumpHookCommand      string
	BackupBeforeDeploy   bool
	// EnvSource is where the console reads the variables the repository
	// needs: "compose", "dotenv" or "off" (ARCHITECTURE 9.4).
	EnvSource string
	// EnvExamplePath is the .env.example path read in dotenv mode.
	EnvExamplePath string
	// GroupID is the group whose network GroupServices join (ARCHITECTURE
	// 6.1). Membership is changed with SetAppGroup only.
	GroupID             *int64
	GroupServices       []string
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

// ProjectName is the Compose project name, which isolates the app's
// containers, networks and volumes.
func (a App) ProjectName() string { return "koroku-" + a.Slug }

const appColumns = `id, slug, repo_id, repo_full_name, repo_url, default_branch, compose_path,
	web_service, web_port, host_port, healthcheck_path, desired_state, environment,
	relax_policy, memory_limit, service_memory_limits, current_deployment_id, created_at, updated_at,
	backup_exclude_volumes, dump_hook_service, dump_hook_command, backup_before_deploy,
	env_source, env_example_path, group_id, group_services`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanApp(row rowScanner) (App, error) {
	var (
		a           App
		repoID      sql.NullInt64
		currentID   sql.NullInt64
		groupID     sql.NullInt64
		groupJSON   string
		limitsJSON  string
		excludeJSON string
		created     int64
		updated     int64
	)
	err := row.Scan(&a.ID, &a.Slug, &repoID, &a.RepoFullName, &a.RepoURL, &a.DefaultBranch, &a.ComposePath,
		&a.WebService, &a.WebPort, &a.HostPort, &a.HealthcheckPath, &a.DesiredState, &a.Environment,
		&a.RelaxPolicy, &a.MemoryLimit, &limitsJSON, &currentID, &created, &updated,
		&excludeJSON, &a.DumpHookService, &a.DumpHookCommand, &a.BackupBeforeDeploy,
		&a.EnvSource, &a.EnvExamplePath, &groupID, &groupJSON)
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
	if groupID.Valid {
		a.GroupID = &groupID.Int64
	}
	a.GroupServices = []string{}
	if groupJSON != "" {
		if err := json.Unmarshal([]byte(groupJSON), &a.GroupServices); err != nil {
			return App{}, fmt.Errorf("decode group_services of app %s: %w", a.Slug, err)
		}
	}
	a.ServiceMemoryLimits = map[string]string{}
	if limitsJSON != "" {
		if err := json.Unmarshal([]byte(limitsJSON), &a.ServiceMemoryLimits); err != nil {
			return App{}, fmt.Errorf("decode service_memory_limits of app %s: %w", a.Slug, err)
		}
	}
	a.BackupExcludeVolumes = []string{}
	if excludeJSON != "" {
		if err := json.Unmarshal([]byte(excludeJSON), &a.BackupExcludeVolumes); err != nil {
			return App{}, fmt.Errorf("decode backup_exclude_volumes of app %s: %w", a.Slug, err)
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
		return "", fmt.Errorf("encode service_memory_limits: %w", err)
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
	limits, err := encodeLimits(app.ServiceMemoryLimits)
	if err != nil {
		return App{}, err
	}
	exclude, err := encodeStrings(app.BackupExcludeVolumes)
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
	res, err := tx.ExecContext(ctx, `INSERT INTO apps (
			slug, repo_id, repo_full_name, repo_url, default_branch, compose_path,
			web_service, web_port, host_port, healthcheck_path, desired_state, environment,
			relax_policy, memory_limit, service_memory_limits, created_at, updated_at,
			backup_exclude_volumes, dump_hook_service, dump_hook_command, backup_before_deploy,
			env_source, env_example_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.Slug, nullInt(app.RepoID), app.RepoFullName, app.RepoURL, app.DefaultBranch, app.ComposePath,
		app.WebService, app.WebPort, port, app.HealthcheckPath, app.DesiredState, app.Environment,
		app.RelaxPolicy, app.MemoryLimit, limits, toMillis(now), toMillis(now),
		exclude, app.DumpHookService, app.DumpHookCommand, app.BackupBeforeDeploy,
		app.EnvSource, app.EnvExamplePath)
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
	if err := tx.Commit(); err != nil {
		return App{}, fmt.Errorf("create app: %w", err)
	}
	return created, nil
}

func allocatePort(ctx context.Context, tx *sql.Tx, start, end int) (int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT host_port FROM apps WHERE host_port BETWEEN ? AND ? ORDER BY host_port`, start, end)
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

// UpdateApp saves the user-editable settings of an app. Slug, host port,
// desired state and the current deployment are not changed here.
func (s *Store) UpdateApp(ctx context.Context, app App, now time.Time) error {
	limits, err := encodeLimits(app.ServiceMemoryLimits)
	if err != nil {
		return err
	}
	exclude, err := encodeStrings(app.BackupExcludeVolumes)
	if err != nil {
		return err
	}
	// A group keeps one environment (ARCHITECTURE 6.1); the app has to
	// leave its group before its environment changes.
	var groupEnv string
	err = s.db.QueryRowContext(ctx, `SELECT g.environment FROM apps a JOIN app_groups g ON g.id = a.group_id
		WHERE a.id = ?`, app.ID).Scan(&groupEnv)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("update app: %w", err)
	case groupEnv != app.Environment:
		return ErrGroupEnvironment
	}
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET
			repo_id = ?, repo_full_name = ?, repo_url = ?, default_branch = ?, compose_path = ?,
			web_service = ?, web_port = ?, healthcheck_path = ?, environment = ?,
			relax_policy = ?, memory_limit = ?, service_memory_limits = ?, updated_at = ?,
			backup_exclude_volumes = ?, dump_hook_service = ?, dump_hook_command = ?, backup_before_deploy = ?,
			env_source = ?, env_example_path = ?
		WHERE id = ?`,
		nullInt(app.RepoID), app.RepoFullName, app.RepoURL, app.DefaultBranch, app.ComposePath,
		app.WebService, app.WebPort, app.HealthcheckPath, app.Environment,
		app.RelaxPolicy, app.MemoryLimit, limits, toMillis(now),
		exclude, app.DumpHookService, app.DumpHookCommand, app.BackupBeforeDeploy,
		app.EnvSource, app.EnvExamplePath, app.ID)
	if err != nil {
		return fmt.Errorf("update app: %w", err)
	}
	return requireOneRow(res, "update app")
}

// SetAppDesiredState records whether the app should be running or stopped.
func (s *Store) SetAppDesiredState(ctx context.Context, appID int64, state string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE apps SET desired_state = ?, updated_at = ? WHERE id = ?`, state, toMillis(now), appID)
	if err != nil {
		return fmt.Errorf("set desired state: %w", err)
	}
	return requireOneRow(res, "set desired state")
}

// DeleteApp removes an app with its env vars and deployments.
func (s *Store) DeleteApp(ctx context.Context, appID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM apps WHERE id = ?`, appID)
	if err != nil {
		return fmt.Errorf("delete app: %w", err)
	}
	return requireOneRow(res, "delete app")
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
