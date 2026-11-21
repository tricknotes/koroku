package deploy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

const registryToken = "ghp_registryTokenForTests123"

func (h *harness) setRegistryCredential(app store.App, registry, username, token string) {
	h.t.Helper()
	sealed, err := h.box.Seal([]byte(token), secrets.RegistryTokenContext(app.ID))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.PutRegistryCredential(context.Background(), store.RegistryCredential{
		AppID: app.ID, Registry: registry, Username: username, TokenEncrypted: sealed, UpdatedAt: h.clock.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
}

// dockerConfigOf returns the DOCKER_CONFIG a command was given, or "".
func dockerConfigOf(cmd runner.Cmd) string {
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "DOCKER_CONFIG="); ok {
			return v
		}
	}
	return ""
}

func TestDeployUsesTheAppsRegistryCredential(t *testing.T) {
	h := newHarness(t, nil)
	blog := h.createApp("blog")
	h.createApp("shop")
	h.setRegistryCredential(blog, "ghcr.io", "octo-bot", registryToken)
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	dir := h.engine.DockerConfigDir("blog")
	for _, sub := range []string{"config", "build", "pull", "up"} {
		cmd := h.lastCall(sub)
		if got := dockerConfigOf(cmd); got != dir {
			t.Errorf("%s: DOCKER_CONFIG = %q, want %q", sub, got, dir)
		}
		if strings.Contains(strings.Join(cmd.Args, " "), registryToken) {
			t.Errorf("%s: the token is in the arguments: %v", sub, cmd.Args)
		}
	}

	// The directory holds only this app's registry, readable by root only.
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, %v", info.Mode().Perm(), err)
	}
	path := filepath.Join(dir, "config.json")
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode: %v, %v", info, err)
	}
	data, _ := os.ReadFile(path)
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config.json: %v\n%s", err, data)
	}
	if len(cfg.Auths) != 1 {
		t.Fatalf("auths = %v", cfg.Auths)
	}
	decoded, _ := base64.StdEncoding.DecodeString(cfg.Auths["ghcr.io"].Auth)
	if string(decoded) != "octo-bot:"+registryToken {
		t.Errorf("auth = %q", decoded)
	}
	if log := h.log("blog", d.ID); strings.Contains(log, registryToken) || !strings.Contains(log, "Container registry credential: set for this app") {
		t.Errorf("log:\n%s", log)
	}

	// An app without a credential runs exactly as before: no DOCKER_CONFIG.
	h.runner.Reset()
	if d := h.deploy("shop", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("shop = %+v", d)
	}
	for _, c := range h.runner.Calls() {
		if c.Name == "docker" && len(c.Env) != 0 {
			t.Errorf("shop: %v got env %v", c.Args, c.Env)
		}
	}
	if _, err := os.Stat(h.engine.DockerConfigDir("shop")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("shop has a docker config dir: %v", err)
	}

	// Removing the credential removes the file at the next deployment.
	if err := h.store.DeleteRegistryCredential(context.Background(), blog.ID); err != nil {
		t.Fatal(err)
	}
	h.runner.Reset()
	if d := h.deploy("blog", shaB); d.Status != store.StatusSucceeded {
		t.Fatalf("blog again = %+v", d)
	}
	if got := dockerConfigOf(h.lastCall("pull")); got != "" {
		t.Errorf("DOCKER_CONFIG after removal = %q", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the config dir is still there: %v", err)
	}
}

func TestRefusedPullExplainsTheRegistryCredential(t *testing.T) {
	h := newHarness(t, nil)
	blog := h.createApp("blog")
	h.setRegistryCredential(blog, "ghcr.io", "octo-bot", registryToken)
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "pull" {
			_, _ = io.WriteString(cmd.Stderr, " db Error Head \"https://ghcr.io/v2/octo/db/manifests/1\": denied\n")
			_, _ = io.WriteString(cmd.Stderr, "auth "+base64.StdEncoding.EncodeToString([]byte("octo-bot:"+registryToken))+" "+registryToken+"\n")
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "container registry credential") {
		t.Fatalf("deployment = %+v", d)
	}
	log := h.log("blog", d.ID)
	if !strings.Contains(log, "The registry refused access") || !strings.Contains(log, "whether the token has expired") {
		t.Errorf("log lacks the hint:\n%s", log)
	}
	if strings.Contains(log, registryToken) || strings.Contains(log, base64.StdEncoding.EncodeToString([]byte("octo-bot:"+registryToken))) {
		t.Errorf("the log leaks the token:\n%s", log)
	}
	// The pull failed before anything was started.
	if slices.Contains(h.dockerCalls(), "up") {
		t.Errorf("calls = %v", h.dockerCalls())
	}
}

func TestBuildFailureWithoutRegistryWordsHasNoHint(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "build" {
			_, _ = io.WriteString(cmd.Stderr, "sh: /app/bin/setup: Permission denied\n")
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.start()
	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", d)
	}
	if log := h.log("blog", d.ID); strings.Contains(log, "The registry refused access") {
		t.Errorf("a failing RUN step got the registry hint:\n%s", log)
	}
}

func TestStartUsesTheAppsRegistryCredential(t *testing.T) {
	h := newHarness(t, nil)
	blog := h.createApp("blog")
	h.start()
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	h.setRegistryCredential(blog, "docker.io", "octo", registryToken)
	app, _ := h.store.GetApp(context.Background(), "blog")
	if err := h.engine.Stop(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	app, _ = h.store.GetApp(context.Background(), "blog")
	h.runner.Reset()
	if err := h.engine.Start(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if got := dockerConfigOf(h.lastCall("up")); got != h.engine.DockerConfigDir("blog") {
		t.Errorf("start: DOCKER_CONFIG = %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(h.engine.DockerConfigDir("blog"), "config.json"))
	if !strings.Contains(string(data), `"https://index.docker.io/v1/"`) {
		t.Errorf("Docker Hub key missing:\n%s", data)
	}

	// A refused pull while starting is reported as such.
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "up" {
			_, _ = io.WriteString(cmd.Stderr, "Error response from daemon: unauthorized: authentication required\n")
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	var regErr *RegistryAuthError
	if err := h.engine.Start(context.Background(), app); !errors.As(err, &regErr) || !regErr.Configured {
		t.Errorf("start with a refused pull: %v", err)
	}

	// Forgetting removes the file.
	if err := h.engine.ForgetRegistryCredential(app); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.engine.DockerConfigDir("blog")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dir after forget: %v", err)
	}
}

func TestLooksLikeRegistryAuthFailure(t *testing.T) {
	for line, want := range map[string]bool{
		`Error response from daemon: Head "https://ghcr.io/v2/octo/app/manifests/sha-1": unauthorized`:         true,
		`Error response from daemon: Head "http://localhost:5000/v2/x/manifests/1": no basic auth credentials`: true,
		`failed to resolve source metadata for ghcr.io/octo/base:1: unexpected status: 401 Unauthorized`:       true,
		`Error response from daemon: pull access denied for octo/app, repository does not exist`:               true,
		` db Error denied`: true,
		`error from registry: denied: permission_denied`: true,
		`#8 0.2 sh: ./setup: Permission denied`:          false,
		`Error: container blog-web-1 exited (1)`:         false,
		`Pulling fs layer`:                               false,
	} {
		if got := looksLikeRegistryAuthFailure(line); got != want {
			t.Errorf("%q: got %v, want %v", line, got, want)
		}
	}
}

func TestRegistryAuthWatcherAcrossWrites(t *testing.T) {
	var sink strings.Builder
	w := &registryAuthWatcher{dst: &sink}
	_, _ = io.WriteString(w, "Pulling\nError: unauth")
	if w.seen {
		t.Fatal("seen too early")
	}
	_, _ = io.WriteString(w, "orized\n")
	if !w.Seen() || sink.String() != "Pulling\nError: unauthorized\n" {
		t.Errorf("seen = %v, out = %q", w.Seen(), sink.String())
	}
}
