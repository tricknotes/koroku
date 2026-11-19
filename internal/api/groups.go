package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// GroupManager changes groups and memberships together with their Docker
// networks (ARCHITECTURE 6.1).
type GroupManager interface {
	CreateGroup(ctx context.Context, name, environment string) (store.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	SetGroup(ctx context.Context, app store.App, groupID *int64, services []string) error
}

type groupMemberJSON struct {
	Slug      string   `json:"slug"`
	Services  []string `json:"services"`
	Hostnames []string `json:"hostnames"`
}

type groupJSON struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Environment string            `json:"environment"`
	Network     string            `json:"network"`
	Members     []groupMemberJSON `json:"members"`
	CreatedAt   time.Time         `json:"created_at"`
}

func (s *Server) groupToJSON(r *http.Request, g store.Group) (groupJSON, error) {
	members, err := s.store.ListGroupMembers(r.Context(), g.ID)
	if err != nil {
		return groupJSON{}, err
	}
	out := groupJSON{
		ID: g.ID, Name: g.Name, Environment: g.Environment, Network: g.Network(),
		Members: []groupMemberJSON{}, CreatedAt: g.CreatedAt,
	}
	for _, app := range members {
		m := groupMemberJSON{Slug: app.Slug, Services: app.GroupServices, Hostnames: []string{}}
		for _, service := range app.GroupServices {
			m.Hostnames = append(m.Hostnames, compose.GroupHostname(service, app.Slug))
		}
		out.Members = append(out.Members, m)
	}
	return out, nil
}

func (s *Server) writeGroup(w http.ResponseWriter, r *http.Request, status int, g store.Group) {
	j, err := s.groupToJSON(r, g)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, status, j)
}

func (s *Server) requireGroups(w http.ResponseWriter) bool {
	if s.groups == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "groups are not available")
		return false
	}
	return true
}

func (s *Server) loadGroup(w http.ResponseWriter, r *http.Request) (store.Group, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "group_not_found", "no such group")
		return store.Group{}, false
	}
	g, err := s.store.GetGroup(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "group_not_found", "no such group")
		return store.Group{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.Group{}, false
	}
	return g, true
}

func validGroupName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	return name, n >= 1 && n <= 40
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := []groupJSON{}
	for _, g := range groups {
		j, err := s.groupToJSON(r, g)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroups(w) {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Environment string `json:"environment"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	errs := fieldErrors{}
	name, ok := validGroupName(in.Name)
	if !ok {
		errs.add("name", "must be 1 to 40 characters")
	}
	if in.Environment != store.EnvironmentProduction && in.Environment != store.EnvironmentStaging {
		errs.add("environment", "must be production or staging")
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	g, err := s.groups.CreateGroup(r.Context(), name, in.Environment)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "group_name_taken", "a group with this name exists")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeGroup(w, r, http.StatusCreated, g)
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	g, ok := s.loadGroup(w, r)
	if !ok {
		return
	}
	var in struct {
		Name *string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Name != nil {
		name, valid := validGroupName(*in.Name)
		if !valid {
			writeValidationError(w, fieldErrors{"name": "must be 1 to 40 characters"})
			return
		}
		err := s.store.RenameGroup(r.Context(), g.ID, name)
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "group_name_taken", "a group with this name exists")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		g.Name = name
	}
	s.writeGroup(w, r, http.StatusOK, g)
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroups(w) {
		return
	}
	g, ok := s.loadGroup(w, r)
	if !ok {
		return
	}
	err := s.groups.DeleteGroup(r.Context(), g.ID)
	switch {
	case errors.Is(err, store.ErrGroupNotEmpty):
		writeError(w, http.StatusConflict, "group_not_empty", "remove the apps from the group first")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "group_not_found", "no such group")
	case err != nil:
		s.internalError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleSetAppGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroups(w) {
		return
	}
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in struct {
		GroupID  *int64   `json:"group_id"`
		Services []string `json:"services"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	services := in.Services
	if services == nil {
		services = []string{}
	}
	errs := fieldErrors{}
	for i, service := range services {
		if !compose.ValidGroupService(service) {
			errs.add("services", "each service name must be lowercase letters, digits and inner hyphens")
		}
		if slices.Contains(services[:i], service) {
			errs.add("services", "a service is listed twice")
		}
	}
	if in.GroupID == nil && len(services) > 0 {
		errs.add("services", "must be empty without a group")
	}
	if in.GroupID != nil {
		g, err := s.store.GetGroup(r.Context(), *in.GroupID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			errs.add("group_id", "no such group")
		case err != nil:
			s.internalError(w, r, err)
			return
		case g.Environment != app.Environment:
			errs.add("group_id", "the group's environment differs from the app's")
		}
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	// Who can reach a production app is a safeguard (ARCHITECTURE 10.3).
	if app.Environment == store.EnvironmentProduction && !s.requireRecentAuth(w, r) {
		return
	}

	err := s.groups.SetGroup(r.Context(), app, in.GroupID, services)
	var cannot *deploy.GroupServiceError
	switch {
	case err == nil:
	case errors.As(err, &cannot):
		parts := make([]string, len(cannot.Violations))
		for i, v := range cannot.Violations {
			parts[i] = v.Service + ": " + v.Message
		}
		writeValidationError(w, fieldErrors{"services": strings.Join(parts, "; ")})
		return
	case errors.Is(err, store.ErrGroupEnvironment):
		writeValidationError(w, fieldErrors{"group_id": "the group's environment differs from the app's"})
		return
	case errors.Is(err, store.ErrNotFound):
		writeValidationError(w, fieldErrors{"group_id": "no such group"})
		return
	default:
		if s.writeOpError(w, r, err) {
			return
		}
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
