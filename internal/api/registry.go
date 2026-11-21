package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// defaultRegistry is the registry the console suggests.
const defaultRegistry = "ghcr.io"

const (
	maxRegistryUsernameLen = 256
	maxRegistryTokenLen    = 4096
)

// registryHostPattern accepts a registry host with an optional port, the
// way image references name it: no scheme, no path, lowercase.
var registryHostPattern = regexp.MustCompile(`^(localhost|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+)(:[0-9]{1,5})?$`)

// registryCredentialJSON is the response shape. The token is never
// returned; Configured says whether one is saved.
type registryCredentialJSON struct {
	Configured bool       `json:"configured"`
	Registry   string     `json:"registry"`
	Username   string     `json:"username"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// registryCredentialInput is the body of PUT. An empty or missing token
// keeps the saved one, so the registry or username can be corrected
// without pasting the token again.
type registryCredentialInput struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

func (s *Server) handleGetRegistryCredential(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	s.writeRegistryCredential(w, r, app)
}

func (s *Server) writeRegistryCredential(w http.ResponseWriter, r *http.Request, app store.App) {
	cred, err := s.store.GetRegistryCredential(r.Context(), app.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, registryCredentialJSON{Registry: defaultRegistry})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	updated := cred.UpdatedAt
	writeJSON(w, http.StatusOK, registryCredentialJSON{
		Configured: true, Registry: cred.Registry, Username: cred.Username, UpdatedAt: &updated,
	})
}

// handlePutRegistryCredential sets the app's registry credential. It takes
// effect with the next deployment or start.
func (s *Server) handlePutRegistryCredential(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok || !s.requireRecentAuth(w, r) {
		return
	}
	var in registryCredentialInput
	if !decodeJSON(w, r, &in) {
		return
	}
	registry := strings.ToLower(strings.TrimSpace(in.Registry))
	username := strings.TrimSpace(in.Username)
	token := strings.TrimSpace(in.Token)

	errs := fieldErrors{}
	if !registryHostPattern.MatchString(registry) || len(registry) > 255 {
		errs.add("registry", "enter the registry host (and port), such as ghcr.io, without https:// or a path")
	}
	switch {
	case username == "":
		errs.add("username", "enter the username")
	case len(username) > maxRegistryUsernameLen:
		errs.add("username", fmt.Sprintf("the username is longer than %d bytes", maxRegistryUsernameLen))
	case strings.ContainsAny(username, ": \t\r\n"):
		errs.add("username", "the username cannot contain a colon or whitespace")
	}
	// The message never contains the token.
	switch {
	case len(token) > maxRegistryTokenLen:
		errs.add("token", fmt.Sprintf("the token is longer than %d bytes", maxRegistryTokenLen))
	case strings.ContainsAny(token, " \t\r\n"):
		errs.add("token", "the token cannot contain whitespace")
	}

	var sealed []byte
	if token == "" {
		existing, err := s.store.GetRegistryCredential(r.Context(), app.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			errs.add("token", "enter the token")
		case err != nil:
			s.internalError(w, r, err)
			return
		default:
			sealed = existing.TokenEncrypted
		}
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if sealed == nil {
		var err error
		if sealed, err = s.box.Seal([]byte(token), secrets.RegistryTokenContext(app.ID)); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if err := s.store.PutRegistryCredential(r.Context(), store.RegistryCredential{
		AppID: app.ID, Registry: registry, Username: username, TokenEncrypted: sealed, UpdatedAt: s.clock.Now(),
	}); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeRegistryCredential(w, r, app)
}

// handleDeleteRegistryCredential removes the credential and the file koroku
// wrote from it. The app's pulls then use the host's default Docker config.
func (s *Server) handleDeleteRegistryCredential(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok || !s.requireRecentAuth(w, r) {
		return
	}
	err := s.store.DeleteRegistryCredential(r.Context(), app.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "registry_credential_not_found", "the app has no container registry credential")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.ops.ForgetRegistryCredential(app); err != nil {
		// The database no longer has it; the next operation on the app
		// removes the file again.
		s.log.Warn("removing the registry credential file failed", "app", app.Slug, "error", err)
	}
	s.writeRegistryCredential(w, r, app)
}
