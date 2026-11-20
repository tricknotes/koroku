package deploy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

func TestMemorySampler(t *testing.T) {
	h := newHarness(t, nil)
	blog := h.createApp("blog")
	shop := h.createApp("shop")
	start := h.clock.Now()

	stats, inspect := statsOutput, inspectOutput
	h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
		joined := strings.Join(cmd.Args, " ")
		switch {
		case joined == "stats --no-stream --no-trunc --format json":
			return stats, nil
		case strings.HasPrefix(joined, "container inspect --format "+containerInspectFormat+" "):
			return inspect, nil
		}
		return "", errors.New("unexpected command: " + joined)
	}
	sampler := h.engine.NewMemorySampler()
	ctx := context.Background()
	if err := sampler.Sample(ctx); err != nil {
		t.Fatal(err)
	}

	// A minute later: the db restarted once (RestartCount 2 → 3), and a
	// second web container (a replica) uses more than the first.
	h.clock.Advance(time.Minute)
	stats = statsOutput + `{"CPUPerc":"1.00%","ID":"fff","MemUsage":"120MiB / 7.413GiB","Name":"koroku-blog-web-2"}` + "\n"
	inspect = strings.Replace(inspectOutput, `"db"	536870912	2`, `"db"	536870912	3`, 1) +
		`"fff"	"/koroku-blog-web-2"	"koroku-blog"	"web"	268435456	0` + "\n"
	if err := sampler.Sample(ctx); err != nil {
		t.Fatal(err)
	}

	limit := int64p(512 << 20)
	web1, db := parseDockerSize("93.84MiB"), parseDockerSize("207.1MiB")
	got, err := h.store.ListMemoryUsage(ctx, blog.ID, start.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []store.MemoryBucket{
		{Service: "db", Start: start, MaxBytes: db, SumBytes: 2 * db, Samples: 2, LimitBytes: limit, Restarts: 1},
		{Service: "web", Start: start, MaxBytes: 120 << 20, SumBytes: web1 + 120<<20, Samples: 2, LimitBytes: int64p(256 << 20)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blog buckets =\n%+v\nwant\n%+v", got, want)
	}
	if got, _ := h.store.ListMemoryUsage(ctx, shop.ID, start.Add(-time.Hour)); len(got) != 1 || got[0].MaxBytes != 300<<20 {
		t.Errorf("shop buckets = %+v", got)
	}

	// Fifteen days on, the old buckets are removed with the next sampling.
	h.clock.Advance(15 * 24 * time.Hour)
	stats = ""
	if err := sampler.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.ListMemoryUsage(ctx, blog.ID, time.Time{}); len(got) != 0 {
		t.Errorf("buckets past the retention remain: %+v", got)
	}
}

func TestMemorySamplerFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.runner.Handler = func(context.Context, runner.Cmd) (string, error) {
		return "", errors.New("Cannot connect to the Docker daemon")
	}
	if err := h.engine.NewMemorySampler().Sample(context.Background()); err == nil {
		t.Error("a docker failure must be reported")
	}
}
