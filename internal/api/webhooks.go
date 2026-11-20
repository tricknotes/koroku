package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/store"
)

// WebhookPath receives the GitHub App's webhook deliveries (ARCHITECTURE 5.2).
const WebhookPath = "/webhooks/github"

const (
	// maxWebhookBodyBytes bounds a delivery. Push payloads list at most a
	// few dozen commits; GitHub itself caps payloads at 25 MB.
	maxWebhookBodyBytes = 5 << 20
	// webhookEnqueueTimeout bounds queueing the deployments of one push.
	webhookEnqueueTimeout = 30 * time.Second
	// Deliveries are remembered this long, up to this many keys, to drop
	// duplicates (ARCHITECTURE 5.2).
	deliveryTTL   = 24 * time.Hour
	deliveryLimit = 4096
)

var (
	eventNamePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)
	deliveryPattern  = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
)

// PushDeployer queues deployments started by GitHub pushes (ARCHITECTURE 5.2).
type PushDeployer interface {
	EnqueuePush(ctx context.Context, app store.App, branch, sha, message, pusher string) (store.Deployment, error)
}

type webhookResultJSON struct {
	// Result is pong, ignored or deployed.
	Result string `json:"result"`
	// Reason says why a delivery was ignored.
	Reason string `json:"reason,omitempty"`
	// Apps lists what each matching app did with a push.
	Apps []pushResultJSON `json:"apps,omitempty"`
}

type pushResultJSON struct {
	App string `json:"app"`
	// Status is queued, duplicate, stopped or failed.
	Status       string `json:"status"`
	DeploymentID int64  `json:"deployment_id,omitempty"`
	// Error is the code of a failure: busy, disk_full or internal.
	Error string `json:"error,omitempty"`
}

// handleGitHubWebhook receives the GitHub App's webhook. It needs no
// session and no Origin (guard lets it through); the signature made with
// [github] webhook_secret is the authentication. Only pushes do anything:
// each app that deploys on push from the pushed branch of the repository
// gets a deployment of the pushed commit.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := s.cfg.GitHub.WebhookSecret
	if secret == "" || s.pushDeployer == nil {
		writeError(w, http.StatusForbidden, "webhook_disabled", "webhooks are not configured: set [github] webhook_secret")
		return
	}
	if !s.webhookLimiter.allow(s.clock.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many webhook deliveries")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "set the webhook content type to application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "the delivery is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "unreadable_body", "the delivery could not be read")
		return
	}
	if err := github.VerifySignature([]byte(secret), body, r.Header.Get(github.SignatureHeader)); err != nil {
		// Neither the signature nor the body is logged.
		s.log.Warn("webhook delivery with a bad signature refused")
		writeError(w, http.StatusUnauthorized, "bad_signature", "the signature does not match the webhook secret")
		return
	}
	event := r.Header.Get(github.EventHeader)
	delivery := r.Header.Get(github.DeliveryHeader)
	if !eventNamePattern.MatchString(event) || !deliveryPattern.MatchString(delivery) {
		writeError(w, http.StatusBadRequest, "bad_headers", "X-GitHub-Event and X-GitHub-Delivery are required")
		return
	}
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "pong"})
		return
	case "push":
	default:
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "event " + event + " is not used"})
		return
	}
	ev, err := github.ParsePushEvent(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_payload", "the push payload could not be read")
		return
	}
	branch, isBranch := ev.Branch()
	switch {
	case !isBranch:
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "not a branch"})
		return
	case ev.RemovesRef():
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "branch deleted"})
		return
	case !gitrepo.IsFullSHA(ev.After):
		writeError(w, http.StatusBadRequest, "bad_payload", "after is not a commit SHA")
		return
	}
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	targets := pushTargets(apps, ev, branch)
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "no app deploys this branch on push"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhookEnqueueTimeout)
	defer cancel()
	bodySum := sha256.Sum256(body)
	bodyKey := hex.EncodeToString(bodySum[:])
	pusher := cleanLogin(ev.Pusher)
	resp := webhookResultJSON{Result: "deployed"}
	failed := false
	for _, app := range targets {
		res := pushResultJSON{App: app.Slug}
		appKey := "/" + strconv.FormatInt(app.ID, 10)
		// GitHub redelivers with the same delivery ID and body. A copy of a
		// signed body sent with another delivery ID is caught by the body.
		keys := []string{"delivery:" + delivery + appKey, "body:" + bodyKey + appKey}
		switch {
		case app.DesiredState == store.DesiredStopped:
			res.Status = "stopped"
		case !s.deliveries.reserve(s.clock.Now(), keys...):
			res.Status = "duplicate"
		default:
			d, err := s.pushDeployer.EnqueuePush(ctx, app, branch, ev.After, ev.HeadMessage, pusher)
			if err != nil {
				// Let a redelivery from GitHub try again.
				s.deliveries.release(keys...)
				failed = true
				res.Status, res.Error = "failed", enqueueErrorCode(err)
				s.log.Error("queueing a push deployment failed", "app", app.Slug, "error", err)
				break
			}
			res.Status, res.DeploymentID = "queued", d.ID
			s.log.Info("push deployment queued", "app", app.Slug, "deployment", d.ID, "branch", branch, "pusher", pusher)
		}
		resp.Apps = append(resp.Apps, res)
	}
	status := http.StatusOK
	if failed {
		// GitHub shows the delivery as failed, and it can be redelivered.
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// pushTargets returns the apps that deploy a push to branch of the event's
// repository: auto-deploy on, fetched through the GitHub App, following
// branch. The repository matches by ID when both sides know it (it
// survives renames), by full name otherwise, ignoring case.
func pushTargets(apps []store.App, ev github.PushEvent, branch string) []store.App {
	var out []store.App
	for _, app := range apps {
		if !app.AutoDeploy || !app.UsesGitHubApp() || app.EffectiveAutoDeployBranch() != branch {
			continue
		}
		if app.RepoID != nil && ev.RepoID != 0 {
			if *app.RepoID != ev.RepoID {
				continue
			}
		} else if !strings.EqualFold(app.RepoFullName, ev.RepoFullName) {
			continue
		}
		out = append(out, app)
	}
	return out
}

// enqueueErrorCode names a failed enqueue for the webhook response.
func enqueueErrorCode(err error) string {
	var diskFull *deploy.DiskFullError
	switch {
	case errors.Is(err, deploy.ErrBusy):
		return "busy"
	case errors.As(err, &diskFull):
		return "disk_full"
	default:
		return "internal"
	}
}

// cleanLogin keeps a GitHub login printable and short before it reaches
// the database, the deploy log and the console.
func cleanLogin(login string) string {
	var b strings.Builder
	for _, r := range login {
		if b.Len() >= 64 {
			break
		}
		if r > ' ' && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// deliverySet remembers recent webhook deliveries, in bounded memory, to
// drop duplicates. It is lost on restart.
type deliverySet struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	order []deliveryEntry
	limit int
	ttl   time.Duration
}

type deliveryEntry struct {
	key string
	at  time.Time
}

func newDeliverySet(limit int, ttl time.Duration) *deliverySet {
	return &deliverySet{seen: map[string]time.Time{}, limit: limit, ttl: ttl}
}

// reserve records keys and reports true, or reports false without
// recording anything when one of them was seen within the TTL.
func (d *deliverySet) reserve(now time.Time, keys ...string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evict(now)
	for _, k := range keys {
		if _, ok := d.seen[k]; ok {
			return false
		}
	}
	for _, k := range keys {
		d.seen[k] = now
		d.order = append(d.order, deliveryEntry{key: k, at: now})
	}
	d.evict(now)
	return true
}

// release forgets keys, for a delivery that did not go through.
func (d *deliverySet) release(keys ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range keys {
		delete(d.seen, k)
	}
}

// evict drops expired entries and the oldest ones beyond the limit.
func (d *deliverySet) evict(now time.Time) {
	for len(d.order) > 0 {
		oldest := d.order[0]
		if now.Sub(oldest.at) < d.ttl && len(d.order) <= d.limit {
			return
		}
		if at, ok := d.seen[oldest.key]; ok && at.Equal(oldest.at) {
			delete(d.seen, oldest.key)
		}
		d.order = d.order[1:]
	}
}
