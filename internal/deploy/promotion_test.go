package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// promotionHarness has a staging and a production app whose web service
// passes ASSET_HOST as a build arg, like a Rails app baking asset_host
// into its assets would.
type promotionHarness struct {
	*harness
	staging, prod store.App
}

func newPromotionHarness(t *testing.T, stagingHost, prodHost string) *promotionHarness {
	t.Helper()
	h := newHarness(t, nil)
	h.servicesFor = func(cmd runner.Cmd) string {
		host := envValue(t, cmd.Args, "ASSET_HOST")
		webPorts := `[{"target": 3000, "published": "3000", "protocol": "tcp"}]`
		dbPorts := `[{"target": 5432, "published": "5432", "protocol": "tcp"}]`
		if _, merged := composeSubcommand(cmd.Args); merged {
			// Simulate the override: the web service on its loopback port.
			app, err := h.store.GetApp(context.Background(), strings.TrimPrefix(cmd.Args[2], "koroku-"))
			if err != nil {
				t.Errorf("config for unknown project %v", cmd.Args)
			}
			webPorts = fmt.Sprintf(`[{"host_ip": "127.0.0.1", "target": 3000, "published": "%d", "protocol": "tcp"}]`, app.HostPort)
			dbPorts = `null`
		}
		return fmt.Sprintf(`{
	"web": {"build": {"context": %q, "dockerfile": "Dockerfile", "args": {"ASSET_HOST": %q}}, "ports": %s},
	"db": {"image": "postgres:17", "ports": %s}
}`, cmd.Dir, host, webPorts, dbPorts)
	}
	p := &promotionHarness{harness: h, staging: h.createApp("shop-staging"), prod: h.createApp("shop")}
	h.setEnv(p.staging, map[string]string{"ASSET_HOST": stagingHost, "SECRET": "staging-secret"})
	h.setEnv(p.prod, map[string]string{"ASSET_HOST": prodHost, "SECRET": "prod-secret"})
	h.start()
	return p
}

// envValue reads one variable from the --env-file of a compose call, as
// Compose interpolation would.
func envValue(t *testing.T, args []string, key string) string {
	t.Helper()
	i := slices.Index(args, "--env-file")
	if i < 0 {
		return ""
	}
	data, err := os.ReadFile(args[i+1])
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(v, "'")
		}
	}
	return ""
}

func (p *promotionHarness) promote(source store.Deployment) store.Deployment {
	p.t.Helper()
	prod, err := p.store.GetApp(context.Background(), "shop")
	if err != nil {
		p.t.Fatal(err)
	}
	d, err := p.engine.EnqueuePromotion(context.Background(), prod, source)
	if err != nil {
		p.t.Fatalf("EnqueuePromotion: %v", err)
	}
	if d.PromotedFromID == nil || *d.PromotedFromID != source.ID || d.CommitSHA != source.CommitSHA {
		p.t.Fatalf("queued promotion = %+v", d)
	}
	return p.wait(d.ID)
}

// stagingRelease deploys shaA to staging and returns the deployment.
func (p *promotionHarness) stagingRelease() store.Deployment {
	p.t.Helper()
	d := p.deploy("shop-staging", shaA)
	if d.Status != store.StatusSucceeded {
		p.t.Fatalf("staging deployment = %+v\n%s", d, p.log("shop-staging", d.ID))
	}
	p.runner.Reset()
	return d
}

func TestPromotionReusesImages(t *testing.T) {
	p := newPromotionHarness(t, "https://assets.example.com", "https://assets.example.com")
	source := p.stagingRelease()

	d := p.promote(source)
	log := p.log("shop", d.ID)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("promotion = %+v\n%s", d, log)
	}
	calls := p.dockerCalls()
	if slices.Contains(calls, "build") {
		t.Errorf("a promotion with the same build definition built: %v", calls)
	}
	srcImage := fmt.Sprintf("koroku-shop-staging-web:r%d", source.ID)
	wantTag := fmt.Sprintf("tag %s koroku-shop-web:r%d", srcImage, d.ID)
	if !slices.Contains(calls, wantTag) {
		t.Errorf("docker calls = %v, want %q", calls, wantTag)
	}
	if !slices.Contains(calls, "image inspect --format {{.Id}} "+srcImage) {
		t.Errorf("the staging image was not checked: %v", calls)
	}
	// The image-only service is still pulled.
	if !slices.Contains(calls, "pull") {
		t.Errorf("docker calls = %v, want a pull", calls)
	}
	for _, want := range []string{
		fmt.Sprintf("promoting deployment #%d", source.ID),
		fmt.Sprintf(" reuse Reusing images of shop-staging deployment #%d", source.ID),
		"web: reusing " + srcImage,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, " build Building images") {
		t.Errorf("log has a build step:\n%s", log)
	}
	// The tagged image belongs to the new release and goes with it.
	meta, err := p.engine.readMeta("shop", d.ID)
	if err != nil || !slices.Contains(meta.Images, fmt.Sprintf("koroku-shop-web:r%d", d.ID)) {
		t.Errorf("release images = %v, %v", meta.Images, err)
	}
}

func TestPromotionRebuildsWhenBuildArgsDiffer(t *testing.T) {
	p := newPromotionHarness(t, "https://staging-assets.example.com", "https://assets.example.com")
	source := p.stagingRelease()

	d := p.promote(source)
	log := p.log("shop", d.ID)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("promotion = %+v\n%s", d, log)
	}
	calls := p.dockerCalls()
	if !slices.Contains(calls, "build") {
		t.Errorf("docker calls = %v, want a build", calls)
	}
	if slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "tag ") }) {
		t.Errorf("an image was tagged although the build args differ: %v", calls)
	}
	// Only the service that could not be reused is built.
	if build := p.lastCall("build"); build.Args[len(build.Args)-1] != "web" {
		t.Errorf("build args = %v", build.Args)
	}
	if !strings.Contains(log, "web: building (build args differ: ASSET_HOST)") {
		t.Errorf("log lacks the reason:\n%s", log)
	}
	for _, secret := range []string{"staging-assets.example.com", "https://assets.example.com", "prod-secret"} {
		if strings.Contains(log, secret) {
			t.Errorf("log leaks %q:\n%s", secret, log)
		}
	}
}

func TestPromotionRebuildsWhenStagingImageIsGone(t *testing.T) {
	p := newPromotionHarness(t, "same", "same")
	source := p.stagingRelease()
	p.mu.Lock()
	p.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if strings.HasPrefix(sub, "image inspect") {
			return errors.New("No such image")
		}
		return nil
	}
	p.mu.Unlock()

	d := p.promote(source)
	log := p.log("shop", d.ID)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("promotion = %+v\n%s", d, log)
	}
	if !slices.Contains(p.dockerCalls(), "build") {
		t.Errorf("docker calls = %v, want a build", p.dockerCalls())
	}
	if !strings.Contains(log, "web: building (the image of the promoted release is gone)") {
		t.Errorf("log lacks the reason:\n%s", log)
	}
}

func TestPromotionRebuildsWhenTaggingFails(t *testing.T) {
	p := newPromotionHarness(t, "same", "same")
	source := p.stagingRelease()
	p.mu.Lock()
	p.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if strings.HasPrefix(sub, "tag ") {
			return errors.New("tag failed")
		}
		return nil
	}
	p.mu.Unlock()

	d := p.promote(source)
	if d.Status != store.StatusSucceeded || !slices.Contains(p.dockerCalls(), "build") {
		t.Fatalf("promotion = %+v, calls %v\n%s", d, p.dockerCalls(), p.log("shop", d.ID))
	}
}

func TestPromotionRebuildsWhenStagingReleaseIsGone(t *testing.T) {
	p := newPromotionHarness(t, "same", "same")
	source := p.stagingRelease()
	if err := os.RemoveAll(p.engine.stateDir("shop-staging", source.ID)); err != nil {
		t.Fatal(err)
	}

	d := p.promote(source)
	log := p.log("shop", d.ID)
	if d.Status != store.StatusSucceeded || !slices.Contains(p.dockerCalls(), "build") {
		t.Fatalf("promotion = %+v, calls %v\n%s", d, p.dockerCalls(), log)
	}
	if !strings.Contains(log, "web: building (the promoted release is no longer on disk)") {
		t.Errorf("log lacks the reason:\n%s", log)
	}
}

func TestPreviewPromotionImages(t *testing.T) {
	p := newPromotionHarness(t, "https://staging.example.com", "https://cdn.example.com")
	source := p.stagingRelease()
	staging, _ := p.store.GetApp(context.Background(), "shop-staging")
	prod, _ := p.store.GetApp(context.Background(), "shop")

	decisions, err := p.engine.PreviewPromotionImages(context.Background(), prod, staging, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Service != "web" || decisions[0].Reuse ||
		decisions[0].Reason != ReasonArgsDiffer || !slices.Equal(decisions[0].DifferingArgs, []string{"ASSET_HOST"}) {
		t.Errorf("decisions = %+v", decisions)
	}
	// The preview's temporary env file is gone, and nothing was built.
	entries, _ := os.ReadDir(p.engine.appDir("shop"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".promotion-env") {
			t.Errorf("temporary env file left behind: %s", e.Name())
		}
	}
	if slices.Contains(p.dockerCalls(), "build") {
		t.Errorf("the preview built: %v", p.dockerCalls())
	}

	p.setEnv(prod, map[string]string{"ASSET_HOST": "https://staging.example.com"})
	decisions, err = p.engine.PreviewPromotionImages(context.Background(), prod, staging, source)
	if err != nil || len(decisions) != 1 || !decisions[0].Reuse {
		t.Errorf("decisions with the same args = %+v, %v", decisions, err)
	}

	if err := os.RemoveAll(p.engine.stateDir("shop-staging", source.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.engine.PreviewPromotionImages(context.Background(), prod, staging, source); !errors.Is(err, ErrSourceReleaseMissing) {
		t.Errorf("preview without the release: %v", err)
	}
}
