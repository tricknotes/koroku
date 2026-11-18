package api

import (
	"context"
	"net/http"
	"slices"
	"sort"

	"github.com/tricknotes/koroku/internal/store"
)

// AppVolumes lists the named volumes of an app. It asks Docker only, so it
// is available whether or not backups are configured.
type AppVolumes interface {
	// VolumeNames returns the named volumes of the app's Compose project
	// that exist in Docker, by the name written in the Compose file (the
	// name the exclude list and the snapshot targets use). An app that was
	// never deployed has none.
	VolumeNames(ctx context.Context, app store.App) ([]string, error)
}

// appVolumeJSON is one named volume of an app as the backup settings see it.
// There is no size on purpose: measuring a volume walks its directory.
type appVolumeJSON struct {
	Name string `json:"name"`
	// Excluded is true when the name is in backup_exclude_volumes.
	Excluded bool `json:"excluded"`
	// Exists is false for a name that is excluded but matches no volume in
	// Docker: excluded ahead of the first deployment, or left over from a
	// volume that is gone. It is listed so that it can be included again.
	Exists bool `json:"exists"`
}

// handleListVolumes lists the app's named volumes together with the names
// in its exclude list.
func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if s.volumes == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "listing volumes is not available")
		return
	}
	names, err := s.volumes.VolumeNames(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": appVolumes(names, app.BackupExcludeVolumes)})
}

// appVolumes merges the volumes that exist with the exclude list, sorted by
// name. A name appears once however often the inputs repeat it.
func appVolumes(existing, excluded []string) []appVolumeJSON {
	out := make([]appVolumeJSON, 0, len(existing)+len(excluded))
	seen := make(map[string]bool, len(existing)+len(excluded))
	for _, name := range existing {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, appVolumeJSON{Name: name, Excluded: slices.Contains(excluded, name), Exists: true})
	}
	for _, name := range excluded {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, appVolumeJSON{Name: name, Excluded: true, Exists: false})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
