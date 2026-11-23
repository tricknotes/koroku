package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tricknotes/koroku/internal/store"
)

// Who may do what (ARCHITECTURE 10.7). The owner (the configured
// allowed_user_id) may do everything. Members hold one role per app, and
// every API route declares, where it is registered, what it needs: any
// logged-in user, the owner, or a level of role on the app the route acts
// on. authorize enforces that before the handler runs, so a handler never
// has to remember to check.

// level orders what a user may do on an app.
type level int

const (
	levelNone level = iota
	levelView
	levelDeploy
	levelManage
	levelOwner
)

func roleLevel(role string) level {
	switch role {
	case store.RoleView:
		return levelView
	case store.RoleDeploy:
		return levelDeploy
	case store.RoleManage:
		return levelManage
	}
	return levelNone
}

// String is the name the API uses for a level, in roles and in the
// details of 403 forbidden_role.
func (l level) String() string {
	switch l {
	case levelView:
		return store.RoleView
	case levelDeploy:
		return store.RoleDeploy
	case levelManage:
		return store.RoleManage
	case levelOwner:
		return "owner"
	}
	return ""
}

// scope says how a route finds the app it acts on.
type scope int

const (
	// scopeUser routes are open to every logged-in user; the handler
	// limits what a member sees (its own sessions, the apps it has a role
	// on).
	scopeUser scope = iota
	// scopeOwner routes are for the owner only.
	scopeOwner
	// scopeApp routes act on the app named by {slug}.
	scopeApp
	// scopeDeployment, scopeTaskRun and scopeDBCopy routes act on the app
	// of the record named by {id}.
	scopeDeployment
	scopeTaskRun
	scopeDBCopy
	// scopeRepo routes read the repository {owner}/{repo}: a member needs
	// the level on an app deployed from it.
	scopeRepo
	// scopeAnyApp routes need the level on at least one app.
	scopeAnyApp
)

// access is what a route requires.
type access struct {
	scope scope
	level level
}

var (
	anyUser   = access{scope: scopeUser}
	ownerOnly = access{scope: scopeOwner, level: levelOwner}
)

func onApp(l level) access        { return access{scope: scopeApp, level: l} }
func onDeployment(l level) access { return access{scope: scopeDeployment, level: l} }
func onTaskRun(l level) access    { return access{scope: scopeTaskRun, level: l} }
func onDBCopy(l level) access     { return access{scope: scopeDBCopy, level: l} }
func onRepo(l level) access       { return access{scope: scopeRepo, level: l} }
func onAnyApp(l level) access     { return access{scope: scopeAnyApp, level: l} }

// actor is the user a request acts as, from its session or API token.
type actor struct {
	UserID int64
	Login  string
	Owner  bool
}

type actorKey struct{}

func actorFrom(r *http.Request) (actor, bool) {
	a, ok := r.Context().Value(actorKey{}).(actor)
	return a, ok
}

// withActor attaches the acting user to the request, for authorize, the
// handlers, the request log and the records an operation creates.
func withActor(r *http.Request, a actor) *http.Request {
	ctx := context.WithValue(r.Context(), actorKey{}, a)
	ctx = store.WithActor(ctx, store.Actor{UserID: a.UserID, Login: a.Login})
	if info := requestInfoFrom(r); info != nil {
		info.login = a.Login
	}
	return r.WithContext(ctx)
}

// isOwner reports whether the request acts as the owner.
func isOwner(r *http.Request) bool {
	a, ok := actorFrom(r)
	return ok && a.Owner
}

// userAllowed reports whether a GitHub user may log in and keep using a
// session or token: the owner, or a member with at least one role.
func (s *Server) userAllowed(ctx context.Context, userID int64) (bool, error) {
	if s.auth.AllowedUserID != 0 && userID == s.auth.AllowedUserID {
		return true, nil
	}
	return s.store.HasAnyRole(ctx, userID)
}

type levelKey struct{}

// appLevel is what the request's user may do on app.
func (s *Server) appLevel(r *http.Request, app store.App) (level, error) {
	a, ok := actorFrom(r)
	if !ok {
		return levelNone, nil
	}
	if a.Owner {
		return levelOwner, nil
	}
	role, err := s.store.AppRole(r.Context(), app.ID, a.UserID)
	if err != nil {
		return levelNone, err
	}
	return roleLevel(role), nil
}

// requestLevel is the level authorize found for the request's app.
func requestLevel(r *http.Request) level {
	if isOwner(r) {
		return levelOwner
	}
	l, _ := r.Context().Value(levelKey{}).(level)
	return l
}

// missing is the 404 for one kind of record. A record the user has no role
// on and a record that does not exist get the same one, byte for byte: a
// member must not be able to tell which apps, or which ids, exist
// (ARCHITECTURE 10.7). authorize and the handlers both answer through
// writeMissing.
type missing struct{ code, message string }

var (
	missingApp        = missing{"app_not_found", "no such app"}
	missingDeployment = missing{"deployment_not_found", "no such deployment"}
	missingTaskRun    = missing{"task_run_not_found", "no such task run"}
	missingDBCopy     = missing{"db_copy_not_found", "no such database copy"}
)

func writeMissing(w http.ResponseWriter, m missing) {
	writeError(w, http.StatusNotFound, m.code, m.message)
}

// writeForbiddenRole refuses an operation the user's role does not allow.
func writeForbiddenRole(w http.ResponseWriter, required level) {
	writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
		Code:    "forbidden_role",
		Message: "your role does not allow this",
		Details: map[string]any{"required": required.String()},
	}})
}

// authorize wraps a handler with the check its route declares.
func (s *Server) authorize(a access, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		act, ok := actorFrom(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "login required")
			return
		}
		if act.Owner || a.scope == scopeUser {
			next.ServeHTTP(w, r)
			return
		}
		if a.scope == scopeOwner {
			writeForbiddenRole(w, levelOwner)
			return
		}
		if a.scope == scopeRepo || a.scope == scopeAnyApp {
			allowed, err := s.memberReaches(r, act, a)
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			if !allowed {
				writeForbiddenRole(w, a.level)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		appID, kind, err := s.routeApp(r, a.scope)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if appID == 0 {
			// Unknown or malformed: the handler answers with its own 404
			// or 400, as it does for the owner.
			next.ServeHTTP(w, r)
			return
		}
		role, err := s.store.AppRole(r.Context(), appID, act.UserID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		got := roleLevel(role)
		if got == levelNone {
			// An app the member has no role on does not exist for it.
			writeMissing(w, kind)
			return
		}
		if got < a.level {
			writeForbiddenRole(w, a.level)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), levelKey{}, got)))
	})
}

// routeApp finds the app a request acts on. It returns 0 when the record
// does not exist, along with the 404 a member without a role gets.
func (s *Server) routeApp(r *http.Request, sc scope) (appID int64, kind missing, err error) {
	ctx := r.Context()
	if sc == scopeApp {
		app, err := s.store.GetApp(ctx, r.PathValue("slug"))
		if errors.Is(err, store.ErrNotFound) {
			return 0, missingApp, nil
		}
		return app.ID, missingApp, err
	}
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	switch sc {
	case scopeDeployment:
		kind = missingDeployment
		if perr == nil {
			var d store.Deployment
			d, err = s.store.GetDeployment(ctx, id)
			appID = d.AppID
		}
	case scopeTaskRun:
		kind = missingTaskRun
		if perr == nil {
			var run store.TaskRun
			run, err = s.store.GetTaskRun(ctx, id)
			appID = run.AppID
		}
	case scopeDBCopy:
		kind = missingDBCopy
		if perr == nil {
			var c store.DBCopy
			c, err = s.store.GetDBCopy(ctx, id)
			appID = c.TargetAppID
		}
	}
	if perr != nil || errors.Is(err, store.ErrNotFound) {
		return 0, kind, nil
	}
	return appID, kind, err
}

// memberReaches reports whether a member holds the level on an app that
// the route allows: any app (scopeAnyApp), or an app deployed from the
// repository in the path (scopeRepo).
func (s *Server) memberReaches(r *http.Request, act actor, a access) (bool, error) {
	roles, err := s.store.RolesOf(r.Context(), act.UserID)
	if err != nil {
		return false, err
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	for appID, role := range roles {
		if roleLevel(role) < a.level {
			continue
		}
		if a.scope == scopeAnyApp {
			return true, nil
		}
		app, err := s.store.GetAppByID(r.Context(), appID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if strings.EqualFold(app.RepoFullName, repo) {
			return true, nil
		}
	}
	return false, nil
}

// route is one API endpoint and what it requires.
type route struct {
	pattern string
	access  access
	handler http.HandlerFunc
}
