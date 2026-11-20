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

// GroupManager changes groups, memberships and links together with their
// Docker networks (ARCHITECTURE 6.1).
type GroupManager interface {
	CreateGroup(ctx context.Context, name, environment string) (store.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	SetGroup(ctx context.Context, app store.App, groupID *int64, services []string) error
	CreateLink(ctx context.Context, app store.App, name string, target store.App, targetService string, services []string) (store.Link, error)
	DeleteLink(ctx context.Context, app store.App, id int64) error
}

type linkJSON struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	TargetApp     string    `json:"target_app"`
	TargetService string    `json:"target_service"`
	Services      []string  `json:"services"`
	Network       string    `json:"network,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitzero"`
}

type groupMemberJSON struct {
	Slug     string     `json:"slug"`
	Services []string   `json:"services"`
	Links    []linkJSON `json:"links"`
}

type groupJSON struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Environment string            `json:"environment"`
	Members     []groupMemberJSON `json:"members"`
	CreatedAt   time.Time         `json:"created_at"`
}

func linkToJSON(l store.Link, installation string) linkJSON {
	return linkJSON{
		ID: l.ID, Name: l.Name, TargetApp: l.TargetAppSlug, TargetService: l.TargetService,
		Services: l.Services, Network: l.Network(installation), CreatedAt: l.CreatedAt,
	}
}

func (s *Server) groupToJSON(r *http.Request, g store.Group) (groupJSON, error) {
	members, err := s.store.ListGroupMembers(r.Context(), g.ID)
	if err != nil {
		return groupJSON{}, err
	}
	out := groupJSON{
		ID: g.ID, Name: g.Name, Environment: g.Environment,
		Members: []groupMemberJSON{}, CreatedAt: g.CreatedAt,
	}
	for _, app := range members {
		links, err := s.store.ListAppLinks(r.Context(), app.ID)
		if err != nil {
			return groupJSON{}, err
		}
		m := groupMemberJSON{Slug: app.Slug, Services: app.GroupServices, Links: []linkJSON{}}
		for _, l := range links {
			m.Links = append(m.Links, linkJSON{
				ID: l.ID, Name: l.Name, TargetApp: l.TargetAppSlug, TargetService: l.TargetService, Services: l.Services,
			})
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
		if !compose.ValidLinkName(service) {
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
	var cannot *deploy.LinkServiceError
	switch {
	case err == nil:
	case errors.As(err, &cannot):
		writeValidationError(w, fieldErrors{"services": violationText(cannot.Violations)})
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

func violationText(violations []compose.Violation) string {
	parts := make([]string, len(violations))
	for i, v := range violations {
		parts[i] = v.Service + ": " + v.Message
	}
	return strings.Join(parts, "; ")
}

func (s *Server) handleListLinks(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	links, err := s.store.ListAppLinks(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	installation, err := s.store.InstallationID(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := []linkJSON{}
	for _, l := range links {
		out = append(out, linkToJSON(l, installation))
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

func (s *Server) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroups(w) {
		return
	}
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in struct {
		Name          string   `json:"name"`
		TargetApp     string   `json:"target_app"`
		TargetService string   `json:"target_service"`
		Services      []string `json:"services"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	errs := fieldErrors{}
	if !compose.ValidLinkName(in.Name) {
		errs.add("name", "must be lowercase letters, digits and inner hyphens")
	}
	if len(in.Services) == 0 {
		errs.add("services", "choose at least one service that uses the name")
	}
	for i, service := range in.Services {
		if !compose.ValidLinkName(service) {
			errs.add("services", "each service name must be lowercase letters, digits and inner hyphens")
		}
		if slices.Contains(in.Services[:i], service) {
			errs.add("services", "a service is listed twice")
		}
	}
	if app.GroupID == nil {
		errs.add("target_app", "the app is not in a group")
	}
	target, err := s.store.GetApp(r.Context(), in.TargetApp)
	switch {
	case errors.Is(err, store.ErrNotFound):
		errs.add("target_app", "no such app")
	case err != nil:
		s.internalError(w, r, err)
		return
	case target.ID == app.ID || target.GroupID == nil || app.GroupID == nil || *target.GroupID != *app.GroupID:
		errs.add("target_app", "must be another app in the same group")
	case !slices.Contains(target.GroupServices, in.TargetService):
		errs.add("target_service", "must be a service the target app exposes")
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if app.Environment == store.EnvironmentProduction && !s.requireRecentAuth(w, r) {
		return
	}

	link, err := s.groups.CreateLink(r.Context(), app, in.Name, target, in.TargetService, in.Services)
	var cannot *deploy.LinkServiceError
	switch {
	case err == nil:
	case errors.As(err, &cannot):
		errs := fieldErrors{}
		var named, others []compose.Violation
		for _, v := range cannot.Violations {
			if v.Service == in.Name && !slices.Contains(in.Services, v.Service) {
				named = append(named, v)
			} else {
				others = append(others, v)
			}
		}
		if len(named) > 0 {
			errs.add("name", violationText(named))
		}
		if len(others) > 0 {
			errs.add("services", violationText(others))
		}
		writeValidationError(w, errs)
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "link_name_taken", "the app already has a link with this name")
		return
	case errors.Is(err, store.ErrLinkTarget):
		writeValidationError(w, fieldErrors{"target_service": "must be a service another app in the group exposes"})
		return
	default:
		if s.writeOpError(w, r, err) {
			return
		}
	}
	installation, err := s.store.InstallationID(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, linkToJSON(link, installation))
}

func (s *Server) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroups(w) {
		return
	}
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		_, err = s.store.GetLink(r.Context(), app.ID, id)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, strconv.ErrSyntax) || errors.Is(err, strconv.ErrRange) {
			writeError(w, http.StatusNotFound, "link_not_found", "no such link")
			return
		}
		s.internalError(w, r, err)
		return
	}
	if app.Environment == store.EnvironmentProduction && !s.requireRecentAuth(w, r) {
		return
	}
	err = s.groups.DeleteLink(r.Context(), app, id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "link_not_found", "no such link")
	default:
		s.writeOpError(w, r, err)
	}
}
