package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// Members (ARCHITECTURE 10.7) are GitHub users the owner lets into the
// console with a role on some apps. Only the owner manages them.

// UserLookup resolves a GitHub login to the account's numeric ID.
type UserLookup interface {
	UserByLogin(ctx context.Context, login string) (github.User, error)
}

type memberRoleJSON struct {
	App  string `json:"app"`
	Role string `json:"role"`
}

type memberJSON struct {
	GitHubUserID int64            `json:"github_user_id"`
	Login        string           `json:"login"`
	AvatarURL    string           `json:"avatar_url"`
	AddedAt      time.Time        `json:"added_at"`
	Roles        []memberRoleJSON `json:"roles"`
}

// memberToJSON names the apps of the roles by slug, in slug order.
func memberToJSON(m store.Member, slugs map[int64]string) memberJSON {
	out := memberJSON{
		GitHubUserID: m.GitHubUserID, Login: m.Login, AvatarURL: m.AvatarURL, AddedAt: m.AddedAt,
		Roles: []memberRoleJSON{},
	}
	for appID, role := range m.Roles {
		if slug, ok := slugs[appID]; ok {
			out.Roles = append(out.Roles, memberRoleJSON{App: slug, Role: role})
		}
	}
	sort.Slice(out.Roles, func(i, j int) bool { return out.Roles[i].App < out.Roles[j].App })
	return out
}

func (s *Server) appSlugs(ctx context.Context) (map[int64]string, error) {
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	slugs := make(map[int64]string, len(apps))
	for _, app := range apps {
		slugs[app.ID] = app.Slug
	}
	return slugs, nil
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	members, err := s.store.ListMembers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	slugs, err := s.appSlugs(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]memberJSON, 0, len(members))
	for _, m := range members {
		out = append(out, memberToJSON(m, slugs))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner":   map[string]any{"github_user_id": s.auth.AllowedUserID},
		"members": out,
	})
}

// writeMember responds with one member as it is now.
func (s *Server) writeMember(w http.ResponseWriter, r *http.Request, status int, userID int64) {
	m, err := s.store.GetMember(r.Context(), userID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	slugs, err := s.appSlugs(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, status, memberToJSON(m, slugs))
}

// handleAddMember resolves a GitHub login to its user ID and adds the user
// without roles. Letting someone in needs a recent login, like issuing an
// API token: a stolen session cookie alone must not open the console to
// another account.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Login string `json:"login"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	login := strings.TrimPrefix(strings.TrimSpace(body.Login), "@")
	if !github.ValidLogin(login) {
		writeValidationError(w, fieldErrors{"login": "is not a GitHub user name"})
		return
	}
	if !s.requireRecentAuth(w, r) {
		return
	}
	if s.users == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "GitHub user lookup is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	user, err := s.users.UserByLogin(ctx, login)
	switch {
	case errors.Is(err, github.ErrNotFound):
		writeError(w, http.StatusNotFound, "user_not_found", "no GitHub user has this name")
		return
	case err != nil:
		s.writeGitHubError(w, r, err)
		return
	}
	if user.ID == s.auth.AllowedUserID {
		writeError(w, http.StatusUnprocessableEntity, "is_owner", "this user is the owner")
		return
	}
	err = s.store.AddMember(r.Context(), store.Member{
		GitHubUserID: user.ID, Login: user.Login, AvatarURL: user.AvatarURL, AddedAt: s.clock.Now(),
	})
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "member_exists", "this user is a member already")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.log.Info("member added", "github_user_id", user.ID, "login", user.Login)
	s.writeMember(w, r, http.StatusCreated, user.ID)
}

// memberID parses {id}, writing a 404 when it is not a member.
func (s *Server) memberID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "member_not_found", "no such member")
		return 0, false
	}
	_, err = s.store.GetMember(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "member_not_found", "no such member")
		return 0, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return 0, false
	}
	return id, true
}

// handleDeleteMember removes a member with its roles, sessions and API
// tokens. Taking access away needs no recent login.
func (s *Server) handleDeleteMember(w http.ResponseWriter, r *http.Request) {
	id, ok := s.memberID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteMember(r.Context(), id); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.log.Info("member removed", "github_user_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// handlePutMemberRole gives a member a role on an app. Granting or raising
// a role needs a recent login; lowering one does not.
func (s *Server) handlePutMemberRole(w http.ResponseWriter, r *http.Request) {
	id, ok := s.memberID(w, r)
	if !ok {
		return
	}
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !store.ValidRole(body.Role) {
		writeValidationError(w, fieldErrors{"role": "must be view, deploy or manage"})
		return
	}
	current, err := s.store.AppRole(r.Context(), app.ID, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if roleLevel(body.Role) > roleLevel(current) && !s.requireRecentAuth(w, r) {
		return
	}
	if err := s.store.SetAppRole(r.Context(), app.ID, id, body.Role); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.log.Info("member role set", "github_user_id", id, "app", app.Slug, "role", body.Role)
	s.writeMember(w, r, http.StatusOK, id)
}

// handleDeleteMemberRole takes a member's role on an app away. A member left
// without any role can no longer log in: its sessions and tokens end.
func (s *Server) handleDeleteMemberRole(w http.ResponseWriter, r *http.Request) {
	id, ok := s.memberID(w, r)
	if !ok {
		return
	}
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if err := s.store.SetAppRole(r.Context(), app.ID, id, ""); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.revokeCredentialsWithoutAccess(r)
	s.log.Info("member role removed", "github_user_id", id, "app", app.Slug)
	s.writeMember(w, r, http.StatusOK, id)
}

// revokeCredentialsWithoutAccess ends the sessions and tokens of users who
// lost their last role. Every request checks access anyway, so a failure is
// only logged.
func (s *Server) revokeCredentialsWithoutAccess(r *http.Request) {
	if s.auth.InsecureNoAuth {
		return
	}
	n, err := s.store.RevokeCredentialsWithoutAccess(r.Context(), s.auth.AllowedUserID)
	if err != nil {
		s.log.Error("revoking credentials of users without a role failed", "error", err)
		return
	}
	if n > 0 {
		s.log.Info("revoked credentials of users without a role", "count", n)
	}
}
