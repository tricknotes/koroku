package deploy

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// imageOnlyServices is an app whose web service runs a per-commit CI image.
func imageOnlyServices(tag string) string {
	return `{
	"web": {"image": "ghcr.io/octo/blog:` + tag + `", "ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]},
	"db": {"image": "postgres:17"}
}`
}

func (h *harness) setServices(services string) {
	h.mu.Lock()
	h.repoServices = services
	h.mu.Unlock()
}

// removedImages lists the images passed to `docker image rm`.
func (h *harness) removedImages() []string {
	var images []string
	for _, c := range h.runner.Calls() {
		if c.Name == "docker" && len(c.Args) > 2 && c.Args[0] == "image" && c.Args[1] == "rm" {
			images = append(images, c.Args[2:]...)
		}
	}
	return images
}

func TestPulledImagesAreRemovedWithTheirRelease(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.KeepReleases = 2 })
	h.createApp("blog")
	h.start()

	var ids []int64
	for i, sha := range []string{shaA, shaB, shaC} {
		h.setServices(imageOnlyServices("sha-" + sha[:7]))
		d := h.deploy("blog", sha)
		if d.Status != store.StatusSucceeded {
			t.Fatalf("deployment %d = %+v", i, d)
		}
		ids = append(ids, d.ID)
		meta, err := h.engine.readMeta("blog", d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"ghcr.io/octo/blog:sha-" + sha[:7], "postgres:17"}; !slices.Equal(meta.PulledImages, want) {
			t.Errorf("pulled images of release %d = %v, want %v", d.ID, meta.PulledImages, want)
		}
	}
	// Only the pruned release's tag goes. postgres:17 is still used by the
	// kept releases.
	if got, want := h.removedImages(), []string{"ghcr.io/octo/blog:sha-aaaaaaa"}; !slices.Equal(got, want) {
		t.Errorf("removed images = %v, want %v", got, want)
	}
}

func TestPulledImagesInUseElsewhereAreKept(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.KeepReleases = 1 })
	h.createApp("blog")
	h.createApp("shop")
	// A container (of an app deployed before koroku recorded pulled
	// images, say) was created from this tag.
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if strings.HasPrefix(sub, "ps --all") {
			_, _ = io.WriteString(cmd.Stdout, "redis:7\nkoroku-blog-web:r1\n")
		}
		return nil
	}
	h.start()

	// shop runs the same tag as blog's first release.
	h.setServices(imageOnlyServices("shared"))
	if d := h.deploy("shop", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("shop = %+v", d)
	}
	h.setServices(`{
	"web": {"image": "ghcr.io/octo/blog:shared", "ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]},
	"cache": {"image": "redis:7"}
}`)
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("blog = %+v", d)
	}
	h.setServices(imageOnlyServices("sha-bbbbbbb"))
	if d := h.deploy("blog", shaB); d.Status != store.StatusSucceeded {
		t.Fatalf("blog = %+v", d)
	}
	// blog's first release is gone, but shop's release still pulls the
	// shared tag and a container uses redis:7.
	if got := h.removedImages(); slices.Contains(got, "ghcr.io/octo/blog:shared") || slices.Contains(got, "redis:7") {
		t.Errorf("removed images = %v", got)
	}

	// Deleting shop leaves the images blog still uses.
	shop, _ := h.store.GetApp(context.Background(), "shop")
	h.runner.Reset()
	if err := h.engine.Delete(context.Background(), shop, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, want := h.removedImages(), []string{"ghcr.io/octo/blog:shared"}; !slices.Equal(got, want) {
		t.Errorf("removed images on delete = %v, want %v", got, want)
	}
}

func TestPulledImagesAreKeptWhenContainersCannotBeListed(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.KeepReleases = 1 })
	h.createApp("blog")
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if strings.HasPrefix(sub, "ps --all") {
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.start()
	for _, sha := range []string{shaA, shaB} {
		h.setServices(imageOnlyServices("sha-" + sha[:7]))
		if d := h.deploy("blog", sha); d.Status != store.StatusSucceeded {
			t.Fatalf("deployment = %+v", d)
		}
	}
	if got := h.removedImages(); len(got) != 0 {
		t.Errorf("removed images = %v, want none", got)
	}
}
