package api

import (
	"fmt"
	"net/http"

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

// handleGetEnv returns the decrypted variables. The UI masks them by default.
func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListEnvVars(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := envBody{Vars: make([]envVarJSON, 0, len(rows))}
	for _, row := range rows {
		plain, err := s.box.Open(row.ValueEncrypted, secrets.EnvVarContext(app.ID, row.Key))
		if err != nil {
			s.internalError(w, r, fmt.Errorf("env var %s of app %s: %w", row.Key, app.Slug, err))
			return
		}
		out.Vars = append(out.Vars, envVarJSON{Key: row.Key, Value: string(plain)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutEnv replaces every variable of the app. The change takes effect
// with the next deployment.
func (s *Server) handlePutEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
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
		switch {
		case !deploy.ValidEnvKey(v.Key):
			// The key is echoed; the value never is.
			errs.add("vars", fmt.Sprintf("%q is not a valid variable name (letters, digits and underscores, not starting with a digit)", truncateForError(v.Key)))
			continue
		case seen[v.Key]:
			errs.add("vars", fmt.Sprintf("%s is listed more than once", v.Key))
			continue
		case len(v.Value) > maxEnvValueLen:
			errs.add("vars", fmt.Sprintf("the value of %s is longer than %d bytes", v.Key, maxEnvValueLen))
			continue
		}
		seen[v.Key] = true
		if _, err := deploy.RenderEnvFile(map[string]string{v.Key: v.Value}); err != nil {
			errs.add("vars", fmt.Sprintf("the value of %s cannot be written to an env file", v.Key))
			continue
		}
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
	s.handleGetEnv(w, r)
}

func truncateForError(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
