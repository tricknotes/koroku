//go:build integration

package integration

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry answers every request like a registry that wants a login
// and records the Authorization headers it was sent. It never accepts one,
// so a pull through it always fails as unauthorized.
type fakeRegistry struct {
	host string

	mu    sync.Mutex
	hits  int
	auths []string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRegistry{host: fmt.Sprintf("localhost:%d", ln.Addr().(*net.TCPAddr).Port)}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits++
		if a := req.Header.Get("Authorization"); a != "" {
			r.auths = append(r.auths, a)
		}
		r.mu.Unlock()
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("WWW-Authenticate", `Basic realm="koroku-test"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return r
}

func (r *fakeRegistry) seen() (hits int, auths []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits, append([]string(nil), r.auths...)
}

// TestRegistryCredentialReachesPullAndBuild deploys an app whose image (and
// one whose base image) lives in a registry that wants a login. The
// registry receives the app's own credential from the Docker config koroku
// writes for the app, both from `docker compose pull` and from BuildKit
// resolving a FROM line, and the deploy log explains the refusal without
// printing the token.
func TestRegistryCredentialReachesPullAndBuild(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	reg := newFakeRegistry(t)
	token := "it-token-" + e.suffix
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("it-user:"+token))

	cases := []struct {
		name  string
		files map[string]string
	}{
		{"pull", map[string]string{
			"compose.yaml": fmt.Sprintf("services:\n  web:\n    image: %s/koroku-it/private:1\n", reg.host),
		}},
		{"build", map[string]string{
			"compose.yaml": "services:\n  web:\n    build: .\n",
			"Dockerfile":   fmt.Sprintf("FROM %s/koroku-it/base:1\n", reg.host),
		}},
	}
	for _, tc := range cases {
		source := newRepo(t, "")
		for name, content := range tc.files {
			source.write(name, content)
		}
		sha := source.commit("Private image")
		slug := e.slug("reg" + tc.name)
		e.createApp(slug, source.url())
		var cred map[string]any
		if status := e.call(http.MethodPut, "/api/apps/"+slug+"/registry-credential", map[string]string{
			"registry": reg.host, "username": "it-user", "token": token,
		}, &cred); status != http.StatusOK || cred["configured"] != true {
			t.Fatalf("%s: put credential: %d %v", tc.name, status, cred)
		}

		d := e.deploy(slug, map[string]string{"sha": sha})
		log := e.logText(d.ID)
		hits, auths := reg.seen()
		if hits == 0 {
			t.Skipf("the Docker daemon cannot reach a registry on this host's loopback:\n%s", log)
		}
		if d.Status != "failed" {
			t.Fatalf("%s: deployment %s, want failed\n%s", tc.name, d.Status, log)
		}
		found := false
		for _, a := range auths {
			found = found || a == wantAuth
		}
		if !found {
			t.Errorf("%s: the registry never got the app's credential (got %d headers)\n%s", tc.name, len(auths), log)
		}
		if !strings.Contains(log, "The registry refused access") || !strings.Contains(d.Error, "container registry credential") {
			t.Errorf("%s: no registry hint: %s\n%s", tc.name, d.Error, log)
		}
		if strings.Contains(log, token) || strings.Contains(d.Error, token) {
			t.Errorf("%s: the token leaked:\n%s", tc.name, log)
		}

		dir := e.engine.DockerConfigDir(slug)
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: config dir: %v %v", tc.name, info, err)
		}
		if info, err := os.Stat(filepath.Join(dir, "config.json")); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: config.json: %v %v", tc.name, info, err)
		}
	}
}

// TestAppWithRegistryCredentialStillPullsPublicImages checks that pointing
// DOCKER_CONFIG at the app's directory leaves the rest of the Docker CLI
// working: the Compose and Buildx plugins are still found (they are
// installed system-wide, not under the config directory), and public
// images pull without a login.
func TestAppWithRegistryCredentialStillPullsPublicImages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "image-app")
	sha := source.commit("Image-only app")
	slug := e.slug("regpublic")
	app := e.createApp(slug, source.url())
	if status := e.call(http.MethodPut, "/api/apps/"+slug+"/registry-credential", map[string]string{
		"registry": "registry.example.com", "username": "it-user", "token": "it-token-" + e.suffix,
	}, nil); status != http.StatusOK {
		t.Fatalf("put credential: %d", status)
	}
	d := e.deploy(slug, map[string]string{"sha": sha})
	if d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if got := page(t, app.HostPort)[""]; !strings.HasPrefix(got, "image-app") {
		t.Errorf("page = %q", got)
	}
}
