package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/tricknotes/koroku/internal/domains"
	"github.com/tricknotes/koroku/internal/routing"
	"github.com/tricknotes/koroku/internal/store"
)

// DomainChecker verifies a custom domain against DNS and stores the result.
type DomainChecker interface {
	Check(ctx context.Context, d store.Domain) (store.Domain, error)
}

type recordJSON struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (s *Server) domainToJSON(app store.App, d store.Domain) map[string]any {
	records := []recordJSON{}
	for _, r := range domains.Instructions(d.Hostname, app.Slug, s.cfg.BaseDomain, s.cfg.PublicIP) {
		records = append(records, recordJSON{Type: r.Type, Name: r.Name, Value: r.Value})
	}
	return map[string]any{
		"id":              d.ID,
		"hostname":        d.Hostname,
		"status":          d.Status,
		"is_primary":      d.IsPrimary,
		"force_active":    d.ForceActive,
		"last_checked_at": d.LastCheckedAt,
		"last_error":      d.LastError,
		"records":         records,
	}
}

func (s *Server) handleListDomains(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	list, err := s.store.ListDomains(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, s.domainToJSON(app, d))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default_hostname": routing.DefaultHostname(app.Slug, s.cfg.BaseDomain),
		"domains":          out,
	})
}

func (s *Server) handleAddDomain(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in struct {
		Hostname string `json:"hostname"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	hostname, err := domains.NormalizeHostname(in.Hostname, s.cfg.BaseDomain)
	if err != nil {
		message := "is not a valid hostname"
		if errors.Is(err, domains.ErrReservedHostname) {
			message = "hostnames under the base domain are assigned automatically and cannot be added"
		}
		writeValidationError(w, fieldErrors{"hostname": message})
		return
	}
	if hostname == s.cfg.ConsoleHost {
		writeValidationError(w, fieldErrors{"hostname": "is the hostname of the console"})
		return
	}
	d, err := s.store.AddDomain(r.Context(), app.ID, hostname, s.clock.Now())
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "hostname_taken", "this hostname is already registered")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Verify right away so that a domain whose DNS is already in place
	// becomes active without waiting for the periodic retry.
	d = s.checkDomain(r, d)
	s.recordActivity(r, app.ID, actDomainAdded, map[string]any{"hostname": d.Hostname, "status": d.Status})
	writeJSON(w, http.StatusCreated, s.domainToJSON(app, d))
}

// checkDomain runs a verification and returns the updated domain, or the
// unchanged one when verification itself could not run.
func (s *Server) checkDomain(r *http.Request, d store.Domain) store.Domain {
	if s.domains == nil {
		return d
	}
	checked, err := s.domains.Check(r.Context(), d)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.Error("verifying domain failed", "hostname", d.Hostname, "error", err)
		}
		return d
	}
	return checked
}

// loadDomain resolves {slug} and {id}.
func (s *Server) loadDomain(w http.ResponseWriter, r *http.Request) (store.App, store.Domain, bool) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return store.App{}, store.Domain{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, "domain_not_found", "no such domain")
		return store.App{}, store.Domain{}, false
	}
	d, err := s.store.GetDomain(r.Context(), app.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "domain_not_found", "no such domain")
		return store.App{}, store.Domain{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.App{}, store.Domain{}, false
	}
	return app, d, true
}

func (s *Server) handleVerifyDomain(w http.ResponseWriter, r *http.Request) {
	app, d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	checked := s.checkDomain(r, d)
	s.recordActivity(r, app.ID, actDomainVerified, map[string]any{"hostname": d.Hostname, "status": checked.Status})
	writeJSON(w, http.StatusOK, s.domainToJSON(app, checked))
}

func (s *Server) handleUpdateDomain(w http.ResponseWriter, r *http.Request) {
	app, d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	var in struct {
		IsPrimary   *bool `json:"is_primary"`
		ForceActive *bool `json:"force_active"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	// Force first: a domain forced active in the same request may become primary.
	if in.ForceActive != nil && *in.ForceActive != d.ForceActive {
		if err := s.store.SetDomainForceActive(ctx, app.ID, d.ID, *in.ForceActive); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if in.IsPrimary != nil {
		err := s.store.SetDomainPrimary(ctx, app.ID, d.ID, *in.IsPrimary)
		if errors.Is(err, store.ErrDomainNotActive) {
			writeValidationError(w, fieldErrors{"is_primary": "only an active domain can be the primary domain"})
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	updated, err := s.store.GetDomain(ctx, app.ID, d.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if in.ForceActive != nil && !*in.ForceActive && d.ForceActive {
		// No longer forced: let DNS decide right away.
		updated = s.checkDomain(r, updated)
	}
	if updated.ForceActive != d.ForceActive {
		s.recordActivity(r, app.ID, actDomainForced, map[string]any{"hostname": d.Hostname, "force_active": updated.ForceActive})
	}
	if updated.IsPrimary != d.IsPrimary {
		s.recordActivity(r, app.ID, actDomainPrimary, map[string]any{"hostname": d.Hostname, "is_primary": updated.IsPrimary})
	}
	s.syncRoutes(r, "domain updated")
	writeJSON(w, http.StatusOK, s.domainToJSON(app, updated))
}

func (s *Server) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	app, d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteDomain(r.Context(), app.ID, d.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.recordActivity(r, app.ID, actDomainDeleted, map[string]any{"hostname": d.Hostname})
	s.syncRoutes(r, "domain deleted")
	w.WriteHeader(http.StatusNoContent)
}
