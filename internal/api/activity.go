package api

import (
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// The activity of an app (ARCHITECTURE 10.7): who changed its settings,
// variables, domains and the container registry credential, started or stopped it, restored data, started a
// task or a database copy, or changed a member's role on it. Deployments
// are not repeated here; they keep their own history with the actor.
//
// The actions and the shape of their details are part of the API
// (docs/API.md); the console turns them into sentences.
const (
	actAppCreated      = "app.created"          // {"environment"}
	actSettingsChanged = "app.settings_changed" // {"changes": [{"field", "from", "to"} | {"field", "secret": true}]}
	actAppStarted      = "app.started"
	actAppStopped      = "app.stopped"
	actAppRestarted    = "app.restarted"
	// actAppWoken: a sleeping app was woken from the console. Sleeps and
	// wakes by requests are not recorded (ARCHITECTURE 8.4).
	actAppWoken         = "app.woken"
	actEnvChanged       = "env.changed"       // {"added", "changed", "removed": [keys], "renamed": [{"from", "to"}], "replaced"}
	actDomainAdded      = "domain.added"      // {"hostname", "status"}
	actDomainVerified   = "domain.verified"   // {"hostname", "status"}
	actDomainPrimary    = "domain.primary"    // {"hostname", "is_primary"}
	actDomainForced     = "domain.forced"     // {"hostname", "force_active"}
	actDomainDeleted    = "domain.deleted"    // {"hostname"}
	actRegistrySet      = "registry.set"      // {"registry", "username", "created", "token_replaced", "from": {"registry", "username"}}
	actRegistryRemoved  = "registry.removed"  // {"registry", "username"}
	actVolumeRestored   = "backup.restored"   // {"snapshot_id"}
	actDumpDownloaded   = "backup.downloaded" // {"snapshot_id"}
	actDBCopyStarted    = "db_copy.started"   // {"copy_id", "source", "source_kind"}
	actDBCopySent       = "db_copy.sent"      // {"source_kind"}, on the source app
	actTaskRunStarted   = "task_run.started"  // {"run_id", "task_name", "service", "command"}
	actTasksChanged     = "tasks.changed"     // {"added", "changed", "removed": [names], "reordered"}
	actMemberRoleSet    = "member.role_set"   // {"member": {"id", "login"}, "from", "to"}
	actMemberRoleRemove = "member.role_removed"
	// actAutoDeploySkipped is recorded by koroku itself when the workflow
	// an app waits for did not succeed (ARCHITECTURE 5.2).
	actAutoDeploySkipped = "auto_deploy.skipped" // {"workflow", "conclusion", "commit_sha", "branch", "run_url"}
)

// Bounds of GET /api/apps/{slug}/activity.
const (
	activityDefaultLimit = 20
	activityMaxLimit     = 100
)

// recordActivity stores an entry for app by the request's user. The
// operation it describes has already happened, so a failure is logged
// rather than turned into an error response.
func (s *Server) recordActivity(r *http.Request, appID int64, action string, details any) {
	if _, err := s.store.RecordActivity(r.Context(), appID, action, details, s.clock.Now()); err != nil {
		s.log.Error("recording activity failed", "action", action, "error", err)
	}
}

type activityJSON struct {
	ID      int64      `json:"id"`
	At      time.Time  `json:"at"`
	Actor   *actorJSON `json:"actor"`
	Action  string     `json:"action"`
	Details any        `json:"details"`
}

func (s *Server) handleListActivity(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := activityDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > activityMaxLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	var before int64
	if raw := q.Get("before"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_before", "before must be an entry ID")
			return
		}
		before = n
	}
	// One more than asked tells whether another page follows.
	list, err := s.store.ListActivity(r.Context(), app.ID, before, limit+1)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var next *int64
	if len(list) > limit {
		list = list[:limit]
		id := list[limit-1].ID
		next = &id
	}
	out := make([]activityJSON, 0, len(list))
	for _, a := range list {
		out = append(out, activityJSON{
			ID: a.ID, At: a.At, Actor: actorToJSON(a.Actor), Action: a.Action, Details: a.Details,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "next_before": next})
}

// settingField is one app setting as the activity records it. A secret
// setting is recorded as changed without its values.
type settingField struct {
	name   string
	secret bool
	get    func(store.App) any
}

// settingFields are the settings PATCH /api/apps/{slug} can change, by
// their JSON names. promote_from is compared by slug in settingChanges.
var settingFields = []settingField{
	{name: "repo_full_name", get: func(a store.App) any { return a.RepoFullName }},
	{name: "repo_url", get: func(a store.App) any { return a.RepoURL }},
	{name: "default_branch", get: func(a store.App) any { return a.DefaultBranch }},
	{name: "compose_path", get: func(a store.App) any { return a.ComposePath }},
	{name: "web_service", get: func(a store.App) any { return a.WebService }},
	{name: "web_port", get: func(a store.App) any { return a.WebPort }},
	{name: "healthcheck_path", get: func(a store.App) any { return a.HealthcheckPath }},
	{name: "environment", get: func(a store.App) any { return a.Environment }},
	{name: "relax_policy", get: func(a store.App) any { return a.RelaxPolicy }},
	{name: "memory_limit", get: func(a store.App) any { return a.MemoryLimit }},
	{name: "service_memory_limits", get: func(a store.App) any { return nonNilMap(a.ServiceMemoryLimits) }},
	{name: "backup_exclude_volumes", get: func(a store.App) any { return nonNilSlice(a.BackupExcludeVolumes) }},
	{name: "dump_hook_service", get: func(a store.App) any { return a.DumpHookService }},
	{name: "dump_hook_command", get: func(a store.App) any { return a.DumpHookCommand }},
	{name: "backup_before_deploy", get: func(a store.App) any { return a.BackupBeforeDeploy }},
	{name: "load_hook_service", get: func(a store.App) any { return a.LoadHookService }},
	{name: "load_hook_command", get: func(a store.App) any { return a.LoadHookCommand }},
	{name: "post_load_service", get: func(a store.App) any { return a.PostLoadService }},
	{name: "post_load_command", get: func(a store.App) any { return a.PostLoadCommand }},
	{name: "env_source", get: func(a store.App) any { return a.EnvSource }},
	{name: "env_example_path", get: func(a store.App) any { return a.EnvExamplePath }},
	{name: "access_protection", get: func(a store.App) any { return a.AccessProtection }},
	{name: "protection_exempt_paths", get: func(a store.App) any { return nonNilSlice(a.ProtectionExemptPaths) }},
	{name: "auto_deploy", get: func(a store.App) any { return a.AutoDeploy }},
	{name: "auto_deploy_branch", get: func(a store.App) any { return a.AutoDeployBranch }},
	{name: "auto_deploy_workflow", get: func(a store.App) any { return a.AutoDeployWorkflow }},
	{name: "idle_sleep", get: func(a store.App) any { return a.IdleSleep }},
	{name: "idle_sleep_minutes", get: func(a store.App) any { return a.IdleSleepMinutes }},
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return maps.Clone(m)
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

// settingChanges lists what an update changed, with the old and new values
// of settings that are not secret.
func settingChanges(before, after store.App, promoteBefore, promoteAfter string) []map[string]any {
	changes := []map[string]any{}
	for _, f := range settingFields {
		from, to := f.get(before), f.get(after)
		if reflect.DeepEqual(from, to) {
			continue
		}
		if f.secret {
			changes = append(changes, map[string]any{"field": f.name, "secret": true})
			continue
		}
		changes = append(changes, map[string]any{"field": f.name, "from": from, "to": to})
	}
	if promoteBefore != promoteAfter {
		changes = append(changes, map[string]any{"field": "promote_from", "from": promoteBefore, "to": promoteAfter})
	}
	return changes
}

// envChange is the details of env.changed: keys only, never values.
type envChange struct {
	Added    []string        `json:"added"`
	Changed  []string        `json:"changed"`
	Removed  []string        `json:"removed"`
	Renamed  []envRenameJSON `json:"renamed"`
	Replaced bool            `json:"replaced"`
}

type envRenameJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func newEnvChange() envChange {
	return envChange{Added: []string{}, Changed: []string{}, Removed: []string{}, Renamed: []envRenameJSON{}}
}

func (c envChange) empty() bool {
	return len(c.Added) == 0 && len(c.Changed) == 0 && len(c.Removed) == 0 && len(c.Renamed) == 0
}

// taskChanges compares the registered tasks before and after a
// replacement by name.
func taskChanges(before, after []store.Task) map[string]any {
	old := map[string]store.Task{}
	for _, t := range before {
		old[t.Name] = t
	}
	added, changed, removed := []string{}, []string{}, []string{}
	kept := map[string]bool{}
	for _, t := range after {
		prev, ok := old[t.Name]
		switch {
		case !ok:
			added = append(added, t.Name)
		case prev.Service != t.Service || prev.Command != t.Command:
			changed = append(changed, t.Name)
		}
		kept[t.Name] = true
	}
	for _, t := range before {
		if !kept[t.Name] {
			removed = append(removed, t.Name)
		}
	}
	reordered := false
	if len(added) == 0 && len(changed) == 0 && len(removed) == 0 {
		for i := range after {
			if before[i].Name != after[i].Name {
				reordered = true
				break
			}
		}
		if !reordered {
			return nil
		}
	}
	return map[string]any{"added": added, "changed": changed, "removed": removed, "reordered": reordered}
}
