//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// assetHostCompose passes ASSET_HOST to the build, like a Rails app that
// bakes asset_host into precompiled assets (ARCHITECTURE 9.7).
const assetHostCompose = `services:
  web:
    build:
      context: .
      args:
        ASSET_HOST: "${ASSET_HOST:-}"
    stop_signal: SIGKILL
    ports:
      - "8080:8080"
`

const assetHostDockerfile = `FROM busybox:1.37
ARG ASSET_HOST=
COPY VERSION entrypoint.sh /app/
RUN echo "$ASSET_HOST" > /app/ASSET_HOST
STOPSIGNAL SIGKILL
CMD ["sh", "/app/entrypoint.sh"]
`

const assetHostEntrypoint = `#!/bin/sh
set -eu
mkdir -p /www
{
  echo "version=$(cat /app/VERSION)"
  echo "asset_host=$(cat /app/ASSET_HOST)"
} > /www/index.html
exec httpd -f -p 8080 -h /www
`

func imageID(t *testing.T, image string) string {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil {
		t.Fatalf("docker image inspect %s: %v", image, err)
	}
	return strings.TrimSpace(string(out))
}

// TestPromotionReusesStagingImage promotes a staging release to production
// on a real daemon: with the same build args the staging image is tagged
// for production and nothing is built; with a production-specific build
// arg the image is rebuilt with production's value.
func TestPromotionReusesStagingImage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "build-app")
	source.write("compose.yaml", assetHostCompose)
	source.write("Dockerfile", assetHostDockerfile)
	source.write("entrypoint.sh", assetHostEntrypoint)
	v1 := source.commit("Version 1")
	source.write("VERSION", "v2\n")
	v2 := source.commit("Version 2")

	stagingSlug, prodSlug := e.slug("stg"), e.slug("prod")
	staging := e.createApp(stagingSlug, source.url())
	prod := e.createApp(prodSlug, source.url())
	var images []string
	t.Cleanup(func() {
		if len(images) > 0 {
			_ = exec.Command("docker", append([]string{"image", "rm"}, images...)...).Run()
		}
	})
	e.setEnv(stagingSlug, map[string]string{"ASSET_HOST": "https://assets.example.com", "STAGE": "staging"})
	e.setEnv(prodSlug, map[string]string{"ASSET_HOST": "https://assets.example.com", "STAGE": "production"})

	// Production runs v1; staging has moved on to v2.
	if d := e.deploy(prodSlug, map[string]string{"sha": v1}); d.Status != "succeeded" {
		t.Fatalf("production v1 %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	stagingRelease := e.deploy(stagingSlug, map[string]string{"sha": v2})
	if stagingRelease.Status != "succeeded" {
		t.Fatalf("staging v2 %s: %s\n%s", stagingRelease.Status, stagingRelease.Error, e.logText(stagingRelease.ID))
	}
	stagingImage := fmt.Sprintf("koroku-%s-web:r%d", stagingSlug, stagingRelease.ID)
	images = append(images, stagingImage)
	if got := page(t, staging.HostPort); got["version"] != "v2" {
		t.Fatalf("staging page = %v", got)
	}

	if status := e.call(http.MethodPatch, "/api/apps/"+prodSlug,
		map[string]string{"environment": "production", "promote_from": stagingSlug}, nil); status != http.StatusOK {
		t.Fatalf("pairing: status %d", status)
	}
	var preview struct {
		Status           string `json:"status"`
		SourceDeployment struct {
			ID int64 `json:"id"`
		} `json:"source_deployment"`
		Images struct {
			Status   string `json:"status"`
			Services []struct {
				Service       string   `json:"service"`
				Reuse         bool     `json:"reuse"`
				DifferingArgs []string `json:"differing_args"`
			} `json:"services"`
		} `json:"images"`
	}
	e.call(http.MethodGet, "/api/apps/"+prodSlug+"/promotion", nil, &preview)
	if preview.Status != "ready" || preview.SourceDeployment.ID != stagingRelease.ID ||
		preview.Images.Status != "checked" || len(preview.Images.Services) != 1 || !preview.Images.Services[0].Reuse {
		t.Fatalf("preview = %+v", preview)
	}

	// Promotion with the same build args: the staging image is reused.
	promoted := e.deploy(prodSlug, map[string]int64{"promote_from_deployment": stagingRelease.ID})
	log := e.logText(promoted.ID)
	if promoted.Status != "succeeded" {
		t.Fatalf("promotion %s: %s\n%s", promoted.Status, promoted.Error, log)
	}
	prodImage := fmt.Sprintf("koroku-%s-web:r%d", prodSlug, promoted.ID)
	images = append(images, prodImage)
	if !strings.Contains(log, "web: reusing "+stagingImage) || strings.Contains(log, "Building images") {
		t.Errorf("the promotion did not reuse the staging image:\n%s", log)
	}
	if got := page(t, prod.HostPort); got["version"] != "v2" || got["asset_host"] != "https://assets.example.com" {
		t.Errorf("production page after promotion = %v", got)
	}
	if a, b := imageID(t, stagingImage), imageID(t, prodImage); a != b {
		t.Errorf("production runs image %s, staging %s", b, a)
	}

	// A production-specific build arg makes the promotion build.
	e.setEnv(prodSlug, map[string]string{"ASSET_HOST": "https://cdn.example.com", "STAGE": "production"})
	e.call(http.MethodGet, "/api/apps/"+prodSlug+"/promotion", nil, &preview)
	if len(preview.Images.Services) != 1 || preview.Images.Services[0].Reuse ||
		strings.Join(preview.Images.Services[0].DifferingArgs, ",") != "ASSET_HOST" {
		t.Errorf("preview with differing args = %+v", preview.Images)
	}
	rebuilt := e.deploy(prodSlug, map[string]int64{"promote_from_deployment": stagingRelease.ID})
	log = e.logText(rebuilt.ID)
	if rebuilt.Status != "succeeded" {
		t.Fatalf("second promotion %s: %s\n%s", rebuilt.Status, rebuilt.Error, log)
	}
	rebuiltImage := fmt.Sprintf("koroku-%s-web:r%d", prodSlug, rebuilt.ID)
	images = append(images, rebuiltImage)
	if !strings.Contains(log, "web: building (build args differ: ASSET_HOST)") || !strings.Contains(log, "Building images") {
		t.Errorf("the promotion did not rebuild:\n%s", log)
	}
	if strings.Contains(log, "cdn.example.com") || strings.Contains(log, "assets.example.com") {
		t.Errorf("the deploy log shows a build arg value:\n%s", log)
	}
	if got := page(t, prod.HostPort); got["version"] != "v2" || got["asset_host"] != "https://cdn.example.com" {
		t.Errorf("production page after the rebuild = %v", got)
	}
	if imageID(t, stagingImage) == imageID(t, rebuiltImage) {
		t.Errorf("the rebuilt production image is the staging image")
	}
}
