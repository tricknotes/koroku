package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tricknotes/koroku/internal/store"
)

// fakeVolumes answers with the volumes "Docker has" for a project.
type fakeVolumes struct {
	mu       sync.Mutex
	names    map[string][]string // Compose project -> volume names
	err      error
	projects []string
}

func (f *fakeVolumes) VolumeNames(_ context.Context, app store.App) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects = append(f.projects, app.ProjectName())
	if f.err != nil {
		return nil, f.err
	}
	return f.names[app.ProjectName()], nil
}

func newVolumesServer(t *testing.T) (http.Handler, *fakeVolumes) {
	t.Helper()
	fake := &fakeVolumes{names: map[string][]string{}}
	h := newTestServer(t, func(o *Options) { o.Volumes = fake }).Handler()
	createApp(t, h, "blog")
	return h, fake
}

func listVolumes(t *testing.T, h http.Handler, slug string) string {
	t.Helper()
	rec := request(t, h, http.MethodGet, "/api/apps/"+slug+"/volumes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list volumes of %s: %d %s", slug, rec.Code, rec.Body)
	}
	return strings.TrimSpace(rec.Body.String())
}

func TestListVolumes(t *testing.T) {
	h, fake := newVolumesServer(t)
	fake.names["paas-blog"] = []string{"pgdata", "uploads"}
	fake.names["paas-shop"] = []string{"data"}

	// Nothing excluded yet: every volume of this app, and only of this app.
	want := `{"volumes":[{"name":"pgdata","excluded":false,"exists":true},{"name":"uploads","excluded":false,"exists":true}]}`
	if got := listVolumes(t, h, "blog"); got != want {
		t.Errorf("volumes = %s\n     want %s", got, want)
	}
	if len(fake.projects) != 1 || fake.projects[0] != "paas-blog" {
		t.Errorf("asked for the volumes of %v", fake.projects)
	}

	// The exclude list shows on the volumes; a name in it that matches no
	// volume is listed too, so that it can be included again.
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"backup_exclude_volumes": ["pgdata", "cache"]}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	want = `{"volumes":[{"name":"cache","excluded":true,"exists":false},` +
		`{"name":"pgdata","excluded":true,"exists":true},` +
		`{"name":"uploads","excluded":false,"exists":true}]}`
	if got := listVolumes(t, h, "blog"); got != want {
		t.Errorf("volumes = %s\n     want %s", got, want)
	}
}

func TestListVolumesOfAnAppThatWasNeverDeployed(t *testing.T) {
	h, _ := newVolumesServer(t)
	// Docker has nothing for the project: an empty list, never null.
	if got := listVolumes(t, h, "blog"); got != `{"volumes":[]}` {
		t.Errorf("volumes = %s", got)
	}
	// A volume excluded ahead of the first deployment is all there is.
	request(t, h, http.MethodPatch, "/api/apps/blog", `{"backup_exclude_volumes": ["pgdata"]}`)
	if got := listVolumes(t, h, "blog"); got != `{"volumes":[{"name":"pgdata","excluded":true,"exists":false}]}` {
		t.Errorf("volumes = %s", got)
	}
}

// Listing volumes takes Docker only: it answers on a host without backups.
func TestListVolumesWithoutBackups(t *testing.T) {
	fake := &fakeVolumes{names: map[string][]string{"paas-blog": {"uploads"}}}
	srv := newTestServer(t, func(o *Options) { o.Volumes = fake })
	if srv.backups != nil {
		t.Fatal("the test needs a server without backups")
	}
	h := srv.Handler()
	createApp(t, h, "blog")
	if rec := request(t, h, http.MethodGet, "/api/apps/blog/backups", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("snapshots without backups: %d", rec.Code)
	}
	if got := listVolumes(t, h, "blog"); got != `{"volumes":[{"name":"uploads","excluded":false,"exists":true}]}` {
		t.Errorf("volumes = %s", got)
	}
}

func TestListVolumesErrors(t *testing.T) {
	h, fake := newVolumesServer(t)

	missing := request(t, h, http.MethodGet, "/api/apps/missing/volumes", "")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "app_not_found") {
		t.Errorf("unknown app: %d %s", missing.Code, missing.Body)
	}
	if len(fake.projects) != 0 {
		t.Errorf("Docker was asked about an unknown app: %v", fake.projects)
	}

	// What Docker said stays in the server log.
	fake.err = errors.New("docker volume ls: exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	failed := request(t, h, http.MethodGet, "/api/apps/blog/volumes", "")
	if failed.Code != http.StatusInternalServerError || !strings.Contains(failed.Body.String(), `"code":"internal"`) {
		t.Errorf("docker failure: %d %s", failed.Code, failed.Body)
	}
	if strings.Contains(failed.Body.String(), "docker.sock") {
		t.Errorf("the response leaks the command error: %s", failed.Body)
	}

	if rec := request(t, h, http.MethodPost, "/api/apps/blog/volumes", `{}`); rec.Code == http.StatusOK {
		t.Errorf("POST must not list volumes: %d", rec.Code)
	}

	// Without a lister the endpoint says so instead of failing.
	bare := newTestServer(t, nil).Handler()
	createApp(t, bare, "blog")
	rec := request(t, bare, http.MethodGet, "/api/apps/blog/volumes", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not_configured") {
		t.Errorf("no lister: %d %s", rec.Code, rec.Body)
	}
}

func TestAppVolumesMergesAndSorts(t *testing.T) {
	got := appVolumes([]string{"uploads", "cache", "uploads"}, []string{"zz", "cache", "zz", "aa"})
	want := []appVolumeJSON{
		{Name: "aa", Excluded: true, Exists: false},
		{Name: "cache", Excluded: true, Exists: true},
		{Name: "uploads", Excluded: false, Exists: true},
		{Name: "zz", Excluded: true, Exists: false},
	}
	if len(got) != len(want) {
		t.Fatalf("volumes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("volumes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if empty := appVolumes(nil, nil); empty == nil || len(empty) != 0 {
		t.Errorf("no volumes must encode as an empty array, got %#v", empty)
	}
}
