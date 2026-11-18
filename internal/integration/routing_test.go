//go:build integration

package integration

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/routing"
	"github.com/tricknotes/koroku/internal/store"
)

type captureLoader struct{ config []byte }

func (c *captureLoader) Load(_ context.Context, config []byte) error {
	c.config = config
	return nil
}

// TestGeneratedConfigIsAcceptedByCaddy builds the proxy configuration from a
// database with running, stopped and never-deployed apps plus custom domains
// and lets a real Caddy validate it.
func TestGeneratedConfigIsAcceptedByCaddy(t *testing.T) {
	t.Parallel()
	const image = "caddy:2"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
			t.Skipf("the %s image is not available: %v: %s", image, err, out)
		}
	}
	ctx := context.Background()
	now := time.Now()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	create := func(slug string) store.App {
		app, err := st.CreateApp(ctx, store.App{
			Slug: slug, RepoFullName: "octo/" + slug, DefaultBranch: "main",
			WebService: "web", WebPort: 3000, HealthcheckPath: "/",
		}, 20000, 20999, now)
		if err != nil {
			t.Fatal(err)
		}
		return app
	}
	deployed := func(app store.App) {
		d, _, err := st.EnqueueDeployment(ctx, app.ID, "main", strings.Repeat("a", 40), "", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.StartDeployment(ctx, d.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
	}
	blog, shop := create("blog"), create("shop-staging")
	create("never-deployed")
	deployed(blog)
	deployed(shop)
	if err := st.SetAppDesiredState(ctx, shop.ID, store.DesiredStopped, now); err != nil {
		t.Fatal(err)
	}
	for _, hostname := range []string{"example.org", "www.example.org"} {
		d, err := st.AddDomain(ctx, blog.ID, hostname, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RecordDomainCheck(ctx, d.ID, true, "", now); err != nil {
			t.Fatal(err)
		}
		if hostname == "www.example.org" {
			if err := st.SetDomainPrimary(ctx, blog.ID, d.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}

	loader := &captureLoader{}
	router := &routing.Router{Store: st, Loader: loader, Settings: routing.Settings{
		BaseDomain:      "dev.example.com",
		ConsoleHost:     "console.dev.example.com",
		ConsoleUpstream: "127.0.0.1:8080",
		AdminListen:     "127.0.0.1:2019",
		ACMECA:          "https://acme-staging-v02.api.letsencrypt.org/directory",
		ACMEEmail:       "admin@example.com",
		AccessLogDir:    "/var/log/caddy/apps",
	}}
	if err := router.ForceSync(ctx); err != nil {
		t.Fatalf("ForceSync: %v", err)
	}

	// The configuration is piped in rather than bind-mounted: the Docker
	// daemon does not necessarily see this machine's temp directory.
	cmd := exec.Command("docker", "run", "--rm", "-i", "--network", "none", image,
		"sh", "-c", "cat > /tmp/caddy.json && caddy validate --config /tmp/caddy.json")
	cmd.Stdin = bytes.NewReader(loader.config)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("caddy rejected the generated configuration: %v\n%s\n--- config ---\n%s", err, out, loader.config)
	}
	if !strings.Contains(string(out), "Valid configuration") {
		t.Errorf("unexpected validate output:\n%s", out)
	}
}
