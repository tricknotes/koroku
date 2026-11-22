package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// DockerConfigDir is the Docker client config directory of an app. It holds
// only the app's own registry credential (ARCHITECTURE 6.1), so a pull for
// one app never uses another app's login, and none uses root's.
func (e *Engine) DockerConfigDir(slug string) string {
	return filepath.Join(e.appDir(slug), "docker")
}

// ForgetRegistryCredential removes the app's Docker config directory after
// its credential was deleted, so the token does not stay on disk until the
// next operation. registryConfig would remove it then as well.
func (e *Engine) ForgetRegistryCredential(app store.App) error {
	if err := os.RemoveAll(e.DockerConfigDir(app.Slug)); err != nil {
		return fmt.Errorf("remove the registry credential file: %w", err)
	}
	return nil
}

// dockerHubKey is the key the Docker CLI files Docker Hub credentials under.
const dockerHubKey = "https://index.docker.io/v1/"

// RegistryAuthKey is the key of a registry in the auths of a Docker config.
// Docker Hub has a legacy URL there; other registries use their host.
func RegistryAuthKey(registry string) string {
	switch registry {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return dockerHubKey
	}
	return registry
}

type dockerConfigFile struct {
	Auths map[string]dockerConfigAuth `json:"auths"`
}

type dockerConfigAuth struct {
	Auth string `json:"auth"`
}

// DockerConfigJSON renders a Docker client config with one credential.
func DockerConfigJSON(registry, username, token string) ([]byte, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	data, err := json.MarshalIndent(dockerConfigFile{
		Auths: map[string]dockerConfigAuth{RegistryAuthKey(registry): {Auth: auth}},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// registryConfig makes the app's Docker config directory match its saved
// registry credential and returns the directory to pass as DOCKER_CONFIG,
// or "" when the app has no credential (the call then uses the default
// config, as without this feature). The token and the encoded auth are
// added to masker. The file is rewritten from the database every time, so
// the database stays the only source of the credential.
func (e *Engine) registryConfig(ctx context.Context, app store.App, masker *secrets.Masker) (string, error) {
	dir := e.DockerConfigDir(app.Slug)
	cred, err := e.opts.Store.GetRegistryCredential(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		// A credential removed in the console must stop being used.
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("remove the registry credential file: %w", err)
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	plain, err := e.opts.Box.Open(cred.TokenEncrypted, secrets.RegistryTokenContext(app.ID))
	if err != nil {
		return "", fmt.Errorf("registry credential: %w", err)
	}
	token := string(plain)
	masker.Add(token, base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+token)))
	data, err := DockerConfigJSON(cred.Registry, cred.Username, token)
	if err != nil {
		return "", fmt.Errorf("render the registry credential: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the registry credential directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("restrict the registry credential directory: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		return "", fmt.Errorf("write the registry credential file: %w", err)
	}
	return dir, nil
}

// lineWatcher passes command output through and notes whether a line
// matches. It finds a registry refusing a pull (looksLikeRegistryAuthFailure)
// or a tag the registry does not have (looksLikeMissingImage) in the output of
// build, pull and up.
type lineWatcher struct {
	dst   io.Writer
	match func(line string) bool

	mu      sync.Mutex
	partial []byte
	seen    bool
}

// maxWatchedLine bounds the unterminated line kept between writes.
const maxWatchedLine = 4 << 10

func (w *lineWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		if w.match(string(w.partial[:i])) {
			w.seen = true
		}
		w.partial = w.partial[i+1:]
	}
	if len(w.partial) > maxWatchedLine {
		if w.match(string(w.partial)) {
			w.seen = true
		}
		w.partial = w.partial[:0]
	}
	w.mu.Unlock()
	return w.dst.Write(p)
}

// Seen reports whether a matching line was printed, including a last line
// without a newline.
func (w *lineWatcher) Seen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen || w.match(string(w.partial))
}

// outputHasRegistryAuthFailure reports whether any line of out reads like a
// registry refusing a pull.
func outputHasRegistryAuthFailure(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if looksLikeRegistryAuthFailure(line) {
			return true
		}
	}
	return false
}

// looksLikeMissingImage reports whether a line says the registry has no
// image for a tag. Docker Hub prints "manifest for <image> not found:
// manifest unknown: manifest unknown", GHCR a bare "manifest unknown".
func looksLikeMissingImage(line string) bool {
	return strings.Contains(strings.ToLower(line), "manifest unknown")
}

// ErrImageNotFound wraps a build, pull or up that failed because the
// registry has no image for a tag, typically a per-commit tag that CI has not
// pushed yet.
var ErrImageNotFound = errors.New("the registry has no image with that tag yet; if CI builds an image for each commit, deploy again once it is pushed")

// logMissingImageHint explains a missing tag in the deploy log.
func logMissingImageHint(log *deployLog) {
	log.Printf("The registry has no image with that tag (manifest unknown). If CI builds an image for each commit, the image of this commit may not be pushed yet: deploy again once the workflow has finished. For automatic deploys, set the workflow to wait for in the app's settings.")
}

func looksLikeRegistryAuthFailure(line string) bool {
	l := strings.ToLower(line)
	for _, s := range []string{"unauthorized", "no basic auth credentials", "authentication required", "pull access denied", "403 forbidden"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	// GHCR answers a missing permission with a bare "denied". A failing
	// RUN step prints "permission denied", which is not about the registry.
	if strings.Contains(l, "denied") && !strings.Contains(l, "permission denied") &&
		(strings.Contains(l, "denied:") || strings.HasSuffix(strings.TrimSpace(l), "denied")) {
		return true
	}
	return false
}

// RegistryAuthError is returned when a build, pull or start failed and the
// output says that the registry refused access.
type RegistryAuthError struct {
	// Configured is whether the app has a registry credential.
	Configured bool
	Err        error
}

func (e *RegistryAuthError) Error() string {
	if e.Configured {
		return "the registry refused access to an image; check the app's container registry credential (registry, username, token expiry and scope): " + e.Err.Error()
	}
	return "the registry refused access to an image; private images need a container registry credential in the app's settings: " + e.Err.Error()
}

func (e *RegistryAuthError) Unwrap() error { return e.Err }

// logRegistryHint explains a refused pull in the deploy log.
func logRegistryHint(log *deployLog, configured bool) {
	if configured {
		log.Printf("The registry refused access (unauthorized or denied). Check the container registry credential in the app's settings: the registry host, the username, and whether the token has expired or lost the read:packages scope.")
		return
	}
	log.Printf("The registry refused access (unauthorized or denied). This app has no container registry credential: set one in the app's settings to pull private images.")
}
