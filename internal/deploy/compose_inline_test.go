package deploy

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

const inlineContent = "services:\n  web:\n    build: .\n    ports: [\"3000\"]\n  db:\n    image: postgres:17\n"

// writeInline makes the app deploy a Compose file written in koroku.
func (h *harness) writeInline(slug, content string) store.App {
	h.t.Helper()
	app, err := h.store.GetApp(context.Background(), slug)
	if err != nil {
		h.t.Fatal(err)
	}
	app.ComposeSource, app.ComposeContent = store.ComposeSourceInline, content
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		h.t.Fatal(err)
	}
	return app
}

// composeFiles returns the -f arguments of a compose call.
func composeFiles(args []string) []string {
	var files []string
	for i, arg := range args {
		if arg == "-f" && i+1 < len(args) {
			files = append(files, args[i+1])
		}
	}
	return files
}

func TestDeployUsesTheComposeWrittenInKoroku(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.writeInline("blog", inlineContent)
	// The repository may bring a file of the same name, as a link to
	// anywhere.
	victim := filepath.Join(h.dir, "victim.conf")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.onFetch = func(dir string) error {
		return os.Symlink(victim, filepath.Join(dir, compose.InlineFileName))
	}
	h.start()

	first := h.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", first, h.log("blog", first.ID))
	}
	if got, _ := os.ReadFile(victim); string(got) != "do not touch" {
		t.Errorf("the symlink target was overwritten: %q", got)
	}
	dir := h.engine.ReleaseDir("blog", first.ID)
	path := filepath.Join(dir, compose.InlineFileName)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the Compose file must be a regular file of the release: %v %v", info, err)
	}
	if got, _ := os.ReadFile(path); string(got) != inlineContent {
		t.Errorf("Compose file = %q", got)
	}
	// Every call reads it in place of the repository's compose.yaml.
	for _, sub := range []string{"config", "build", "up"} {
		files := composeFiles(h.lastCall(sub).Args)
		if len(files) == 0 || files[0] != path {
			t.Errorf("%s reads %v", sub, files)
		}
	}
	if meta, err := h.engine.readMeta("blog", first.ID); err != nil || meta.ComposeFile != compose.InlineFileName {
		t.Errorf("release metadata = %+v, %v", meta, err)
	}
	if log := h.log("blog", first.ID); !strings.Contains(log, "Compose file: koroku.compose.yaml (written in koroku; the repository's own Compose files are not read)") {
		t.Errorf("log = %s", log)
	}

	// A saved change reaches the next release only.
	changed := inlineContent + "  cache:\n    image: redis:8\n"
	h.writeInline("blog", changed)
	if got, _ := os.ReadFile(path); string(got) != inlineContent {
		t.Errorf("the kept release changed: %q", got)
	}
	second := h.deploy("blog", shaB)
	if second.Status != store.StatusSucceeded {
		t.Fatalf("second deployment = %+v\n%s", second, h.log("blog", second.ID))
	}
	if got, _ := os.ReadFile(filepath.Join(h.engine.ReleaseDir("blog", second.ID), compose.InlineFileName)); string(got) != changed {
		t.Errorf("second release = %q", got)
	}
	if got, _ := os.ReadFile(path); string(got) != inlineContent {
		t.Errorf("the first release changed: %q", got)
	}
}

func TestDeployWithoutTheSavedComposeFails(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.writeInline("blog", " \n")
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "none is saved") {
		t.Fatalf("deployment = %+v", d)
	}
	// The repository's own file is not a fallback.
	if slices.Contains(h.dockerCalls(), "config") {
		t.Errorf("calls = %v", h.dockerCalls())
	}
}

// The preview reads the source's checkout with the target's Compose file,
// without writing into the source's release.
func TestPreviewPromotionImagesWithAnInlineTarget(t *testing.T) {
	p := newPromotionHarness(t, "https://assets.example.com", "https://assets.example.com")
	source := p.stagingRelease()
	staging, _ := p.store.GetApp(context.Background(), "shop-staging")
	prod := p.writeInline("shop", inlineContent)
	sourceDir := p.engine.ReleaseDir("shop-staging", source.ID)
	before, _ := os.ReadDir(sourceDir)

	var read string
	p.mu.Lock()
	p.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "config" && cmd.Args[2] == "koroku-shop" {
			data, err := os.ReadFile(composeFiles(cmd.Args)[0])
			read = string(data)
			return err
		}
		return nil
	}
	p.mu.Unlock()

	decisions, err := p.engine.PreviewPromotionImages(context.Background(), prod, staging, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || !decisions[0].Reuse {
		t.Errorf("decisions = %+v", decisions)
	}
	if read != inlineContent {
		t.Errorf("the preview read %q", read)
	}
	call := p.lastCall("config")
	files := composeFiles(call.Args)
	i := slices.Index(call.Args, "--project-directory")
	if i < 0 || call.Args[i+1] != sourceDir || len(files) != 1 || filepath.Dir(files[0]) != p.engine.appDir("shop") {
		t.Errorf("config call = %v", call.Args)
	}
	if after, _ := os.ReadDir(sourceDir); len(after) != len(before) {
		t.Errorf("the source's release changed: %v", after)
	}
	entries, _ := os.ReadDir(p.engine.appDir("shop"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".promotion-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
