package backup

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/runner"
)

// dockerVolumes registers volumes of a project with made-up mountpoints.
func (h *host) dockerVolumes(project string, shorts ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.volumes[project] == nil {
		h.volumes[project] = map[string]string{}
	}
	for _, short := range shorts {
		h.volumes[project][short] = "/var/lib/docker/volumes/" + project + "_" + short + "/_data"
	}
}

func TestVolumeNamesListsTheComposeNamesOfOneProject(t *testing.T) {
	h := newHost(t)
	h.dockerVolumes("koroku-blog", "uploads", "pgdata", "cache")
	h.dockerVolumes("koroku-shop", "data")

	names, err := VolumeNames(context.Background(), h.run, "koroku-blog")
	if err != nil {
		t.Fatalf("VolumeNames: %v", err)
	}
	if want := []string{"cache", "pgdata", "uploads"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	// Docker only, with the arguments as separate words.
	assertLines(t, "commands", h.calls(), []string{
		"docker volume ls --filter label=com.docker.compose.project=koroku-blog --format {{.Name}}",
		"docker volume inspect -- koroku-blog_cache koroku-blog_pgdata koroku-blog_uploads",
	})
}

// The list an API client sees is the list a backup run walks.
func TestVolumeNamesMatchWhatARunBacksUp(t *testing.T) {
	e := newEnv(t)
	e.volume(blog, "uploads", map[string]string{"a.txt": "a"})
	e.volume(blog, "cache", map[string]string{"b.txt": "b"})
	e.apps = []App{blog}

	names, err := VolumeNames(context.Background(), e.host.run, blog.Project)
	if err != nil {
		t.Fatalf("VolumeNames: %v", err)
	}
	res, err := e.m.RunAll(context.Background(), TriggerManual)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	var backedUp []string
	for _, target := range res.Targets {
		if target.Kind == KindVolume {
			backedUp = append(backedUp, target.Target)
		}
	}
	if !slices.Equal(names, backedUp) {
		t.Errorf("VolumeNames = %v, the run backed up %v", names, backedUp)
	}
}

func TestVolumeNamesOfAProjectThatNeverStartedIsEmpty(t *testing.T) {
	h := newHost(t)
	h.dockerVolumes("koroku-shop", "data")

	names, err := VolumeNames(context.Background(), h.run, "koroku-blog")
	if err != nil || len(names) != 0 {
		t.Errorf("VolumeNames = %v, %v; want no volumes and no error", names, err)
	}
	if calls := h.calls(); len(calls) != 1 {
		t.Errorf("nothing to inspect, but the commands were %v", calls)
	}
}

func TestVolumeNamesReadsTheNameFromTheLabels(t *testing.T) {
	h := newHost(t)
	h.set("docker volume ls", func(context.Context, runner.Cmd) (string, error) {
		return "koroku-blog_uploads\nkoroku-blog_legacy\nother_data\n", nil
	})
	h.set("docker volume inspect", func(context.Context, runner.Cmd) (string, error) {
		return `[
			{"Name": "koroku-blog_uploads", "Mountpoint": "/var/lib/docker/volumes/koroku-blog_uploads/_data",
			 "Labels": {"com.docker.compose.project": "koroku-blog", "com.docker.compose.volume": "files"}},
			{"Name": "koroku-blog_legacy", "Mountpoint": "/var/lib/docker/volumes/koroku-blog_legacy/_data",
			 "Labels": {"com.docker.compose.project": "koroku-blog"}},
			{"Name": "other_data", "Mountpoint": "/var/lib/docker/volumes/other_data/_data",
			 "Labels": {"com.docker.compose.project": "other", "com.docker.compose.volume": "data"}}
		]`, nil
	})

	names, err := VolumeNames(context.Background(), h.run, "koroku-blog")
	if err != nil {
		t.Fatalf("VolumeNames: %v", err)
	}
	// The label wins over Docker's name; without it the project prefix is
	// dropped; a volume of another project is never listed.
	if want := []string{"files", "legacy"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestVolumeNamesReportsADockerFailure(t *testing.T) {
	h := newHost(t)
	h.set("docker volume ls", func(_ context.Context, cmd runner.Cmd) (string, error) {
		_, _ = cmd.Stderr.Write([]byte("Cannot connect to the Docker daemon\n"))
		return "", exit("docker", 1)
	})

	_, err := VolumeNames(context.Background(), h.run, "koroku-blog")
	if err == nil {
		t.Fatal("a failing docker command must be an error")
	}
	for _, want := range []string{"list the volumes of koroku-blog", "docker volume ls", "exit status 1", "Cannot connect to the Docker daemon"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestVolumeNamesBoundsTheCommands(t *testing.T) {
	h := newHost(t)
	h.set("docker volume ls", func(ctx context.Context, _ runner.Cmd) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("docker volume ls runs without a deadline")
		}
		return "", nil
	})
	if _, err := VolumeNames(context.Background(), h.run, "koroku-blog"); err != nil {
		t.Fatalf("VolumeNames: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VolumeNames(canceled, h.run, "koroku-blog"); err == nil {
		t.Error("a canceled request must not report an empty list")
	}
}

func TestVolumeNamesRejectsAnInvalidProjectName(t *testing.T) {
	h := newHost(t)
	for _, project := range []string{"", "-p", "koroku blog", "koroku-blog,other"} {
		if _, err := VolumeNames(context.Background(), h.run, project); err == nil {
			t.Errorf("project %q was accepted", project)
		}
	}
	if calls := h.calls(); len(calls) != 0 {
		t.Errorf("commands ran for invalid names: %v", calls)
	}
}
