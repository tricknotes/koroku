package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/tricknotes/koroku/internal/envpresets"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

type envPresetJSON struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Generated bool   `json:"generated"`
	Framework string `json:"framework"`
	File      string `json:"file"`
	Match     string `json:"match"`
}

type envPresetsJSON struct {
	Status      string               `json:"status"`
	Reason      string               `json:"reason"`
	ComposePath string               `json:"compose_path"`
	Frameworks  []string             `json:"frameworks"`
	Presets     []envPresetJSON      `json:"presets"`
	Skipped     []envpresets.Skipped `json:"skipped"`
}

// handleEnvPresets suggests the variables the create-app screen prefills
// (ARCHITECTURE 9.8). Secrets are generated here, fresh on every request,
// and stored only when the app is created with them. Failures to read the
// repository are part of a 200 response: the screen then shows nothing.
func (s *Server) handleEnvPresets(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	composePath := strings.TrimSpace(r.URL.Query().Get("path"))
	composeSource := strings.TrimSpace(r.URL.Query().Get("compose_source"))
	errs := fieldErrors{}
	if composeSource != "" && composeSource != store.ComposeSourceRepository && composeSource != store.ComposeSourceInline {
		errs.add("compose_source", "must be repository or inline")
	}
	if _, _, err := github.SplitFullName(owner + "/" + repo); err != nil {
		errs.add("repo", "is not a valid repository name")
	}
	if ref != "" {
		if err := gitrepo.ValidateRef(ref); err != nil {
			errs.add("ref", "is not a valid branch or tag name")
		}
	}
	if composePath != "" {
		clean := path.Clean(composePath)
		if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || len(composePath) > 255 {
			errs.add("path", "must be a path inside the repository")
		}
		composePath = clean
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), envRequirementsTimeout)
	defer cancel()
	result := s.envPresets.Read(ctx, envpresets.Request{
		Owner: owner, Repo: repo, Ref: ref, ComposePath: composePath,
		// The repository's Compose file is not the one the app will run.
		IgnoreCompose: composeSource == store.ComposeSourceInline,
	})
	if result.Reason == envpresets.ReasonGitHubError {
		s.log.Warn("reading env presets failed", "repo", owner+"/"+repo)
	}
	out := envPresetsJSON{
		Status:     result.Status,
		Reason:     result.Reason,
		Frameworks: []string{},
		Presets:    []envPresetJSON{},
		Skipped:    []envpresets.Skipped{},
	}
	if result.Status == envpresets.StatusOK {
		d := result.Detection
		out.ComposePath, out.Frameworks, out.Skipped = d.ComposePath, d.Frameworks, d.Skipped
		for _, p := range d.Presets {
			value := p.Value
			if p.Generator != "" {
				generated, err := envpresets.Generate(p.Generator, rand.Reader)
				if err != nil {
					s.internalError(w, r, err)
					return
				}
				value = generated
			}
			out.Presets = append(out.Presets, envPresetJSON{
				Key: p.Key, Value: value, Generated: p.Generator != "",
				Framework: p.Framework, File: p.File, Match: p.Match,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// sealCreateEnv validates the variables sent with a new app the way
// PUT /api/apps/{slug}/env does. It returns the function that seals them
// for the app's ID once the store knows it.
func (s *Server) sealCreateEnv(vars []envVarJSON, errs fieldErrors) func(appID int64) ([]store.EnvVar, error) {
	if len(vars) > maxEnvVars {
		errs.add("env", fmt.Sprintf("at most %d variables are allowed", maxEnvVars))
		return nil
	}
	seen := map[string]bool{}
	for _, v := range vars {
		if seen[v.Key] {
			errs.add("env", fmt.Sprintf("%s is listed more than once", truncateForError(v.Key)))
			continue
		}
		if msg, ok := checkEnvVar(v.Key, v.Value); !ok {
			errs.add("env", msg)
			continue
		}
		seen[v.Key] = true
	}
	if len(vars) == 0 {
		return nil
	}
	return func(appID int64) ([]store.EnvVar, error) {
		rows := make([]store.EnvVar, 0, len(vars))
		for _, v := range vars {
			sealed, err := s.box.Seal([]byte(v.Value), secrets.EnvVarContext(appID, v.Key))
			if err != nil {
				return nil, fmt.Errorf("sealing %s: %w", v.Key, err)
			}
			rows = append(rows, store.EnvVar{Key: v.Key, ValueEncrypted: sealed})
		}
		return rows, nil
	}
}
