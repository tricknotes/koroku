package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// Output shapes as Docker 29 prints them (IDs shortened for readability).
const (
	statsOutput = `{"BlockIO":"3.5GB / 225MB","CPUPerc":"6.09%","Container":"aaa","ID":"aaa","MemPerc":"1.24%","MemUsage":"93.84MiB / 7.413GiB","Name":"koroku-blog-web-1","NetIO":"12.6MB / 11MB","PIDs":"9"}
{"BlockIO":"206MB / 24.7MB","CPUPerc":"0.08%","Container":"bbb","ID":"bbb","MemPerc":"2.73%","MemUsage":"207.1MiB / 7.413GiB","Name":"koroku-blog-db-1","NetIO":"0B / 0B","PIDs":"39"}
{"BlockIO":"57.8MB / 119kB","CPUPerc":"1.00%","Container":"ccc","ID":"ccc","MemPerc":"0.25%","MemUsage":"19.23MiB / 7.413GiB","Name":"koroku-dev-caddy-1","NetIO":"0B / 0B","PIDs":"13"}
{"BlockIO":"0B / 0B","CPUPerc":"12.00%","Container":"ddd","ID":"ddd","MemPerc":"10.00%","MemUsage":"300MiB / 512MiB","Name":"koroku-shop-web-1","NetIO":"0B / 0B","PIDs":"20"}
{"BlockIO":"0B / 0B","CPUPerc":"--","Container":"eee","ID":"eee","MemPerc":"--","MemUsage":"-- / --","Name":"registry","NetIO":"--","PIDs":"--"}
`
	inspectOutput = `"aaa"	"/koroku-blog-web-1"	"koroku-blog"	"web"	536870912
"bbb"	"/koroku-blog-db-1"	"koroku-blog"	"db"	536870912
"ccc"	"/koroku-dev-caddy-1"	"koroku-dev"	"caddy"	0
"ddd"	"/koroku-shop-web-1"	"koroku-shop"	"web"	536870912
"eee"	"/registry"	""	""	0
`
)

func int64p(v int64) *int64 { return &v }
func strp(v string) *string { return &v }

func TestContainerResources(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.createApp("shop")
	h.createApp("idle") // nothing running: no group
	h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
		joined := strings.Join(cmd.Args, " ")
		switch {
		case joined == "stats --no-stream --no-trunc --format json":
			return statsOutput, nil
		case joined == "info --format {{.NCPU}} {{.MemTotal}}":
			return "4 7959953408\n", nil
		case strings.HasPrefix(joined, "container inspect --format "+containerInspectFormat+" "):
			if ids := strings.Fields(strings.TrimPrefix(joined, "container inspect --format "+containerInspectFormat)); !reflect.DeepEqual(ids, []string{"aaa", "bbb", "ccc", "ddd", "eee"}) {
				return "", errors.New("unexpected ids " + strings.Join(ids, ","))
			}
			return inspectOutput, nil
		}
		return "", errors.New("unexpected command: " + joined)
	}
	got, err := h.engine.ContainerResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := ContainerResources{
		CPUCount:         4,
		MemoryTotalBytes: 7959953408,
		Groups: []ResourceGroup{
			{
				App: strp("blog"), MemoryBytes: parseDockerSize("93.84MiB") + parseDockerSize("207.1MiB"), MemoryLimitBytes: int64p(1 << 30), CPUPercent: 1.54,
				Containers: []ContainerUsage{
					{Name: "koroku-blog-db-1", Project: "koroku-blog", Service: "db", MemoryBytes: parseDockerSize("207.1MiB"), MemoryLimitBytes: int64p(512 << 20), CPUPercent: 0.02},
					{Name: "koroku-blog-web-1", Project: "koroku-blog", Service: "web", MemoryBytes: parseDockerSize("93.84MiB"), MemoryLimitBytes: int64p(512 << 20), CPUPercent: 1.52},
				},
			},
			{
				App: strp("shop"), MemoryBytes: 300 << 20, MemoryLimitBytes: int64p(512 << 20), CPUPercent: 3,
				Containers: []ContainerUsage{
					{Name: "koroku-shop-web-1", Project: "koroku-shop", Service: "web", MemoryBytes: 300 << 20, MemoryLimitBytes: int64p(512 << 20), CPUPercent: 3},
				},
			},
			{
				// A koroku- project that is no app, and a container outside
				// Compose, share the last group. No limit: no total limit.
				MemoryBytes: parseDockerSize("19.23MiB"), CPUPercent: 0.25,
				Containers: []ContainerUsage{
					{Name: "koroku-dev-caddy-1", Project: "koroku-dev", Service: "caddy", MemoryBytes: parseDockerSize("19.23MiB"), CPUPercent: 0.25},
					{Name: "registry"},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ContainerResources =\n%s\nwant\n%s", dump(got), dump(want))
	}
}

func dump(r ContainerResources) string {
	out, _ := json.MarshalIndent(r, "", "  ")
	return string(out)
}

func TestContainerResourcesWithNothingRunning(t *testing.T) {
	h := newHarness(t, nil)
	h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
		switch cmd.Args[0] {
		case "stats":
			return "", nil
		case "info":
			return "2 4294967296\n", nil
		}
		return "", errors.New("unexpected command")
	}
	got, err := h.engine.ContainerResources(context.Background())
	if err != nil || got.CPUCount != 2 || got.Groups == nil || len(got.Groups) != 0 {
		t.Errorf("ContainerResources = %+v, %v", got, err)
	}
	for _, call := range h.runner.Calls() {
		if call.Args[0] == "container" {
			t.Errorf("inspected with no containers: %v", call.Args)
		}
	}
}

func TestContainerResourcesRaces(t *testing.T) {
	defer func(d time.Duration) { dockerRetryDelay = d }(dockerRetryDelay)
	dockerRetryDelay = time.Millisecond
	h := newHarness(t, nil)
	h.createApp("blog")
	var mu sync.Mutex
	statsCalls := 0
	h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
		switch cmd.Args[0] {
		case "stats":
			mu.Lock()
			defer mu.Unlock()
			statsCalls++
			if statsCalls == 1 {
				return "", errors.New("Error response from daemon: No such container: 0123")
			}
			return statsOutput, nil
		case "info":
			return "4 7959953408\n", nil
		case "container":
			// Two containers went away after docker stats listed them.
			lines := strings.Split(inspectOutput, "\n")
			return strings.Join(lines[:3], "\n") + "\n", errors.New("exit status 1: Error response from daemon: No such container: ddd; Error response from daemon: No such container: eee")
		}
		return "", errors.New("unexpected command")
	}
	got, err := h.engine.ContainerResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if statsCalls != 2 {
		t.Errorf("docker stats ran %d times, want a retry", statsCalls)
	}
	var names []string
	for _, g := range got.Groups {
		for _, c := range g.Containers {
			names = append(names, c.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"koroku-blog-db-1", "koroku-blog-web-1", "koroku-dev-caddy-1"}) {
		t.Errorf("containers = %v", names)
	}

	// Any other failure of inspect is an error.
	h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
		switch cmd.Args[0] {
		case "stats":
			return statsOutput, nil
		case "info":
			return "4 7959953408\n", nil
		}
		return "", errors.New("Cannot connect to the Docker daemon")
	}
	if _, err := h.engine.ContainerResources(context.Background()); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Errorf("err = %v", err)
	}
}

func TestParseStatsRejectsGarbage(t *testing.T) {
	if _, err := parseStats([]byte("not json")); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := parseContainerInspect([]byte(`"aaa"	"/x"`)); err == nil {
		t.Error("short inspect line accepted")
	}
}
