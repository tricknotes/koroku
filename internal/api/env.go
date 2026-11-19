package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

const (
	maxEnvVars     = 500
	maxEnvValueLen = 64 << 10
)

type envVarJSON struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type envBody struct {
	Vars []envVarJSON `json:"vars"`
}

// envListJSON is the response shape. Value is nil when values are hidden.
type envListJSON struct {
	Vars []envVarOutJSON `json:"vars"`
	// Removed lists keys that were deleted but are still in the running
	// release.
	Removed []string `json:"removed"`
	// Compared is false when there is no running release to compare with,
	// or it predates the fingerprints (ARCHITECTURE 9.2).
	Compared bool `json:"compared"`
	// ValuesHidden is true when the session is too old to see secrets
	// (ARCHITECTURE 10.3); the keys are still listed.
	ValuesHidden bool `json:"values_hidden"`
}

type envVarOutJSON struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
	// Pending is "added" or "changed" when the saved variable differs from
	// the running release, nil otherwise.
	Pending *string `json:"pending"`
}

// envVarPutJSON is the body of PUT /api/apps/{slug}/env/{key}. Key renames
// the variable; Value nil keeps the value of the one being renamed.
type envVarPutJSON struct {
	Key   *string `json:"key"`
	Value *string `json:"value"`
}

var (
	pendingAdded   = "added"
	pendingChanged = "changed"
)

// handleGetEnv returns the decrypted variables. The UI masks them by default.
// Without a recent login only the keys are returned.
func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	s.writeEnv(w, r, app)
}

// writeEnv responds with the app's variables and how they differ from the
// running release.
func (s *Server) writeEnv(w http.ResponseWriter, r *http.Request, app store.App) {
	rows, err := s.store.ListEnvVars(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var deployed map[string]string
	if app.CurrentDeploymentID != nil {
		deployed, _, err = s.store.DeploymentEnvFingerprints(r.Context(), *app.CurrentDeploymentID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	out := envListJSON{
		Vars:         make([]envVarOutJSON, 0, len(rows)),
		Removed:      []string{},
		Compared:     deployed != nil,
		ValuesHidden: !s.recentlyAuthenticated(r),
	}
	saved := make(map[string]bool, len(rows))
	for _, row := range rows {
		saved[row.Key] = true
		ad := secrets.EnvVarContext(app.ID, row.Key)
		plain, err := s.box.Open(row.ValueEncrypted, ad)
		if err != nil {
			s.internalError(w, r, fmt.Errorf("env var %s of app %s: %w", row.Key, app.Slug, err))
			return
		}
		v := envVarOutJSON{Key: row.Key}
		if !out.ValuesHidden {
			value := string(plain)
			v.Value = &value
		}
		if deployed != nil {
			// The fingerprint reveals nothing about the value, so the
			// difference is shown even while values are hidden.
			switch fp, ok := deployed[row.Key]; {
			case !ok:
				v.Pending = &pendingAdded
			case fp != s.box.Fingerprint(ad, plain):
				v.Pending = &pendingChanged
			}
		}
		out.Vars = append(out.Vars, v)
	}
	for key := range deployed {
		if !saved[key] {
			out.Removed = append(out.Removed, key)
		}
	}
	sort.Strings(out.Removed)
	writeJSON(w, http.StatusOK, out)
}

// checkEnvVar applies the rules every saved variable must meet. The message
// may name the key but never contains the value.
func checkEnvVar(key, value string) (string, bool) {
	switch {
	case !deploy.ValidEnvKey(key):
		return fmt.Sprintf("%q is not a valid variable name (letters, digits and underscores, not starting with a digit)", truncateForError(key)), false
	case len(value) > maxEnvValueLen:
		return fmt.Sprintf("the value of %s is longer than %d bytes", key, maxEnvValueLen), false
	}
	if _, err := deploy.RenderEnvFile(map[string]string{key: value}); err != nil {
		return fmt.Sprintf("the value of %s cannot be written to an env file", key), false
	}
	return "", true
}

// handlePutEnv replaces every variable of the app. The change takes effect
// with the next deployment.
func (s *Server) handlePutEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok || !s.requireRecentAuth(w, r) {
		return
	}
	var in envBody
	if !decodeJSON(w, r, &in) {
		return
	}
	errs := fieldErrors{}
	if len(in.Vars) > maxEnvVars {
		errs.add("vars", fmt.Sprintf("at most %d variables are allowed", maxEnvVars))
	}
	seen := map[string]bool{}
	rows := make([]store.EnvVar, 0, len(in.Vars))
	for _, v := range in.Vars {
		if seen[v.Key] {
			errs.add("vars", fmt.Sprintf("%s is listed more than once", v.Key))
			continue
		}
		if msg, ok := checkEnvVar(v.Key, v.Value); !ok {
			errs.add("vars", msg)
			continue
		}
		seen[v.Key] = true
		sealed, err := s.box.Seal([]byte(v.Value), secrets.EnvVarContext(app.ID, v.Key))
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		rows = append(rows, store.EnvVar{Key: v.Key, ValueEncrypted: sealed})
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if err := s.store.ReplaceEnvVars(r.Context(), app.ID, rows); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeEnv(w, r, app)
}

// handlePutEnvVar adds, changes or renames one variable. The change takes
// effect with the next deployment.
func (s *Server) handlePutEnvVar(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok || !s.requireRecentAuth(w, r) {
		return
	}
	from := r.PathValue("key")
	var in envVarPutJSON
	if !decodeJSON(w, r, &in) {
		return
	}
	key := from
	if in.Key != nil {
		key = *in.Key
	}
	rename := key != from

	rows, err := s.store.ListEnvVars(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var current *store.EnvVar
	exists := false
	for i, row := range rows {
		if row.Key == from {
			current = &rows[i]
		}
		if row.Key == key {
			exists = true
		}
	}
	if rename && current == nil {
		writeError(w, http.StatusNotFound, "env_var_not_found", "no such variable")
		return
	}

	var value string
	switch {
	case in.Value != nil:
		value = *in.Value
	case rename:
		plain, err := s.box.Open(current.ValueEncrypted, secrets.EnvVarContext(app.ID, from))
		if err != nil {
			s.internalError(w, r, fmt.Errorf("env var %s of app %s: %w", from, app.Slug, err))
			return
		}
		value = string(plain)
	default:
		writeValidationError(w, fieldErrors{"value": "is required"})
		return
	}
	if msg, ok := checkEnvVar(key, value); !ok {
		field := "value"
		if !deploy.ValidEnvKey(key) {
			field = "key"
		}
		writeValidationError(w, fieldErrors{field: msg})
		return
	}
	if rename && exists {
		writeError(w, http.StatusConflict, "env_var_exists", fmt.Sprintf("%s already exists", key))
		return
	}
	if !exists && !rename && len(rows) >= maxEnvVars {
		writeValidationError(w, fieldErrors{"key": fmt.Sprintf("at most %d variables are allowed", maxEnvVars)})
		return
	}

	// The ciphertext is bound to the key, so a rename seals the value again.
	sealed, err := s.box.Seal([]byte(value), secrets.EnvVarContext(app.ID, key))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v := store.EnvVar{Key: key, ValueEncrypted: sealed}
	if rename {
		err = s.store.RenameEnvVar(r.Context(), app.ID, from, v)
	} else {
		err = s.store.PutEnvVar(r.Context(), app.ID, v)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "env_var_not_found", "no such variable")
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "env_var_exists", fmt.Sprintf("%s already exists", key))
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.writeEnv(w, r, app)
}

// handleDeleteEnvVar removes one variable. The change takes effect with the
// next deployment.
func (s *Server) handleDeleteEnvVar(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok || !s.requireRecentAuth(w, r) {
		return
	}
	err := s.store.DeleteEnvVar(r.Context(), app.ID, r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "env_var_not_found", "no such variable")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeEnv(w, r, app)
}

func truncateForError(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
